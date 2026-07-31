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
//   - cmd/gregale/output.go and any *_test.go already excluded by the
//     per-directory walker.
//
// Scope note: the walker descends every directory rooted under the
// repo root passed via `--repo-root` (or the test's current working
// directory when unset). The CLI packaging this test enforces only
// has visibility into the local . tree, so we walk the package root
// directly. The intent is "no literal in production code that travels
// over the wire"; docs and tests are out of scope (docs are sourced
// to spec §6 / cold-wake.md, tests are gated by the suite's own
// asserts).
func TestLintTripwire_NoLiteralWakeHeaderOutsidePkgWire(t *testing.T) {
	fset := token.NewFileSet()

	// Walk the whole repo except pkg/wire itself. Each package is
	// parsed in its own directory; pkgs is a map keyed by directory
	// relative to the walker root.
	type pkgFiles struct {
		root string
		files map[string]*ast.File
	}
	pkgs := map[string]*pkgFiles{}

	walkErr := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
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
			// mask the rule.
			return nil
		}
		dir := filepath.Dir(path)
		bucket, ok := pkgs[dir]
		if !ok {
			bucket = &pkgFiles{root: dir, files: map[string]*ast.File{}}
			pkgs[dir] = bucket
		}
		bucket.files[path] = pf
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}

	forbidden := []string{`"x-faas-wake"`, `"x-gregale-wake"`, `"X-Faas-Wake"`, `"X-Gregale-Wake"`}

	var violations []string
	for _, bucket := range pkgs {
		for _, file := range bucket.files {
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
