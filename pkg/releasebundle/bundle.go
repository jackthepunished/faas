package releasebundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ManifestName = "manifest.json"

type Manifest struct {
	FormatVersion int       `json:"format_version"`
	ReleaseID     string    `json:"release_id"`
	CommitSHA     string    `json:"commit_sha"`
	Target        string    `json:"target"`
	CreatedAt     time.Time `json:"created_at"`
	Files         []File    `json:"files"`
}

type File struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func Build(root, releaseID, commitSHA, target string, now time.Time) (Manifest, error) {
	if root == "" {
		return Manifest{}, errors.New("releasebundle: empty root")
	}
	if releaseID == "" {
		return Manifest{}, errors.New("releasebundle: empty release id")
	}
	if commitSHA == "" {
		return Manifest{}, errors.New("releasebundle: empty commit sha")
	}
	if target == "" {
		return Manifest{}, errors.New("releasebundle: empty target")
	}

	files := make([]File, 0)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		if err := validatePath(rel); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		files = append(files, File{
			Path:   rel,
			Mode:   uint32(info.Mode().Perm()),
			Size:   info.Size(),
			SHA256: hash,
		})
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("releasebundle: walk: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Manifest{
		FormatVersion: 1,
		ReleaseID:     releaseID,
		CommitSHA:     commitSHA,
		Target:        target,
		CreatedAt:     now.UTC(),
		Files:         files,
	}, nil
}

func Write(root string, manifest Manifest) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("releasebundle: marshal manifest: %w", err)
	}
	body = append(body, '\n')
	path := filepath.Join(root, ManifestName)
	tmp, err := os.CreateTemp(root, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("releasebundle: create manifest temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("releasebundle: chmod manifest temp: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("releasebundle: write manifest temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("releasebundle: sync manifest temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("releasebundle: close manifest temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("releasebundle: publish manifest: %w", err)
	}
	return nil
}

func Read(root string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("releasebundle: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("releasebundle: decode manifest: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Verify(root string, manifest Manifest) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		seen[file.Path] = struct{}{}
		path := filepath.Join(root, filepath.FromSlash(file.Path))
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("releasebundle: stat %s: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("releasebundle: %s is not a regular file", file.Path)
		}
		if info.Size() != file.Size {
			return fmt.Errorf("releasebundle: %s size %d, want %d", file.Path, info.Size(), file.Size)
		}
		if info.Mode().Perm() != os.FileMode(file.Mode) {
			return fmt.Errorf("releasebundle: %s mode %o, want %o", file.Path, info.Mode().Perm(), file.Mode)
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		if hash != file.SHA256 {
			return fmt.Errorf("releasebundle: %s sha256 %s, want %s", file.Path, hash, file.SHA256)
		}
	}
	var unexpected []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != ManifestName {
			if _, ok := seen[rel]; !ok {
				unexpected = append(unexpected, rel)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("releasebundle: verify tree: %w", err)
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		return fmt.Errorf("releasebundle: unexpected files: %s", strings.Join(unexpected, ", "))
	}
	return nil
}

func ValidateManifest(manifest Manifest) error {
	if manifest.FormatVersion != 1 {
		return fmt.Errorf("releasebundle: unsupported format version %d", manifest.FormatVersion)
	}
	if manifest.ReleaseID == "" || manifest.CommitSHA == "" || manifest.Target == "" {
		return errors.New("releasebundle: manifest identity is incomplete")
	}
	if manifest.CreatedAt.IsZero() {
		return errors.New("releasebundle: manifest created_at is zero")
	}
	previous := ""
	for _, file := range manifest.Files {
		if err := validatePath(file.Path); err != nil {
			return err
		}
		if file.Path <= previous {
			return fmt.Errorf("releasebundle: files are not strictly sorted at %s", file.Path)
		}
		previous = file.Path
		if file.Mode&0o170000 != 0 {
			return fmt.Errorf("releasebundle: invalid mode for %s", file.Path)
		}
		if file.Size < 0 {
			return fmt.Errorf("releasebundle: negative size for %s", file.Path)
		}
		if len(file.SHA256) != sha256.Size*2 {
			return fmt.Errorf("releasebundle: invalid sha256 for %s", file.Path)
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return fmt.Errorf("releasebundle: invalid sha256 for %s: %w", file.Path, err)
		}
	}
	return nil
}

func validatePath(path string) error {
	if path == "" || filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return fmt.Errorf("releasebundle: unsafe path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean != path || clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return fmt.Errorf("releasebundle: unsafe path %q", path)
	}
	return nil
}

func hashFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("releasebundle: read %s: %w", path, err)
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:]), nil
}
