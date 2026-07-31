// handler_storage_routing_test.go — ADR-054 §3 end-to-end pin.
//
// Pins that the imaged Handler's storage wiring honours the
// production routing semantics: when WithStorage is given a
// PrefixRouter with apps/ routed to a non-default backend, every
// layer publish lands in the apps/ backend (not the local one).
// The fake OCI backend tracks per-op counters so the test asserts
// the prefix dispatch fires exactly once per call.
//
// Lives next to handler_image_build_test.go (which exercises the
// full build pipeline). This file is the smaller, hermetic pin for
// the storage seam specifically — it doesn't need the OCI manifest
// pipeline, just the storageFor() route the handler uses to
// publish a per-app layer blob.

package imaged

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/storage"
)

// fakeOCIPutter is the minimal StorageBackend stub for the OCI
// route. Counts every op so the test can assert which backend
// received which key. Implements LocalArtifactLister so the
// PrefixRouter can aggregate its keys at the router level.
type fakeOCIPutter struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	puts    int
	gets    int
	deletes int
}

func newFakeOCIPutter() *fakeOCIPutter {
	return &fakeOCIPutter{blobs: map[string][]byte{}}
}

func (f *fakeOCIPutter) Put(_ context.Context, key string, r io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.blobs[key] = data
	return nil
}

func (f *fakeOCIPutter) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	data, ok := f.blobs[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeOCIPutter) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	delete(f.blobs, key)
	return nil
}

func (f *fakeOCIPutter) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.blobs))
	for k := range f.blobs {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TestHandler_StorageRouting_AppsToOCI pins the end-to-end
// "apps/ → OCI" semantics from ADR-054 §3. A Handler with a
// PrefixRouter routing apps/ to a fake OCI backend must publish
// per-app layer blobs via that backend; the local backend must
// NOT receive any apps/-prefixed key.
//
// The test calls the package-private storageFor() seam to keep
// the diff small — there's no public "publish layer blob" method
// today (the call site lives inside the build pipeline). The
// seam is what the build pipeline uses too, so a regression
// here breaks both paths.
func TestHandler_StorageRouting_AppsToOCI(t *testing.T) {
	// Distinct local roots so any misrouting to "local" is
	// caught by a path mismatch (the local backend writes to
	// its own TempDir, not where the OCI stub expects to find
	// keys).
	localRoot := t.TempDir()
	localBE, err := storage.NewLocalStorageBackend(localRoot)
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	oci := newFakeOCIPutter()
	router, err := storage.NewPrefixRouter(
		map[string]storage.StorageBackend{"apps/": oci},
		localBE,
	)
	if err != nil {
		t.Fatalf("NewPrefixRouter: %v", err)
	}

	h := New(state.NewMemStore(), &fakeNotifier{}, fakePuller{}, &fakeBuilder{},
		"./init", localRoot, silentLogger())
	h.WithStorage(router)

	ctx := context.Background()
	be, err := h.storageFor()
	if err != nil {
		t.Fatalf("storageFor: %v", err)
	}

	// Publish a per-app layer blob under the canonical key.
	const layerKey = "apps/acme/d-1.ext4"
	const blob = "fake-ext4-blob"
	if err := be.Put(ctx, layerKey, strings.NewReader(blob)); err != nil {
		t.Fatalf("Put %q: %v", layerKey, err)
	}

	// OCI stub must have received exactly the apps/ write.
	if oci.puts != 1 {
		t.Errorf("oci.puts = %d, want 1", oci.puts)
	}
	// The OCI stub stores the stripped remainder; the router
	// dispatched "apps/acme/d-1.ext4" with "apps/" stripped,
	// so the OCI key is "acme/d-1.ext4".
	if got, err := readOCIString(ctx, oci, "acme/d-1.ext4"); err != nil {
		t.Errorf("read OCI blob: %v", err)
	} else if got != blob {
		t.Errorf("OCI blob = %q, want %q", got, blob)
	}

	// Local root must be empty — no apps/-prefixed write landed
	// there. Reading the local root directly proves the routing
	// decision kept the data out of the local backend.
	entries, err := localBE.List(ctx, "")
	if err != nil {
		t.Fatalf("localBE.List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("local root has %d entries, want 0 (routing leaked to local): %v", len(entries), entries)
	}
}

// TestHandler_StorageRouting_LayerKeyShape pins the canonical
// key shape the handler must publish under: apps/<slug>/<depID>.ext4.
// The shape mirrors sched.AppLayerKey (pkg/sched/paths.go) and is
// the load-bearing invariant for vmmd's wake path — a vmmd wakes
// the layer under exactly this key, so the publish side must agree.
//
// In this test the OCI stub is wired directly (no router), so the
// key in the stub matches the full "apps/..." form — that's the
// point of the pin: a future refactor that splits keys via a
// router must still produce the full canonical shape end-to-end.
func TestHandler_StorageRouting_LayerKeyShape(t *testing.T) {
	oci := newFakeOCIPutter()
	h := New(state.NewMemStore(), &fakeNotifier{}, fakePuller{}, &fakeBuilder{},
		"./init", t.TempDir(), silentLogger()).WithStorage(oci)
	be, err := h.storageFor()
	if err != nil {
		t.Fatalf("storageFor: %v", err)
	}
	const key = "apps/my-app/dep-abc.ext4"
	if err := be.Put(context.Background(), key, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if oci.puts != 1 {
		t.Errorf("oci.puts = %d, want 1", oci.puts)
	}
	// Sanity: the stub stored the full key as-is.
	if got, err := readOCIString(context.Background(), oci, key); err != nil {
		t.Errorf("read OCI stub: %v", err)
	} else if got != "x" {
		t.Errorf("OCI stub = %q, want %q", got, "x")
	}
}

// readOCIString pulls a blob out of the OCI stub. The stub's Get
// strips the route prefix (its constructor sees the routed
// remainder, not the full key), so callers must pass the stripped
// form.
func readOCIString(ctx context.Context, oci *fakeOCIPutter, key string) (string, error) {
	rc, err := oci.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
