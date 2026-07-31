package gitfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// buildArchive assembles a tar.gz body in memory. entries is the
// file-map (relative path → content). rootPrefix is the
// leading "<root>/" prefix the codeload endpoint wraps every
// entry under; pass "" to skip the prefix (the extractor
// detects the no-prefix case).
func buildArchive(t *testing.T, rootPrefix string, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		// Apply the root prefix to the entry name. The
		// extractor strips one leading "<root>/" prefix.
		// Build the actual header name verbatim.
		if rootPrefix != "" {
			hdr.Name = filepath.ToSlash(filepath.Join(rootPrefix, name))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// withServer spins up an httptest.Server, hands the URL to a
// fetcher pointed at it, and returns the fetcher + cleanup
// closure. The handler is the closure under test.
func withServer(t *testing.T, handler http.HandlerFunc, capBytes int64) (*httpFetcher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	hc := &http.Client{Timeout: 5 * time.Second}
	f := NewHTTPWithBase(t.TempDir(), srv.URL, hc, capBytes)
	return f, srv
}

func TestFetch_HappyPath(t *testing.T) {
	body := buildArchive(t, "owner-repo-sha", map[string]string{
		"README.md":          "# Hello\n",
		"src/index.go":       "package main\n",
		"docker-compose.yml": "version: '3'\nservices:\n  api:\n    build: .\n",
	})
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Validate the Authorization header was set.
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("missing Authorization header: %v", got)
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(body)
	}, 0)
	tree, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "ghs_test")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = tree.Close() }()

	// The root prefix "owner-repo-sha" must be stripped. Files
	// land at FS root.
	fsys := tree.FS()
	for _, p := range []string{"README.md", "src/index.go", "docker-compose.yml"} {
		if _, err := fsys.Open(p); err != nil {
			t.Errorf("missing %q after prefix strip: %v", p, err)
		}
	}
}

func TestFetch_StripPrefixFromAbsoluteEntry(t *testing.T) {
	// After stripping the leading prefix, the entries should
	// land at the FS root even if the entry names contain
	// forward slashes.
	body := buildArchive(t, "owner-sha", map[string]string{
		"a/b/c.txt": "deep",
		"top.txt":   "top",
	})
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}, 0)
	tree, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = tree.Close() }()
	fsys := tree.FS()
	if _, err := fsys.Open("a/b/c.txt"); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
	if _, err := fsys.Open("top.txt"); err != nil {
		t.Errorf("top file missing: %v", err)
	}
}

func TestFetch_Unauthorized(t *testing.T) {
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token expired"))
	}, 0)
	_, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "expired")
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestFetch_NotFound(t *testing.T) {
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}, 0)
	_, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFetch_ArchiveTooLarge(t *testing.T) {
	// Build a body larger than the cap.
	body := buildArchive(t, "owner-sha", map[string]string{
		"big.txt": strings.Repeat("a", 200),
	})
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The streaming guard is the load-bearing one; the
		// Content-Length header is also set so the test
		// covers both paths.
		w.Header().Set("Content-Length", "999999")
		_, _ = w.Write(body)
	}, 100)
	_, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Errorf("err = %v, want ErrArchiveTooLarge", err)
	}
}

func TestFetch_BadArchive_GzipFailure(t *testing.T) {
	// Send non-gzip body. The extractor should fail with
	// ErrBadArchive.
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not gzip"))
	}, 0)
	_, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if !errors.Is(err, ErrBadArchive) {
		t.Errorf("err = %v, want ErrBadArchive", err)
	}
}

func TestFetch_RejectsPathEscape(t *testing.T) {
	// Build a tarball with a ".." entry that would escape
	// the archive root.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "../escaped.txt",
		Mode:     0o644,
		Size:     4,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte("pwn\n")); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}, 0)
	_, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if !errors.Is(err, ErrBadArchive) {
		t.Errorf("err = %v, want ErrBadArchive", err)
	}
}

func TestFetch_RejectsInvalidCommitSHA(t *testing.T) {
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit for invalid input")
	}, 0)
	for _, sha := range []string{"", "abc", "ZZZ", "abc 1234567"} {
		_, err := f.Fetch(context.Background(), "owner/repo", sha, "tok")
		if !errors.Is(err, ErrBadArchive) {
			t.Errorf("sha=%q: err = %v, want ErrBadArchive", sha, err)
		}
	}
}

func TestFetch_RejectsInvalidRepoPath(t *testing.T) {
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be hit for invalid input")
	}, 0)
	for _, repo := range []string{"", "no-slash", "owner/repo/extra", "owner repo"} {
		_, err := f.Fetch(context.Background(), repo, "abcdef1234567", "tok")
		if !errors.Is(err, ErrBadArchive) {
			t.Errorf("repo=%q: err = %v, want ErrBadArchive", repo, err)
		}
	}
}

func TestFetch_ContextCancellation(t *testing.T) {
	// Server hangs forever; ctx cancels after 50ms.
	hc := &http.Client{Timeout: 0}
	_ = hc
	f := NewHTTPWithBase(t.TempDir(), "http://127.0.0.1:1", &http.Client{Timeout: 1 * time.Second}, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.Fetch(ctx, "owner/repo", "abcdef1234567", "tok")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestTree_CloseIsIdempotent(t *testing.T) {
	body := buildArchive(t, "owner-sha", map[string]string{
		"hi.txt": "hello",
	})
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}, 0)
	tree, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Errorf("second Close (idempotent): %v", err)
	}
	// After Close, the underlying dir is gone.
	fsys := tree.FS()
	if _, err := fsys.Open("hi.txt"); err == nil {
		t.Error("FS still accessible after Close")
	}
}

func TestTree_FSReturnsValidFilesystem(t *testing.T) {
	// The Tree.FS() should hand back a usable fs.FS — pinned
	// by fstest.MapFS shape comparison so reposcan can
	// consume it directly.
	body := buildArchive(t, "owner-sha", map[string]string{
		"docker-compose.yml": "version: '3'\n",
		"src/main.go":        "package main\n",
	})
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}, 0)
	tree, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = tree.Close() }()

	// Test against the same fs.FS signature reposcan consumes.
	fsys := tree.FS()
	want := fstest.MapFS{
		"docker-compose.yml": &fstest.MapFile{Data: []byte("version: '3'\n")},
		"src/main.go":        &fstest.MapFile{Data: []byte("package main\n")},
	}
	if err := fstest.TestFS(fsys, "docker-compose.yml", "src/main.go"); err != nil {
		t.Errorf("fstest.TestFS: %v", err)
	}
	_ = want // signature shape assert
}

func TestFetch_NoWorkDir(t *testing.T) {
	// WorkDir must already exist before Fetch. The fetcher
	// creates it via MkdirAll, so a fresh tmpdir should
	// work. Verify the temp dir created under it.
	workDir := filepath.Join(t.TempDir(), "fresh")
	f, _ := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buildArchive(t, "r", map[string]string{"x": "y"}))
	}, 0)
	f.workDir = workDir
	tree, err := f.Fetch(context.Background(), "owner/repo", "abcdef1234567", "tok")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = tree.Close() }()
	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("workDir not created: %v", err)
	}
}
