// json_parity_test.go — operator-side Tier A8.2 JSON-flag parity gate
// (issue #911 / ADR-110 PR-6.5).
//
// Mirrors cmd/gregale/json_parity_test.go byte-for-byte except the
// `nonJSONAllowList` is restricted to operator-side dispatchers:
// cmdBackup, cmdHostAge, cmdPKI, cmdSignKeys, cmdNodeKey. The
// customer-side list (cmdAccount, cmdInit, cmdLogin, cmdLogout,
// cmdMfa, cmdOverageCap, cmdRestore, cmdWhoami, cmdTrustedPublishers)
// stays in cmd/gregale/json_parity_test.go — moving them here would
// invert the gate.
//
// Drift-test for cliCommands ↔ main.go lives in
// commands_completion_test.go::TestCompletion_ManifestDrift.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// nonJSONAllowList is the closed set of operator-side cmdXxx funcs
// that DELIBERATELY emit no JSON. Keep entries alphabetical.
var nonJSONAllowList = map[string]string{
	"cmdBackup":   "operator fs writes",
	"cmdHostAge":  "operator fs writes",
	"cmdPKI":      "operator fs writes",
	"cmdSignKeys": "operator fs writes",
	"cmdNodeKey":  "operator fs writes",
}

// TestJSONOutputHonored is the parity gate. Fails loudly when a
// top-level cmdXxx references jsonOutput (or any of its leaves
// does) but no test exercises that branch.
func TestJSONOutputHonored(t *testing.T) {
	jsonCmds, err := collectJSONEmitters()
	if err != nil {
		t.Fatalf("walk jsonOutput emitters: %v", err)
	}
	if len(jsonCmds) == 0 {
		t.Fatal("no jsonOutput emitters found — extractor is broken")
	}

	topLevel := map[string]bool{}
	for _, name := range jsonCmds {
		top := topLevelDispatcher(name)
		if top != "" {
			topLevel[top] = true
		}
	}

	tested := jsonTestedTopLevel()
	if len(tested) == 0 {
		t.Fatal("no jsonOutput = true assignments found — extractor is broken")
	}

	for c := range topLevel {
		if _, isAllowlisted := nonJSONAllowList[c]; isAllowlisted {
			continue
		}
		if !tested[c] {
			t.Errorf("top-level dispatcher %q emits JSON (or has JSON-emitting leaves) but no test sets jsonOutput = true for it; add a test or move to nonJSONAllowList with a rationale comment", c)
		}
	}
}

func topLevelDispatcher(leaf string) string {
	best := ""
	for _, c := range cliCommands {
		if !strings.HasPrefix(leaf, c.Name) {
			continue
		}
		if len(c.Name) > len(best) {
			best = c.Name
		}
	}
	return best
}

// collectJSONEmitters walks every non-test .go file in the
// current package directory and returns the set of func cmdXxx
// names whose body references the package-level jsonOutput
// identifier. Mirrors cmd/gregale/json_parity_test.go:111.
func collectJSONEmitters() ([]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var emitters []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if !strings.HasPrefix(fn.Name.Name, "cmd") {
				return true
			}
			if fn.Body == nil {
				return true
			}
			hasJSON := false
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				id, ok := inner.(*ast.Ident)
				if !ok {
					return true
				}
				// Operator-side dispatchers reference jsonOutput
				// either directly OR through the jsonEnabled()
				// wrapper (commands_manifest.go:211). Both signal
				// "this dispatcher honours --json".
				if id.Name == "jsonOutput" || id.Name == "jsonEnabled" {
					hasJSON = true
					return false
				}
				return true
			})
			if hasJSON {
				emitters = append(emitters, fn.Name.Name)
			}
			return true
		})
	}
	return emitters, nil
}

// jsonTestedTopLevel walks every _test.go file and maps every
// jsonOutput reference to the enclosing top-level cmdXxx
// dispatcher. Mirrors cmd/gregale/json_parity_test.go:163.
func jsonTestedTopLevel() map[string]bool {
	tested := map[string]bool{}
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		if err != nil {
			continue
		}
		type tfunc struct {
			name  string
			start token.Pos
		}
		var tests []tfunc
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if !strings.HasPrefix(fn.Name.Name, "Test") {
				return true
			}
			rest := fn.Name.Name[4:]
			if !strings.HasPrefix(rest, "Cmd") {
				return true
			}
			rest = strings.TrimPrefix(rest, "Cmd")
			if idx := strings.Index(rest, "_"); idx >= 0 {
				rest = rest[:idx]
			}
			tests = append(tests, tfunc{name: rest, start: fn.Body.Pos()})
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || id.Name != "jsonOutput" {
				return true
			}
			for _, tf := range tests {
				if tf.start <= id.Pos() {
					full := "cmd" + tf.name
					top := topLevelDispatcher(full)
					if top == "" {
						top = full
					}
					tested[top] = true
					break
				}
			}
			return true
		})
	}
	return tested
}
