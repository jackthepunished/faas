package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// framework is the source kind auto-detected from the current directory when
// `faas deploy` is run with no source flag (issue #313). It intentionally
// mirrors the top-level filename rule in pkg/builderd/detect.go, but is copied
// here rather than imported: importing pkg/builderd would pull the entire
// server stack (DB, scheduler, firecracker) into the CLI binary. The rule is
// small and stable; the two copies are the accepted trade for zero server
// blast radius. The server re-detects authoritatively from the uploaded
// tarball, so this value is only used for CLI UX + the dockerfile flag.
type framework string

const (
	fwNode    framework = "node"
	fwPython  framework = "python"
	fwGo      framework = "go"
	fwDocker  framework = "docker"
	fwUnknown framework = "unknown"
)

// defaultExcludeDirs are directory names dropped anywhere in the tree. These
// are build artifacts / VCS metadata that both bloat the tarball past the
// SourceTarballMaxMB cap (pkg/api/limits.go) and are reproduced server-side by
// the builder. Aggressive-but-predictable: other dotfiles (.env, .dockerignore,
// .npmrc, .github/) are deliberately kept.
var defaultExcludeDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
}

// defaultExcludeFiles are basenames dropped anywhere in the tree (OS junk).
var defaultExcludeFiles = map[string]bool{
	".DS_Store": true,
	"Thumbs.db": true,
}

// packEpoch is a fixed modification time stamped on every archive entry so the
// packed tarball is byte-reproducible for a given input (tests depend on this,
// and it avoids leaking local mtimes).
var packEpoch = time.Unix(0, 0)

// shouldExclude reports whether a slash-separated path relative to the packed
// root (e.g. "node_modules/foo/index.js") should be omitted from the archive.
func shouldExclude(relSlashPath string, isDir bool) bool {
	base := relSlashPath
	if i := strings.LastIndex(relSlashPath, "/"); i >= 0 {
		base = relSlashPath[i+1:]
	}
	if isDir {
		return defaultExcludeDirs[base]
	}
	// Any file living under an excluded directory is dropped implicitly by
	// WalkDir's SkipDir, so here we only handle file-level rules.
	if defaultExcludeFiles[base] {
		return true
	}
	if strings.HasSuffix(base, ".pyc") {
		return true
	}
	return false
}

// detectFramework sniffs the TOP-LEVEL entries of srcDir (no recursion) and
// returns the implied framework. A Dockerfile wins over language markers, in
// lockstep with pkg/builderd/detect.go. Returns fwUnknown when nothing at the
// root identifies the project.
func detectFramework(srcDir string) framework {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fwUnknown
	}
	var hasDocker, hasNode, hasPython, hasGo bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(e.Name()) {
		case "dockerfile":
			hasDocker = true
		case "package.json":
			hasNode = true
		case "requirements.txt", "pyproject.toml", "pipfile", "setup.py":
			hasPython = true
		case "go.mod":
			hasGo = true
		}
	}
	switch {
	case hasDocker:
		return fwDocker
	case hasNode:
		return fwNode
	case hasPython:
		return fwPython
	case hasGo:
		return fwGo
	}
	return fwUnknown
}

// packDirToTarGz walks srcDir and writes a gzipped tar archive to destPath. The
// archive's single top-level directory is filepath.Base(srcDir), preserving the
// invariant apid's validateTarballShape depends on (one project root). Symlinks,
// hardlinks and device nodes are rejected — apid rejects them too, so failing
// fast in the CLI is strictly better UX. Regular files are streamed with a fixed
// mtime for reproducibility. Returns the count of regular files archived.
//
// The gzip→tar→walk shape mirrors cmd/faas/templates/embed.go:TarGz.
func packDirToTarGz(srcDir, destPath string) (fileCount int, err error) {
	root := filepath.Base(srcDir)

	f, err := os.Create(destPath)
	if err != nil {
		return 0, fmt.Errorf("create archive %s: %w", destPath, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close archive: %w", cerr)
		}
	}()
	gz := gzip.NewWriter(f)
	defer func() {
		if cerr := gz.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close gzip: %w", cerr)
		}
	}()
	tw := tar.NewWriter(gz)
	defer func() {
		if cerr := tw.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close tar: %w", cerr)
		}
	}()

	// Collect first, sort, then write — a deterministic archive order makes
	// the output reproducible (same input → same bytes).
	type entry struct {
		abs  string
		rel  string // slash-separated, relative to srcDir (no root prefix)
		info os.FileInfo
	}
	var entries []entry
	walkErr := filepath.WalkDir(srcDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == srcDir {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		relSlash := filepath.ToSlash(rel)
		if shouldExclude(relSlash, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to pack symlink %q (unpack the target or use --tarball)", relSlash)
		}
		if !d.IsDir() && !mode.IsRegular() {
			return fmt.Errorf("refusing to pack irregular file %q (device/socket/pipe)", relSlash)
		}
		entries = append(entries, entry{abs: p, rel: relSlash, info: info})
		return nil
	})
	if walkErr != nil {
		return 0, fmt.Errorf("walk %s: %w", srcDir, walkErr)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	for _, e := range entries {
		hdr, herr := tar.FileInfoHeader(e.info, "")
		if herr != nil {
			return 0, fmt.Errorf("header for %s: %w", e.rel, herr)
		}
		hdr.Name = root + "/" + e.rel
		if e.info.IsDir() {
			hdr.Name += "/"
		}
		hdr.ModTime = packEpoch
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, fmt.Errorf("write header %s: %w", hdr.Name, err)
		}
		if e.info.IsDir() {
			continue
		}
		if err := copyRegular(tw, e.abs); err != nil {
			return 0, err
		}
		fileCount++
	}
	return fileCount, nil
}

// copyRegular streams one regular file into the tar writer. It routes through
// openCustomerFile (commands5.go) — the vetted symlink-safe boundary — rather
// than a bare os.Open, both to satisfy the cmd/faas os.Open tripwire and
// because the walked paths are customer-supplied (TOCTOU: a path Lstat'd as
// regular during the walk could be swapped for a symlink before we read it).
func copyRegular(tw *tar.Writer, abs string) error {
	f, err := openCustomerFile(abs)
	if err != nil {
		return fmt.Errorf("open %s: %w", abs, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copy %s: %w", abs, err)
	}
	return nil
}

// autoPackCwd is the zero-config entry point: it packs srcDir into a fresh temp
// tarball, detects the framework, and returns everything the deploy path needs.
// The caller owns the returned path and must os.Remove it. On any error the
// temp file (if created) is removed before returning.
func autoPackCwd(srcDir string) (tarballPath string, fw framework, fileCount int, err error) {
	f, err := os.CreateTemp("", "faas-cwd-*.tar.gz")
	if err != nil {
		return "", fwUnknown, 0, fmt.Errorf("create temp tarball: %w", err)
	}
	path := f.Name()
	_ = f.Close()

	n, err := packDirToTarGz(srcDir, path)
	if err != nil {
		_ = os.Remove(path)
		return "", fwUnknown, 0, err
	}
	return path, detectFramework(srcDir), n, nil
}
