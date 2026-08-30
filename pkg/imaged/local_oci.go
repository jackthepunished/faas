package imaged

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/pkg/oci"
	"github.com/onebox-faas/faas/pkg/rootfs"
	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/state"
)

// localOCIIndex and localOCIManifest are the small parts of an OCI layout
// archive produced by builderd's BuildKit exporter. The registry path already
// has an equivalent streaming abstraction; source builds need the same shape
// without pretending a host-local tarball is a registry reference.
type localOCIIndex struct {
	Manifests []oci.Descriptor `json:"manifests"`
}

// Bound local layer extraction so a malformed archive cannot consume an
// unbounded amount of disk while imaged copies a builderd artifact.
const maxLocalOCILayerBytes = 16 << 30

// loadLocalOCIArchive opens a builderd-produced OCI layout tarball, extracts
// its gzip-compressed layer blobs to temporary files, and parses the image
// config. The returned readers are ordered bottom-to-top and remain valid
// until cleanup is called.
func loadLocalOCIArchive(archivePath string) (oci.Config, []io.ReadCloser, func(), error) {
	tmpDir, err := os.MkdirTemp("", "faas-local-oci-")
	if err != nil {
		return oci.Config{}, nil, func() {}, fmt.Errorf("create local OCI tempdir: %w", err)
	}
	var layerFiles []*os.File
	cleanup := func() {
		for _, f := range layerFiles {
			_ = f.Close()
		}
		_ = os.RemoveAll(tmpDir)
	}
	fail := func(err error) (oci.Config, []io.ReadCloser, func(), error) {
		cleanup()
		return oci.Config{}, nil, func() {}, err
	}

	indexBytes, err := readLocalOCIEntry(archivePath, "index.json", 1<<20)
	if err != nil {
		return fail(fmt.Errorf("read OCI index: %w", err))
	}
	var index localOCIIndex
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return fail(fmt.Errorf("decode OCI index: %w", err))
	}
	if len(index.Manifests) != 1 {
		return fail(fmt.Errorf("OCI index has %d manifests, want exactly one", len(index.Manifests)))
	}
	manifestName, err := localOCIBlobName(index.Manifests[0].Digest)
	if err != nil {
		return fail(fmt.Errorf("OCI manifest digest: %w", err))
	}
	manifestBytes, err := readLocalOCIEntry(archivePath, manifestName, 8<<20)
	if err != nil {
		return fail(fmt.Errorf("read OCI manifest: %w", err))
	}
	var manifest oci.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fail(fmt.Errorf("decode OCI manifest: %w", err))
	}
	configName, err := localOCIBlobName(manifest.Config.Digest)
	if err != nil {
		return fail(fmt.Errorf("OCI config digest: %w", err))
	}
	layerNames := make(map[string]int, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		name, err := localOCIBlobName(layer.Digest)
		if err != nil {
			return fail(fmt.Errorf("OCI layer %d digest: %w", i, err))
		}
		layerNames[name] = i
		f, err := os.Create(filepath.Join(tmpDir, fmt.Sprintf("layer-%03d.tar.gz", i)))
		if err != nil {
			return fail(fmt.Errorf("create OCI layer %d: %w", i, err))
		}
		layerFiles = append(layerFiles, f)
	}

	configBytes, err := extractLocalOCIBlobs(archivePath, configName, layerNames, layerFiles)
	if err != nil {
		return fail(err)
	}
	config, err := oci.ParseConfig(bytes.NewReader(configBytes))
	if err != nil {
		return fail(fmt.Errorf("parse OCI config: %w", err))
	}
	for i, f := range layerFiles {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fail(fmt.Errorf("rewind OCI layer %d: %w", i, err))
		}
	}
	readers := make([]io.ReadCloser, len(layerFiles))
	for i, f := range layerFiles {
		readers[i] = f
	}
	return config, readers, cleanup, nil
}

func readLocalOCIEntry(archivePath, name string, maxBytes int64) ([]byte, error) {
	// archivePath is an internal builderd output selected from the deployment
	// row, not a customer-supplied path crossing the host boundary.
	//nolint:forbidigo // the local OCI reader must open this vetted artifact.
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name != name {
			continue
		}
		limited := io.LimitReader(tr, maxBytes+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("entry %q exceeds %d bytes", name, maxBytes)
		}
		return data, nil
	}
	return nil, fmt.Errorf("entry %q not found", name)
}

func extractLocalOCIBlobs(archivePath, configName string, layerNames map[string]int, layerFiles []*os.File) ([]byte, error) {
	// archivePath is an internal builderd output selected from the deployment
	// row, not a customer-supplied path crossing the host boundary.
	//nolint:forbidigo // the local OCI reader must open this vetted artifact.
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	var configBytes []byte
	seenLayers := make([]bool, len(layerFiles))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == configName {
			configBytes, err = io.ReadAll(io.LimitReader(tr, 16<<20))
		} else if i, ok := layerNames[hdr.Name]; ok {
			if seenLayers[i] {
				return nil, fmt.Errorf("duplicate OCI layer entry %q", hdr.Name)
			}
			var copied int64
			copied, err = io.Copy(layerFiles[i], io.LimitReader(tr, maxLocalOCILayerBytes+1))
			if err == nil && copied > maxLocalOCILayerBytes {
				err = fmt.Errorf("OCI layer %q exceeds %d bytes", hdr.Name, maxLocalOCILayerBytes)
			}
			seenLayers[i] = true
		}
		if err != nil {
			return nil, err
		}
	}
	if len(configBytes) == 0 {
		return nil, fmt.Errorf("OCI config entry %q not found", configName)
	}
	for i, seen := range seenLayers {
		if !seen {
			return nil, fmt.Errorf("OCI layer %d was not found", i)
		}
		if err := layerFiles[i].Sync(); err != nil {
			return nil, fmt.Errorf("sync OCI layer %d: %w", i, err)
		}
	}
	return configBytes, nil
}

func localOCIBlobName(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("unsupported digest %q", digest)
	}
	hexPart := strings.TrimPrefix(digest, prefix)
	if len(hexPart) != 64 {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("invalid digest %q: %w", digest, err)
	}
	return "blobs/sha256/" + hexPart, nil
}

// buildLocalOCIAppLayer converts a source-build OCI tarball into the app's
// bootable drive1. Source-built apps do not have a registry reference or a
// platform-runtime prefix to feed through the registry two-drive path, so all
// layers from this trusted local artifact are applied here.
func (h *Handler) buildLocalOCIAppLayer(ctx context.Context, app state.App, dep state.Deployment, acct state.Account) error {
	config, layers, cleanup, err := loadLocalOCIArchive(dep.RootfsPath)
	if err != nil {
		return fmt.Errorf("imaged: load built OCI image: %w", err)
	}
	defer cleanup()

	manifest, err := oci.ManifestFromConfig(config)
	if err != nil {
		return fmt.Errorf("imaged: built OCI manifest: %w", err)
	}
	// F8 fixup: shared default-seeding helper — same rule the
	// registry pull path applies in manifestFromImageConfig.
	applyContainerDefaults(&manifest)
	if dep.Handler != "" {
		manifest.Entrypoint = []string{dep.Handler}
	}
	manifest, err = applyOverrides(manifest, dep)
	if err != nil {
		return fmt.Errorf("imaged: built OCI manifest overrides: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("imaged: built OCI manifest invalid: %w", err)
	}

	be, err := h.storageFor()
	if err != nil {
		return fmt.Errorf("imaged: local OCI storageFor: %w", err)
	}
	appsKey := sched.AppLayerKey(app.Slug, dep.ID)
	result, err := h.builder.Build(ctx, rootfs.BuildInput{
		Layers:         layersAsReaders(layers),
		Manifest:       manifest,
		GuestInitPath:  h.guestInitPath,
		Plan:           acct.Plan,
		Storage:        be,
		StorageKey:     appsKey,
		SBOMRun:        h.syftRun,
		SBOMStorageKey: h.sbomStorageKeyForDeployment(ctx, dep.ID),
	})
	if err != nil {
		return fmt.Errorf("imaged: build local OCI app layer: %w", err)
	}
	h.updateBuildProvenanceSBOM(ctx, dep.ID, result.SBOMKey)
	if err := h.store.SetDeploymentRootfs(ctx, dep.ID, h.appsRootPath(app.Slug, dep.ID), appsKey, result.ContentBytes); err != nil {
		return fmt.Errorf("imaged: stamp local OCI rootfs: %w", err)
	}
	if err := h.replicateLayer(ctx, appsKey); err != nil {
		return err
	}
	h.log.Info("imaged: build local OCI app layer", "app", app.Slug, "bytes", result.ContentBytes)
	return nil
}
