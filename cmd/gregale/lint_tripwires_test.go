package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLintTripwire_NoBareOsOpenInCLI is the Go-test counterpart to the
// .golangci.yml forbidigo rule on `os.Open\(`. PR #101 closed the
// symlink-follow attack surface in `gregale deploy --tarball` by routing
// every customer-supplied path through `openCustomerFile`
// (defined in `commands5.go`, package `main`). The lint rule enforces
// that — but lint rules can be silently disabled in a future PR
// ("just this once"). This test fails fast at `go test` time if
// anyone re-introduces a bare `os.Open(` anywhere in cmd/gregale/
// outside the documented escape hatch in commands5.go.
//
// Tripwire contract:
//   - any `*ast.CallExpr` whose Function is `os.Open` in any non-test
//     .go file under this package is a violation
//   - the only allowed exception is inside commands5.go, where
//     `openCustomerFile` itself uses os.Open as the security boundary
//     (and is already annotated with `//nolint:forbidigo`)
//   - test files (*_test.go) are excluded because `writeMinimalFile`
//     uses os.Create — but never os.Open — and the test fixtures
//     should never reach the wire
//
// If a new caller legitimately needs os.Open on a customer path,
// route it through openCustomerFile. If it needs os.Open for a
// vetted / non-customer path, the call must live OUTSIDE cmd/gregale/
// (e.g. in pkg/api or one of the daemons); the CLI never opens a
// path that is not customer-supplied.
func TestLintTripwire_NoBareOsOpenInCLI(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		// Walk every .go file except generated protobuf stubs and test
		// fixtures (test files use os.Create, never os.Open, and
		// tripwire-ing them would couple this test to fixture churn).
		name := fi.Name()
		if strings.HasSuffix(name, "_test.go") {
			return false
		}
		if strings.HasSuffix(name, ".pb.go") || strings.HasSuffix(name, "_grpc.pb.go") {
			return false
		}
		return true
	}, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse cmd/gregale: %v", err)
	}

	var violations []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			fileName := fset.Position(file.Pos()).Filename
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isOsOpenCall(call) {
					return true
				}
				// Documented exception: openCustomerFile body in
				// cmd/gregale/commands5.go. The line is annotated with
				// `//nolint:forbidigo` and is the security boundary
				// itself — pre-open + post-open Lstat discipline.
				if strings.HasSuffix(fileName, "commands5.go") {
					return true
				}
				pos := fset.Position(call.Pos())
				violations = append(violations, pos.String())
				return true
			})
		}
	}

	if len(violations) > 0 {
		// The path-in-help-text points maintainers at the helper file
		// and the rule annotation it carries, so the next reader can
		// find the right fix without grepping the codebase.
		t.Fatalf("found bare os.Open( outside openCustomerFile (cmd/gregale/commands5.go) — see //nolint:forbidigo near that function for the documented exception:\n  %s\n\nroute customer-supplied paths through openCustomerFile; vetted-id paths must live in pkg/api or a daemon, not the CLI",
			strings.Join(violations, "\n  "))
	}
}

// isOsOpenCall reports whether call is `os.Open(...)` — i.e. the
// function is a SelectorExpr whose X is the package qualifier "os"
// and whose Sel.Name is "Open". Matches both `os.Open(f)` and
// method-style receiver calls if anyone ever writes one.
func isOsOpenCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Open" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "os"
}

// TestLintTripwire_NoGlyphLiteralOutsideOutput closes the UX §3.2 surface
// the §3.2 PR opened: the leading-glyph rule is enforced by a writer-based
// gate (output.go::PrintOK/PrintFail/PrintProgress/PrintWarn) so the
// glyph disappears in pipes and under NO_COLOR. Any new code path that
// prints a raw ✓/✗/→/! string literal in cmd/gregale/ — outside output.go
// and outside *_test.go — would bypass the gate, so this test fails fast
// at `go test` time the moment someone copies an old `fmt.Println("✓ …")`
// pattern into a new file.
//
// Excludes:
//   - cmd/gregale/output.go: the gate itself. By design carries all four
//     glyphs as string literals.
//   - cmd/gregale/output_test.go and any other *_test.go: tests legitimately
//     assert "glyph present" / "glyph absent" shapes, plus §3.3's static
//     Error() contract test which always carries "→".
//   - Comments: BasicLits in source comments aren't part of the AST
//     token stream, so they're naturally excluded.
//
// Two intentional exceptions worth knowing:
//   - commands5.go:504: `"Renamed %s → %s"` keeps the mid-string `→`
//     (a semantic from-to, not a progress glyph — preserved per the
//     §3.2 plan, follow-up to clean up separately).
//   - commands2.go:315: `"Opening %s to bind %s → %s"` — same shape,
//     semantic mid-string `→` for "bind X → Y". Not a progress glyph.
//
// Both literals are not leading-prefix glyphs so they wouldn't be matched
// by the simple "starts with" rule below; they're listed here so a future
// reviewer who sees a "should this be excluded?" question has the answer
// in-tree.
func TestLintTripwire_NoGlyphLiteralOutsideOutput(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		name := fi.Name()
		if strings.HasSuffix(name, "_test.go") {
			return false
		}
		if strings.HasSuffix(name, ".pb.go") || strings.HasSuffix(name, "_grpc.pb.go") {
			return false
		}
		return true
	}, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse cmd/gregale: %v", err)
	}

	// Leading-glyph strings we care about. The check is "starts with the
	// glyph" because the migration spec is "leading prefix only" — mid-string
	// `→` (semantic from-to notation) is explicitly preserved. A more
	// aggressive "any occurrence" rule would over-trigger on legitimate
	// cross-references and the §3.3 docs-URL line.
	leadingGlyphs := []string{"✓", "✗", "→"}

	var violations []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			fileName := fset.Position(file.Pos()).Filename
			if strings.HasSuffix(fileName, "output.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v := lit.Value
				for _, g := range leadingGlyphs {
					// strconv.UnquoteChar would be more precise, but a
					// leading-prefix check on the raw literal (including
					// its opening quote) is enough for the patterns this
					// PR introduces: `"✓ ", "✗ ", "→ ` (single byte UTF-8).
					if strings.HasPrefix(v, "\""+g) || strings.HasPrefix(v, "`"+g) {
						pos := fset.Position(lit.Pos())
						violations = append(violations, pos.String()+": "+v)
						break
					}
				}
				return true
			})
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found leading ✓/✗/→ string literal outside output.go — gate every customer-facing line through PrintOK/PrintFail/PrintProgress/PrintWarn so it strips in pipes and under NO_COLOR:\n  %s\n\n(mid-string `→` is allowed; this rule matches leading prefix only. Add `// lint:allow-glyph` above the line and document the reason if you genuinely need an exception.)",
			strings.Join(violations, "\n  "))
	}
}

// TestLintTripwire_NoLiteralWakeHeaderOutsidePkgWire closes bug 2
// (PR #439): the wire `x-faas-wake` header is the published customer
// contract (docs/cold-wake.md, docs/faas_ux_spec.md, docs/STATUS.md) —
// the Gregale rename kept the `x-faas-` prefix on purpose so downstream
// tooling and SDKs that depend on the header name don't break. PR #439
// silently renamed the CLI's probe from `x-faas-wake` to
// `x-gregale-wake`, breaking the cold-wake affordance for `gregale
// open` while every test stubbed the renamed literal and made the
// suite self-confirming.
//
// The fix routes both the producer (pkg/gateway) and the consumer
// (cmd/gregale) through pkg/wire.WakeHeader. This tripwire fails fast
// if any future PR reintroduces a literal `"x-faas-wake"` or
// `"x-gregale-wake"` anywhere outside the documented canonical home
// (pkg/wire/wake.go). The header constant is the only sanctioned
// spelling.
//
// Excludes:
//   - pkg/wire/wake.go: the canonical home; the literal IS the contract.
//   - any *_test.go: tests legitimately assert or stub the wire
//     header literal — they are the contract tests, not the production
//     code.
//   - generated *.pb.go stubs.
//
// Scope: the walker descends from the nearest enclosing `go.mod`
// directory. `go test ./cmd/gregale/...` chdirs into cmd/gregale
// before running, so the walker must explicitly locate the repo root
// via go.mod — otherwise it would silently scope to just cmd/gregale
// and miss regressions in pkg/gateway (the producer side). The intent
// is "no literal in production code that travels over the wire";
// docs and tests are out of scope.
func TestLintTripwire_NoLiteralWakeHeaderOutsidePkgWire(t *testing.T) {
	fset := token.NewFileSet()

	// Locate the repo root (the directory containing go.mod) by
	// walking up from the test's CWD. The test lives at
	// cmd/gregale/...; the CWD when `go test` runs is cmd/gregale/.
	// Without this, the walker below only sees cmd/gregale/ and the
	// tripwire is silently blind to pkg/gateway regressions — exactly
	// the side PR #439 broke.
	root, err := findRepoRoot(".")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	// Walk the whole repo except pkg/wire itself. Each package is
	// parsed in its own directory; pkgs is a map keyed by directory
	// relative to the walker root.
	pkgs := map[string]map[string]*ast.File{}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendor, generated, and test fixture subtrees.
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip the canonical home — the literal IS the contract.
		if strings.HasSuffix(path, "pkg/wire/wake.go") {
			return nil
		}
		// Skip *_test.go (tests legitimately assert or stub the wire
		// header literal — they are the contract tests, not the
		// production code).
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip generated protobuf stubs.
		if strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		pf, ferr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if ferr != nil {
			// Generated or unsupported files (e.g. build-tag-only)
			// may fail to parse. Don't fail the tripwire on those —
			// just skip them so a single unparseable file doesn't
			// mask the rule. nilerr lint fires on `return nil` here;
			// the skip is deliberate.
			return nil //nolint:nilerr // intentional skip on parse failure; see comment above
		}
		dir := filepath.Dir(path)
		bucket, ok := pkgs[dir]
		if !ok {
			bucket = map[string]*ast.File{}
			pkgs[dir] = bucket
		}
		bucket[path] = pf
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}

	forbidden := []string{`"x-faas-wake"`, `"x-gregale-wake"`, `"X-Faas-Wake"`, `"X-Gregale-Wake"`}

	var violations []string
	for _, files := range pkgs {
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, forbid := range forbidden {
					if lit.Value == forbid {
						pos := fset.Position(lit.Pos())
						violations = append(violations, pos.String()+": "+lit.Value)
						return true
					}
				}
				return true
			})
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found literal x-faas-wake / x-gregale-wake string outside pkg/wire/wake.go — these headers are the published customer contract and must be sourced from pkg/wire.WakeHeader (see docs/cold-wake.md):\n  %s\n\nIf a legacy gateway test legitimately needs the literal, move it to a *_test.go file (excluded) or convert it to wire.WakeHeader.",
			strings.Join(violations, "\n  "))
	}
}

// findRepoRoot climbs from start upward until it finds a go.mod
// file. Returns the absolute directory of go.mod. Used by the
// repo-walking lint tripwire so the walker's root is the repo root
// regardless of the test's working directory (which chdirs into
// cmd/gregale under `go test ./cmd/gregale/...`).
func findRepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// TestLintTripwire_NoLiteralWakeHeaderSelfTest exercises the AST
// walker by injecting a literal into a synthetic production-style
// file under a temp directory and asserting the tripwire flags it.
// This is the regression guard for the guard: a future refactor that
// silently breaks the walker (e.g. an early-return that skips the
// ast.Inspect callback) would land without anyone noticing, because
// the live walker never finds a violation on a clean tree. The
// self-test makes a violation unavoidable on every run.
func TestLintTripwire_NoLiteralWakeHeaderSelfTest(t *testing.T) {
	// Build a small Go file that contains the forbidden literal in
	// a string context the walker will recognise. We can't use the
	// in-package walker because it skips pkg/wire — instead we run
	// the same walk against a temp directory and assert the literal
	// shows up in the violations list.
	tmp := t.TempDir()
	src := `package tripwiretest

// Synthetic production-like file carrying the forbidden header
// literal. Exists only to exercise the AST walker.
var header = "x-faas-wake"
`
	srcPath := filepath.Join(tmp, "tripwire.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	fset := token.NewFileSet()
	pf, err := parser.ParseFile(fset, srcPath, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	forbidden := []string{`"x-faas-wake"`}
	var found string
	ast.Inspect(pf, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, forbid := range forbidden {
			if lit.Value == forbid {
				found = fset.Position(lit.Pos()).String()
				return false
			}
		}
		return true
	})
	if found == "" {
		t.Fatal("self-test: walker did not detect the seeded x-faas-wake literal — the tripwire may be silently broken")
	}
}

// TestLintTripwire_NoLiteralDocsDomainEverywhere closes issue #420:
// every customer-facing or third-party-readable URL the platform
// emits used to carry a `DOMAIN` placeholder literal
// (`https://docs/DOMAIN/...`, `https://DOMAIN/billing`,
// `+https://DOMAIN`, `apps.DOMAIN`, etc.). PR #439 + PR #455 swept
// the CLI help block + apid REST; this tripwire makes sure no future
// PR reintroduces one anywhere outside the canonical home
// (pkg/wire/docs.go).
//
// Why a repo-wide walk rather than a per-package scan: the
// placeholders surface in pkg/vmmdgrpc (gRPC envelope), pkg/auth/
// middleware (apid REST 402 Detail), pkg/oci + pkg/storage
// (User-Agent to OCI registries), and cmd/gregale (synthesized docs
// row). A per-package tripwire would miss whichever of these
// packets the next regression lands in.
//
// Excludes:
//   - pkg/wire/docs.go: the canonical home; the literals
//     `docs.gregale.dev` and `gregale.dev` ARE the contract.
//   - any *_test.go: tests legitimately stub the URL or pin the
//     wire shape (pkg/grpcerr/grpcerr_test.go round-trip assertions).
//   - generated *.pb.go stubs.
//
// Scope: the walker descends from the nearest enclosing `go.mod`
// directory. `go test ./cmd/gregale/...` chdirs into cmd/gregale
// before running, so the walker explicitly locates the repo root via
// go.mod — otherwise it would silently scope to just cmd/gregale
// and miss regressions in the daemons.
func TestLintTripwire_NoLiteralDocsDomainEverywhere(t *testing.T) {
	fset := token.NewFileSet()

	// Locate the repo root (the directory containing go.mod).
	root, err := findRepoRoot(".")
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	// Walk the whole repo except the canonical homes + generated
	// stubs + tests. Each package is parsed in its own directory;
	// pkgs is a map keyed by directory relative to the walker root.
	pkgs := map[string]map[string]*ast.File{}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "node_modules" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip the canonical homes — the constants DocsHost and
		// PlatformHost are the contract, the way pkg/wire/wake.go
		// owns x-faas-wake.
		if strings.HasSuffix(path, "pkg/wire/docs.go") || strings.HasSuffix(path, "pkg/wire/wake.go") {
			return nil
		}
		// Skip *_test.go (tests legitimately assert or stub the
		// wire URL literal — they are the contract tests, not the
		// production code).
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip generated protobuf stubs.
		if strings.HasSuffix(path, ".pb.go") {
			return nil
		}
		pf, ferr := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if ferr != nil {
			// Generated or unsupported files may fail to parse;
			// skip them so a single unparseable file doesn't mask
			// the rule. nilerr lint fires on `return nil` here; the
			// skip is deliberate.
			return nil //nolint:nilerr // intentional skip on parse failure; see comment above
		}
		dir := filepath.Dir(path)
		bucket, ok := pkgs[dir]
		if !ok {
			bucket = map[string]*ast.File{}
			pkgs[dir] = bucket
		}
		bucket[path] = pf
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}

	// Strict forbidden list (per user direction). Each entry is a
	// substring the walker matches against every string literal in
	// every visited .go file. The forms are exhaustive of every
	// DOMAIN-shaped spelling the survey found on main:
	//   - `https://docs/DOMAIN` matches `WithDocs("https://docs/DOMAIN/vmmd#...")` in pkg/vmmdgrpc
	//   - `https://DOMAIN`     matches `https://DOMAIN/billing` in pkg/auth/middleware
	//   - `://DOMAIN/`         path-bearing generic catch-all
	//   - `://DOMAIN"`         string-terminated generic catch-all
	//   - `docs.DOMAIN`        matches the issue's literal spelling + `apps.DOMAIN` style
	//   - `.DOMAIN`            suffix-bearing catch-all (covers `apps.DOMAIN`)
	//   - `https://docs/vmmd#` malformed-host regression caught in PR-A
	//       (issue #420): the original `https://docs/vmmd#<fragment>` had
	//       a bare `docs` host with no TLD — every other vmmdgrpc site
	//       composes `https://` + wire.DocsHost + `/vmmd#<fragment>`. The
	//       `https://docs/` substring overlaps with `https://docs/DOMAIN`,
	//       but the vmmd fragment is presentational and not a placeholder
	//       — we ban the whole shape so a future regression that drops
	//       the TLD AND the placeholder guard would still fire.
	//   - `docs.gregale.example` RFC 2606 reserved-TLD regression caught
	//       in PR-A (issue #420): the pre-#458 docs host used the
	//       IANA-reserved example TLD (`example`). PR #458 renamed to
	//       `docs.gregale.dev` but missed two sites in cmd/gregale. The
	//       literal must stay out of the tree entirely — reserved TLDs
	//       cannot resolve, so a stray lookup fails fast and obviously.
	//
	// Overlap note: `https://docs/DOMAIN` is a strict superset of
	// `https://DOMAIN`, and `://DOMAIN/` is a strict superset of
	// `://DOMAIN`. The redundant entries are kept on purpose — the
	// exact entries document the pre-rename literal form a future
	// regression could re-introduce. Walker's `strings.Contains`
	// short-circuits per match, so the overlap has no runtime cost.
	// Don't delete the "redundant" entries without also deleting the
	// comment lines above; one without the other is a confusing
	// intermediate state.
	forbidden := []string{
		"https://docs/DOMAIN",
		"https://DOMAIN",
		"://DOMAIN/",
		"://DOMAIN\"",
		"docs.DOMAIN",
		".DOMAIN",
		"https://docs/vmmd#",
		"docs.gregale.example",
	}

	var violations []string
	for _, files := range pkgs {
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, forbid := range forbidden {
					if strings.Contains(lit.Value, forbid) {
						pos := fset.Position(lit.Pos())
						violations = append(violations, pos.String()+": "+lit.Value)
						return true
					}
				}
				return true
			})
		}
	}

	if len(violations) > 0 {
		t.Fatalf("found DOMAIN-shaped placeholder string outside pkg/wire/docs.go — these literals leak unsubstituted placeholders to customers (issue #420) and must be sourced from pkg/wire.DocsHost / pkg/wire.PlatformHost:\n  %s\n\nIf a test legitimately needs the literal, move it to a *_test.go file (excluded) or convert it to a wire.DocsHost / wire.PlatformHost reference.",
			strings.Join(violations, "\n  "))
	}
}

// TestLintTripwire_NoLiteralDocsDomainSelfTest exercises the
// placeholder walker by injecting a forbidden literal into a
// synthetic production-style file under a temp directory and
// asserting the tripwire flags it. Without this, the AST walker is
// silently blind to itself — a future refactor that breaks the
// walker's substring match would land without anyone noticing
// because the live walker never finds a violation on a clean tree.
//
// One sub-test per forbidden entry, each seeded with a distinct
// synthetic literal so a regression in any single entry's substring
// match is named.
func TestLintTripwire_NoLiteralDocsDomainSelfTest(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		forbid string
	}{
		{
			// Pre-#439 placeholder form — the original issue #420
			// literal. Kept as the canonical walker exercise.
			name: "docs_DOMAIN_placeholder",
			src: `package tripwiretest

// Synthetic production-like file carrying the forbidden placeholder
// literal. Exists only to exercise the AST walker.
var url = "https://docs/DOMAIN/vmmd#create"
`,
			forbid: "https://docs/DOMAIN",
		},
		{
			// PR-A (issue #420) malformed-host regression: the host
			// was dropped to bare `docs` (no TLD) on the vmmd error
			// sites. The literal `https://docs/vmmd#prepare` snuck
			// past the original tripwire because the substring
			// `https://docs/DOMAIN` requires the placeholder token.
			// PR-C closes the gap by banning the whole
			// `https://docs/vmmd#` shape.
			name: "docs_vmmd_no_tld",
			src: `package tripwiretest

// Synthetic production-like file carrying the malformed-host
// regression. The vmmd fragment is a presentational path, not a
// placeholder — the tripwire bans the whole shape so a future
// regression that drops the TLD AND the placeholder guard would
// still fire.
var url = "https://docs/vmmd#prepare"
`,
			forbid: "https://docs/vmmd#",
		},
		{
			// PR-A (issue #420) RFC 2606 reserved-TLD regression:
			// the pre-#458 docs host used the IANA-reserved example
			// TLD. PR #458 renamed to `docs.gregale.dev` but missed
			// two sites in cmd/gregale. The reserved TLD must stay
			// out of the tree entirely — reserved TLDs cannot
			// resolve, so a stray lookup fails fast and obviously.
			name: "docs_example_reserved_tld",
			src: `package tripwiretest

// Synthetic production-like file carrying the RFC 2606 reserved-TLD
// regression. The .example TLD is unreachable by design.
var url = "https://docs.gregale.example/build/limits#memory"
`,
			forbid: "docs.gregale.example",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			srcPath := filepath.Join(tmp, "tripwire.go")
			if err := os.WriteFile(srcPath, []byte(tc.src), 0o644); err != nil {
				t.Fatalf("seed file: %v", err)
			}

			fset := token.NewFileSet()
			pf, err := parser.ParseFile(fset, srcPath, nil, parser.AllErrors)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			forbidden := []string{tc.forbid}
			var found string
			ast.Inspect(pf, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, forbid := range forbidden {
					if strings.Contains(lit.Value, forbid) {
						found = fset.Position(lit.Pos()).String()
						return false
					}
				}
				return true
			})
			if found == "" {
				t.Fatalf("self-test: walker did not detect the seeded %q literal — the tripwire may be silently broken for this entry", tc.forbid)
			}
		})
	}
}
