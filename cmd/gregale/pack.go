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
// `gregale deploy` is run with no source flag (issue #313). It intentionally
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

// Function runtime literals — declared as constants here so the
// inferFunctionRuntime switch can use named values rather than
// repeating the wire string (which goconst would otherwise flag
// because the runtime names recur across the CLI: --template
// function-node forces "node22", the wire form field carries
// "node22", etc.). The whitelist matches apid's validator at
// cmd/apid/handlers.go:98 (node22 / node24 / python312 /
// python313 / go124 / go124-alpine); this PR only emits the
// runtime values the auto-detect path can infer, but adding a
// new runtime to the map is a follow-up ADR.
const (
	runtimeNode22    = "node22"
	runtimePython312 = "python312"
	runtimeGo124     = "go124"
)

// shape is the deploy shape auto-detected from the current directory when
// `gregale deploy` runs with no source flag (issue #737 / ADR-083). A function
// shape means "single handler file at the root, no app markers"; an app shape
// means any app marker (package.json / requirements.txt / go.mod / Dockerfile
// / …) is present at the root. The convention is intentionally narrow:
// a customer with `package.json + handler.js` is unambiguously a Node app,
// and must pass --function to force function mode (otherwise auto-detection
// would silently break every existing Node user).
//
// shapeUnknown fires when the cwd is empty or contains only excluded files
// (.git, .DS_Store, README, dotfiles). The CLI surfaces this as the no-source
// error and lets the customer pick --image, --tarball, --template, --repo,
// or the new --function/--app explicit flags.
type shape int

const (
	shapeApp shape = iota
	shapeFunction
	shapeUnknown
)

// functionHandlerFiles is the closed set of file names that, when present
// alone at the project root, signal a function deploy. The names match the
// template convention at cmd/gregale/templates/function-node/handler.js (and
// its python/go siblings). Anything else falls through to shapeApp or
// shapeUnknown.
var functionHandlerFiles = map[string]bool{
	"handler.js": true,
	"handler.ts": true,
	"handler.py": true,
	"handler.go": true,
}

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

// appMarker is the closed set of filenames whose presence at the project
// root (or, for the depth-2 hint path, under a single subdirectory) marks
// a directory as containing deployable source for an *app* (Railpack
// framework path). A README.md or dotfile in the same directory is NOT
// a marker — those files don't change the deploy shape.
//
// Single source of truth: detectFramework and detectNestedMarkerHint
// both consult this map, so a new marker (e.g. Cargo.toml for a Rust
// Railpack pipeline that lands in a future ADR) only needs to be added
// here, not in two switches. The set matches pkg/builderd/detect.go:73-82
// on the server side — the CLI is intentionally the lighter view (no
// Dockerfile priority ordering, since the CLI's job is to hint, not to
// pick a build pipeline).
var appMarker = map[string]bool{
	"package.json":     true,
	"requirements.txt": true,
	"pyproject.toml":   true,
	"pipfile":          true,
	"setup.py":         true,
	"go.mod":           true,
	"dockerfile":       true,
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
		// Keep in sync with pkg/builderd/detect.go — the rule intentionally
		// mirrors the server-side detector; if you change one, change both.
		// Marker membership is sourced from appMarker so a single edit
		// (issue #744 / ADR-086) covers both detectFramework and the new
		// detectNestedMarkerHint helper.
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

// detectNestedMarkerHint returns true if srcDir contains at least one app
// marker within 2 subdirectory levels of the project root, capturing the
// common monorepo layout (apps/web/package.json, services/api/go.mod,
// libs/x/pyproject.toml, frontend/package.json). Used by resolveDeployShape's
// shapeUnknown branch (issue #744 / ADR-086) to surface a "looks like a
// workspace" hint pointing the customer at `gregale scan --path .` instead
// of opening an issue.
//
// Cheap on purpose: a recursive os.ReadDir per top-level subdir, capped at
// depth 2, no file contents read. Excluded subdirs (defaultExcludeDirs —
// node_modules, .git, vendor, __pycache__ — plus dotfile dirs) are skipped
// at every depth so a stray node_modules/x/package.json does not
// false-positive as a workspace.
//
// Depth-3+ monorepos (e.g. apps/web/services/api/package.json — three
// subdirectory levels deep) intentionally return false: the customer gets
// the existing bare error. Deeper detection belongs to `gregale scan`
// (pkg/reposcan), which already handles it via the
// workspaces_extra_test.go "monorepo" fixture. The CLI hint is just a
// pointer at the next step.
func detectNestedMarkerHint(srcDir string) bool {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if isExcludedSubdir(e.Name()) {
			continue
		}
		// walkForMarkers(d1, 1) recurses one level into each top-level
		// subdir, which puts us at depth 2 from the project root — files
		// at apps/web/package.json are seen; files at
		// apps/web/services/api/package.json (depth 3) are not.
		if walkForMarkers(filepath.Join(srcDir, e.Name()), 1) {
			return true
		}
	}
	return false
}

// walkForMarkers recurses at most maxDepth levels below dir looking for an
// app marker. Returns true on the first hit. Excluded subdirs
// (defaultExcludeDirs + dotfiles) are skipped at every level so a real
// workspace isn't false-positive-masked by a sibling node_modules tree.
func walkForMarkers(dir string, maxDepth int) bool {
	if maxDepth < 0 {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if isExcludedSubdir(name) {
				continue
			}
			if walkForMarkers(filepath.Join(dir, name), maxDepth-1) {
				return true
			}
			continue
		}
		if appMarker[strings.ToLower(name)] {
			return true
		}
	}
	return false
}

// isExcludedSubdir reports whether a subdirectory should be skipped at any
// depth during the nested-marker hint walk (issue #744 / ADR-086). The
// defaultExcludeDirs set covers build artifacts and VCS metadata; dotfile
// dirs (e.g. .vscode, .github) are also skipped so a repo with .vscode/
// doesn't confuse the marker sniff.
func isExcludedSubdir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return defaultExcludeDirs[strings.ToLower(name)]
}

// NestedMarkerHintError wraps a deploy-shape error to carry the
// "looks like a workspace — try: gregale scan --path ." hint through
// printErr (issue #744 / ADR-086). The wrapped err is the original
// shapeUnknown message; Hint is the additional customer-facing line.
// errors.As extracts the hint in printErr so JSON-mode consumers can
// programmatically detect it without parsing free-form stderr text.
type NestedMarkerHintError struct {
	Dir  string
	Hint string
	Err  error
}

func (e *NestedMarkerHintError) Error() string { return e.Err.Error() }
func (e *NestedMarkerHintError) Unwrap() error { return e.Err }

// detectShape sniffs the TOP-LEVEL entries of srcDir (no recursion) and returns
// the deploy shape (issue #737 / ADR-083). The rule:
//
//   - shapeFunction: exactly one of {handler.js, handler.ts, handler.py,
//     handler.go} at the root AND none of the app markers (package.json /
//     requirements.txt / pyproject.toml / Pipfile / setup.py / go.mod /
//     Dockerfile). A README.md and dotfiles are ignored — most repos have
//     them and they don't change the shape.
//   - shapeApp: any app marker present at the root. App markers always win
//     over a co-located handler.* — a customer with `package.json +
//     handler.js` is unambiguously a Node app and must pass --function to
//     override.
//   - shapeUnknown: cwd is empty, missing, or contains only excluded files.
//
// The detector is intentionally minimal: it mirrors the top-level sniff rule
// the server's pkg/builderd/detect.go:41-95 applies to the uploaded tarball,
// so a CLI-detected shape matches what builderd will see on the other end.
// Files only ever seen during the build (node_modules, .git, __pycache__,
// vendor) are NOT app markers — the framework detector only counts the
// "primary" files, and so does shape.
func detectShape(srcDir string) shape {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return shapeUnknown
	}
	var (
		hasAppMarker bool
		handlerFile  string // name of the single handler.* if present
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Excluded files (build artifacts, OS junk) are not markers —
		// they don't change the shape. Match the same set defaultExcludeFiles
		// uses so behaviour is consistent with what gets packed.
		if defaultExcludeFiles[name] {
			continue
		}
		// Dotfiles (other than .git, which is excluded above) and
		// README* are ignored. The customer reads a README on GitHub,
		// not in a deployable function — and dotfiles (.env,
		// .dockerignore, .npmrc) are common and not shape-changing.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(strings.ToLower(name), "readme") {
			continue
		}
		switch strings.ToLower(name) {
		// Keep in sync with detectFramework's app-marker switch and
		// pkg/builderd/detect.go:73-82. If you change one, change all.
		case "package.json", "requirements.txt", "pyproject.toml",
			"pipfile", "setup.py", "go.mod", "dockerfile":
			hasAppMarker = true
		}
		if functionHandlerFiles[strings.ToLower(name)] {
			// Second handler.* wins means the shape is ambiguous —
			// fall through to shapeApp (any co-located handler that
			// isn't a single, named handler.js signals "this is a
			// project, not a function").
			if handlerFile != "" {
				hasAppMarker = true
				continue
			}
			handlerFile = strings.ToLower(name)
		}
	}
	switch {
	case hasAppMarker:
		return shapeApp
	case handlerFile != "":
		return shapeFunction
	default:
		return shapeUnknown
	}
}

// inferFunctionRuntime picks the apid runtime + wire handler for a
// function-shaped repo. The runtime is keyed on the handler file's
// extension; the wire handler is the literal `handler.handler` value
// that imaged's function-layer manifest rewrites to /app/<runtime>.js
// (per the convention at cmd/gregale/templates/function-node/handler.js
// and defaultTemplateHandler at cmd/gregale/commands2.go:48). The bool
// is false when the cwd lacks a single, recognised handler file —
// callers should fall back to shapeUnknown in that case.
//
// This is the load-bearing helper that wires detectShape into the
// cmdDeployTarball flow: detectShape picks shapeFunction, then
// inferFunctionRuntime fills in runtime + handler for the multipart
// form. Both default to the same convention the function-* templates
// force today, so an existing function customer who runs
// `gregale deploy` against a hand-written handler.js gets the exact
// same wire shape they would have got via
// `gregale --template function-node --tarball ...`.
func inferFunctionRuntime(srcDir string) (runtime, handler string, ok bool) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return "", "", false
	}
	var picked string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if functionHandlerFiles[strings.ToLower(e.Name())] {
			if picked != "" {
				return "", "", false // ambiguous — multiple handlers
			}
			picked = strings.ToLower(e.Name())
		}
	}
	if picked == "" {
		return "", "", false
	}
	switch strings.ToLower(picked) {
	case "handler.js", "handler.ts":
		return runtimeNode22, defaultTemplateHandler, true
	case "handler.py":
		return runtimePython312, defaultTemplateHandler, true
	case "handler.go":
		return runtimeGo124, defaultTemplateHandler, true
	}
	return "", "", false
}

// zeroConfigSourceCapMB is the conservative per-plan floor used by the
// zero-config preflight: the server will reject anything above this on
// Free/Hobby (see pkg/api/limits.go: SourceTarballMaxMB). We don't have the
// customer's plan on the wire without a GetAccount round-trip; the floor is
// the safest choice because a zero-config abort on Free is much better UX
// than a slow upload that ends in a 413. Customers on Pro/Scale who exceed
// 100 MB but fit within 250 MB should pass --tarball of a hand-built archive.
const zeroConfigSourceCapMB = 100

// packDirToTarGz walks srcDir and writes a gzipped tar archive to destPath. The
// archive's single top-level directory is filepath.Base(srcDir), preserving the
// invariant apid's validateTarballShape depends on (one project root). Symlinks,
// hardlinks and device nodes are rejected — apid rejects them too, so failing
// fast in the CLI is strictly better UX. Regular files are streamed with a fixed
// mtime for reproducibility, and each file is read through a LimitReader
// capped at zeroConfigSourceCapMB so a single runaway file aborts early
// instead of materialising its full size into the tar. After the archive is
// written the on-disk size is re-checked against the cap (gzip compression
// can change the byte count either way). Returns the count of regular files
// archived.
//
// The gzip→tar→walk shape mirrors cmd/gregale/templates/embed.go:TarGz.
func packDirToTarGz(srcDir, destPath string) (regularFileCount int, err error) {
	root := filepath.Base(srcDir)

	f, err := os.Create(destPath)
	if err != nil {
		return 0, fmt.Errorf("create archive %s: %w", destPath, err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	defer func() {
		// Defer must close in tar→gzip→file order so gzip's trailer flushes
		// to disk before the file's Close. Idempotent: Close on a
		// previously-closed writer returns an error we ignore.
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
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
			return fmt.Errorf("symlink %q is not allowed in source tarballs (apid rejects symlink entries) — remove the symlink or pass --tarball of an archive you built yourself", relSlash)
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
		regularFileCount++
	}
	// Final size check. Close gzip→tar before statting so the on-disk size
	// is the actual packed size (gzip writes its trailer at Close). Close
	// is idempotent — the defer above also calls them on any return path.
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return 0, fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return 0, fmt.Errorf("close gzip: %w", err)
	}
	st, err := os.Stat(destPath)
	if err != nil {
		return 0, fmt.Errorf("stat packed tarball: %w", err)
	}
	const capBytes = zeroConfigSourceCapMB * 1024 * 1024
	if st.Size() > capBytes {
		return 0, fmt.Errorf("packed cwd is %d MB, over the %d MB zero-config cap; trim large files or pass --tarball of a hand-built archive",
			st.Size()/(1024*1024), zeroConfigSourceCapMB)
	}
	return regularFileCount, nil
}

// copyRegular streams one regular file into the tar writer. It routes through
// openCustomerFile (commands5.go) — the vetted symlink-safe boundary — rather
// than a bare os.Open, both to satisfy the cmd/gregale os.Open tripwire and
// because the walked paths are customer-supplied (TOCTOU: a path Lstat'd as
// regular during the walk could be swapped for a symlink before we read it).
//
// The stream is wrapped in a LimitReader at zeroConfigSourceCapMB so a single
// runaway file (a 2 GB raw dataset committed by accident) aborts early with
// a clear error instead of streaming gigabytes through gzip→tar. The cap
// matches the Free/Hobby floor; Pro/Scale customers who deliberately commit
// larger files should pass --tarball of a hand-built archive.
func copyRegular(tw *tar.Writer, abs string) error {
	f, err := openCustomerFile(abs)
	if err != nil {
		return fmt.Errorf("open %s: %w", abs, err)
	}
	defer func() { _ = f.Close() }()
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("copy %s: %w", abs, err)
	}
	const warnBytes = zeroConfigSourceCapMB * 1024 * 1024
	if n >= warnBytes {
		return fmt.Errorf("refusing to pack %s: %d bytes >= %d MB per-file cap (untracked large file? pass --tarball of a hand-built archive)",
			filepath.Base(abs), n, zeroConfigSourceCapMB)
	}
	return nil
}

// buildCreateRequest stamps the issue #737 / ADR-083 fields onto the
// CreateAppRequest the CLI hands to apid. Lives in commands2.go (next
// to the caller) so the function-testable wire-shape contract stays
// in one file; tests in commands2_test.go exercise it directly.

// resolveDeployShape runs the cwd detector and emits the customer-visible
// "Detected: …" line for issue #737 / ADR-083. The print goes BEFORE the
// multipart POST so the customer's first response from the CLI is the
// deploy shape. The explicit --function / --app flags short-circuit the
// detector (see the mutex check in cmdDeployTarball); this helper assumes
// they have already been mutex-checked. On shapeUnknown, an actionable
// error is returned — the caller turns it into a customer-visible
// printErr. The returned (runtime, handler) are non-empty only on the
// shapeFunction path; on shapeApp they are empty (server-side Railpack
// auto-detects). Allocated to live in pack.go (next to detectShape /
// inferFunctionRuntime) so the unit test exercises both the wire
// contract and the print line in one place.
//
// The print is suppressed when jsonOutput is true: the §3.2 --json
// contract requires stdout to be a single parseable JSON object, so
// a freeform "Detected: …" line would corrupt `gregale deploy --json
// | jq`. The shape is still resolved (the wire shape is the same
// either way); only the customer-visible banner is gated.
func resolveDeployShape(srcDir string, explicitFunction, explicitApp, jsonOutput bool) (shape, string, string, error) {
	detected := detectShape(srcDir)
	if explicitFunction {
		detected = shapeFunction
	} else if explicitApp {
		detected = shapeApp
	}
	switch detected {
	case shapeUnknown:
		baseErr := fmt.Errorf(
			"no deployable source found in %s: expected package.json, requirements.txt / pyproject.toml / "+
				"Pipfile / setup.py, go.mod, or Dockerfile at the project root for an *app*, "+
				"OR a single handler.{js,ts,py,go} for a *function* — "+
				"or pass --image, --tarball, --template, --repo, --function, or --app",
			filepath.Base(srcDir))
		// Issue #744 / ADR-086: if the cwd has app markers at depth 1 (a
		// common monorepo layout — apps/web/package.json, services/api/go.mod),
		// surface a customer-visible hint pointing at `gregale scan --path .`.
		// The hint is wrapped in NestedMarkerHintError so printErr can route
		// it to stderr without corrupting --json stdout. Customers with deep
		// (depth-3+) monorepos still get the bare error — that's fine, the
		// reposcan tree handles them via `gregale scan` regardless.
		if detectNestedMarkerHint(srcDir) {
			return shapeUnknown, "", "", &NestedMarkerHintError{
				Dir: filepath.Base(srcDir),
				Hint: fmt.Sprintf(
					"Hint: found app markers under subdirectories of %s — this looks like a workspace (monorepo). "+
						"Run `gregale scan --path .` to decompose it into per-app plans, then apply them individually.",
					filepath.Base(srcDir)),
				Err: baseErr,
			}
		}
		return shapeUnknown, "", "", baseErr
	case shapeFunction:
		rt, hnd, ok := inferFunctionRuntime(srcDir)
		if !ok {
			return shapeUnknown, "", "", fmt.Errorf(
				"--function requires a single handler.{js,ts,py,go} at the project root; "+
					"found zero or ambiguous handler files in %s",
				filepath.Base(srcDir))
		}
		// Print uses the inferred values — the caller may still
		// override them via an explicit --runtime / --handler (out of
		// scope here, but the CLI does it just after this returns).
		if !jsonOutput {
			PrintOK(osStdout, "Detected: function, runtime=%s, handler=%s", rt, hnd)
		}
		return shapeFunction, rt, hnd, nil
	case shapeApp:
		fw := detectFramework(srcDir)
		if !jsonOutput {
			PrintOK(osStdout, "Detected: app, framework=%s", fw)
		}
		return shapeApp, "", "", nil
	}
	return shapeUnknown, "", "", fmt.Errorf("internal: resolveDeployShape fell through")
}

// autoPackCwd is the zero-config entry point: it packs srcDir into a fresh temp
// tarball, detects the framework, and returns everything the deploy path needs.
// The caller owns the returned path and must os.Remove it. On any error the
// temp file (if created) is removed before returning. fileCount is the count
// of regular files archived — NOT the server-side entry count, which includes
// directory entries (see cmd/apid/deploy_inputs.go:maxSourceFiles).
func autoPackCwd(srcDir string) (tarballPath string, fw framework, fileCount int, err error) {
	f, err := os.CreateTemp("", "gregale-cwd-*.tar.gz")
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
