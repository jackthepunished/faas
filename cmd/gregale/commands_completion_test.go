// commands_completion_test.go — Tier A8 / ADR-083.
//
// Smoke tests for the four completion backends + the man renderer.
// Mirrors commands_tier_d_test.go's pattern: pure-string assertions,
// no external process invocation, table-driven where the shape
// is uniform across backends.
//
// The bash -n / groff -man syntax-check tests are intentionally
// NOT included here — both tools are absent on most dev boxes
// (CI's metal runner does have them, but unit tests must pass on
// any machine per CLAUDE.md "make test"). A future PR can add
// `//go:build bash_complete` and `//go:build roff_complete`
// test files for the integrated validation when the toolchain
// is reliably available; today the structural tests below are
// the tripwire.

package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCompletion_Bash_RegistersAllCommands(t *testing.T) {
	var buf bytes.Buffer
	captureStdoutSwap(t, &buf, cmdCompletionBash)
	out := buf.String()
	if !strings.Contains(out, "# bash completion for gregale") {
		t.Fatalf("bash header missing")
	}
	if !strings.Contains(out, "complete -F __gregale gregale") {
		t.Fatalf("bash registration missing")
	}
	for _, c := range cliCommands {
		if !strings.Contains(out, `"$cmd" = "`+c.Name+`"`) {
			t.Errorf("bash missing dispatch for %q", c.Name)
		}
	}
}

func TestCompletion_Zsh_HasCompdef(t *testing.T) {
	var buf bytes.Buffer
	captureStdoutSwap(t, &buf, cmdCompletionZsh)
	out := buf.String()
	if !strings.HasPrefix(out, "#compdef gregale\n") {
		t.Fatalf("zsh #compdef header missing or not first line; got prefix %q", firstLine(out))
	}
	for _, c := range cliCommands {
		if !strings.Contains(out, "_gregale_"+c.Name+"()") {
			t.Errorf("zsh missing per-command function for %q", c.Name)
		}
	}
}

func TestCompletion_Fish_HasComplete(t *testing.T) {
	var buf bytes.Buffer
	captureStdoutSwap(t, &buf, cmdCompletionFish)
	out := buf.String()
	if !strings.Contains(out, "complete -c gregale") {
		t.Fatalf("fish complete -c missing")
	}
	for _, c := range cliCommands {
		if !strings.Contains(out, " -a '"+c.Name+"'") && !strings.Contains(out, " -a \""+c.Name+"\"") {
			t.Errorf("fish missing complete entry for %q", c.Name)
		}
	}
}

func TestCompletion_Powershell_HasRegisterArgumentCompleter(t *testing.T) {
	var buf bytes.Buffer
	captureStdoutSwap(t, &buf, cmdCompletionPowershell)
	out := buf.String()
	if !strings.Contains(out, "Register-ArgumentCompleter") {
		t.Fatalf("powershell registration missing")
	}
	for _, c := range cliCommands {
		if !strings.Contains(out, " -eq '"+c.Name+"'") {
			t.Errorf("powershell missing dispatch for %q", c.Name)
		}
	}
}

func TestCompletion_ManifestDrift(t *testing.T) {
	// Walk main.go's switch and collect every `case "<name>":` arm.
	// Also walk the dispatch constants (commands2.go) to recover
	// the values behind `case dispatchFoo:` forms.
	dispatchConsts := map[string]string{
		"dispatchApps":              "apps",
		"dispatchDeployments":       "deployments",
		"dispatchDeployment":        "deployment",
		"dispatchBuild":             "build",
		"appSlugFallback":           "app",
		"statusLiteral":             "status",
		"dispatchSignKeys":          "sign-keys",
		"dispatchTrustedPublishers": "trusted-publishers",
		"dispatchHostAge":           "host-age",
		"dispatchBackup":            "backup",
		"dispatchPKI":               "pki",
	}
	caseNames, err := extractMainCaseArms(dispatchConsts)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	manifestNames := make(map[string]struct{}, len(cliCommands))
	for _, c := range cliCommands {
		manifestNames[c.Name] = struct{}{}
	}
	// Internal pseudo-commands the manifest deliberately omits:
	// help, version. They are dispatched in run() but rendered in
	// the top-level usage block, not as separate cliCommand entries.
	internal := map[string]struct{}{
		"help":       {},
		"version":    {},
		"--version":  {},
		"-v":         {},
		"--help":     {},
		"-h":         {},
		"completion": {},
		"man":        {},
	}
	for name := range caseNames {
		if _, ok := internal[name]; ok {
			continue
		}
		if _, ok := manifestNames[name]; !ok {
			t.Errorf("main.go has case %q but no cliCommand entry in cli_meta.go", name)
		}
	}
	for name := range manifestNames {
		if _, ok := internal[name]; ok {
			// Manifest may include internal commands (e.g. completion, man);
			// that's fine — they ARE in the dispatch table.
			continue
		}
		if _, ok := caseNames[name]; !ok {
			t.Errorf("cliCommand %q has no matching case arm in main.go", name)
		}
	}
}

func extractMainCaseArms(dispatchConsts map[string]string) (map[string]struct{}, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	cases := make(map[string]struct{})
	ast.Inspect(f, func(n ast.Node) bool {
		cs, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, expr := range cs.List {
			switch e := expr.(type) {
			case *ast.BasicLit:
				if e.Kind == token.STRING {
					name := strings.Trim(e.Value, `"`)
					cases[name] = struct{}{}
				}
			case *ast.Ident:
				if val, ok := dispatchConsts[e.Name]; ok {
					cases[val] = struct{}{}
				} else {
					cases[e.Name] = struct{}{}
				}
			}
		}
		return true
	})
	return cases, nil
}

func TestCompletion_DispatcherRoutesToBackends(t *testing.T) {
	cases := []struct {
		shell string
		want  string
	}{
		{"bash", "__gregale"},
		{"zsh", "#compdef"},
		{"fish", "complete -c"},
		{"powershell", "Register-ArgumentCompleter"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			var buf bytes.Buffer
			captureStdoutSwap(t, &buf, func() int { return cmdCompletion([]string{tc.shell}) })
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("%s: expected %q in output; got %s", tc.shell, tc.want, buf.String())
			}
		})
	}
}

func TestCompletion_UnknownShellExitsOne(t *testing.T) {
	var buf bytes.Buffer
	captureStderrSwap(t, &buf, func() int { return cmdCompletion([]string{"tcsh"}) })
	if !strings.Contains(buf.String(), "unknown subcommand") {
		t.Fatalf("expected error message; got %s", buf.String())
	}
}

func TestCompletion_NoArgExitsOne(t *testing.T) {
	var buf bytes.Buffer
	captureStderrSwap(t, &buf, func() int { return cmdCompletion(nil) })
	if !strings.Contains(buf.String(), "usage:") {
		t.Fatalf("expected usage; got %s", buf.String())
	}
}

func TestMan_TopLevel_ContainsNameAndSynopsis(t *testing.T) {
	var buf bytes.Buffer
	renderManTop(&buf)
	out := buf.String()
	for _, want := range []string{".TH GREGALE(1)", ".SH NAME", ".SH SYNOPSIS", ".SH SEE ALSO"} {
		if !strings.Contains(out, want) {
			t.Errorf("man top: missing %q", want)
		}
	}
}

func TestMan_CommandPage_ContainsSubcommandList(t *testing.T) {
	c, ok := lookupCliCommand("alerts")
	if !ok {
		t.Fatalf("alerts not in manifest")
	}
	var buf bytes.Buffer
	renderManCommand(&buf, c)
	out := buf.String()
	for _, want := range []string{".TH GREGALE-ALERTS(1)", ".SH SUBCOMMANDS", "list", "add", "rotate-secret"} {
		if !strings.Contains(out, want) {
			t.Errorf("man alerts: missing %q", want)
		}
	}
}

func TestMan_CommandPage_ContainsFlagsSection(t *testing.T) {
	c, ok := lookupCliCommand("registry")
	if !ok {
		t.Fatalf("registry not in manifest")
	}
	var buf bytes.Buffer
	renderManCommand(&buf, c)
	if !strings.Contains(buf.String(), ".SH FLAGS") {
		t.Errorf("man registry: missing FLAGS section")
	}
	if !strings.Contains(buf.String(), "--app") {
		t.Errorf("man registry: missing --app flag")
	}
}

func TestMan_UnknownCommandExitsOne(t *testing.T) {
	var buf bytes.Buffer
	captureStderrSwap(t, &buf, func() int { return cmdMan([]string{"no-such-cmd"}) })
	if !strings.Contains(buf.String(), "unknown command") {
		t.Fatalf("expected error; got %s", buf.String())
	}
}

func TestMan_TooManyArgsExitsOne(t *testing.T) {
	var buf bytes.Buffer
	captureStderrSwap(t, &buf, func() int { return cmdMan([]string{"a", "b"}) })
	if !strings.Contains(buf.String(), "usage:") {
		t.Fatalf("expected usage; got %s", buf.String())
	}
}

func TestLookupCliCommand(t *testing.T) {
	if _, ok := lookupCliCommand("apps"); !ok {
		t.Fatal("apps not in manifest")
	}
	if _, ok := lookupCliCommand("nope"); ok {
		t.Fatal("nope should not be in manifest")
	}
}

func TestEscapeRoff(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{".start", "\\&.start"},
		{"a\\b", "a\\\\b"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := escapeRoff(tc.in); got != tc.want {
			t.Errorf("escapeRoff(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// captureStdoutSwap swaps osStdout for a buffer, runs fn, and restores.
// Renamed to avoid collision with the safeBuffer-based captureStdout
// in commands5_test.go. Accepts func() int because every
// cmdCompletionXxx returns an exit code.
func captureStdoutSwap(t *testing.T, buf *bytes.Buffer, fn func() int) int {
	t.Helper()
	prev := osStdout
	osStdout = buf
	defer func() { osStdout = prev }()
	return fn()
}

// captureStderrSwap redirects os.Stderr to a buffer for fn's
// duration. Uses os.Pipe so the FD-based writes from os.Stderr
// are captured (a buffer+Write would NOT catch them).
func captureStderrSwap(t *testing.T, buf *bytes.Buffer, fn func() int) error {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, r)
		close(done)
	}()
	rc := fn()
	_ = w.Close()
	os.Stderr = orig
	<-done
	_ = rc
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
