// completion_test.go — coverage pass for cmd/gregalectl completion
// scripts (Cluster 1 of the gregalectl coverage depth-pass, follow-on
// to PR #1044). Mirrors the file-local capture-helper convention at
// commands_pki_test.go:46-51 and commands_compute_nodes_test.go.
//
// Pinned contracts:
//   - cmdCompletionBash emits a `complete -F __gregale gregale` trailer
//   - cmdCompletionPowershell emits a `Register-ArgumentCompleter`
//     registration on `gregale`
//   - cmdCompletionZsh emits a `#compdef gregale` autoload directive
//   - cmdCompletionFish emits at least one `complete -c gregale`
//     registration per cliCommand
//   - cmdCompletion routes "bash|zsh|fish|powershell" to the matching
//     renderer and exits 0 on each branch; exits 1 on unknown subcommand
//   - cachePathForScripts returns a non-empty absolute path under the
//     per-user config dir (via api.NewCompletionCache().Path())
//   - cmdCompletionCacheList returns 0 with no output (operator stub)
package main

import (
	"os"
	"strings"
	"testing"
)

type completionBuffer struct{ data []byte }

func (b *completionBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *completionBuffer) Bytes() []byte  { return b.data }
func (b *completionBuffer) String() string { return string(b.data) }

func captureOsStdoutCompletion(t *testing.T) (*completionBuffer, func()) {
	t.Helper()
	old := osStdout
	buf := &completionBuffer{}
	osStdout = buf
	return buf, func() { osStdout = old }
}

// captureStderrRun runs fn with the REAL os.Stderr file descriptor
// redirected to a buffer and returns the captured string. Mirrors
// the precedent at commands_release_sbom_gate_test.go:104-123.
// Used for error-path assertions where the source code writes to
// os.Stderr (the real fd) rather than the osStderr package var.
func captureStderrRun(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	_ = r.Close()
	return string(buf)
}

// TestCmdCompletionBash_HappyPath pins the bash completion script
// shape: header (cache path + slug helper + completion function),
// per-command dispatch block, footer with top-level fallback +
// `complete -F __gregale gregale` registration.
func TestCmdCompletionBash_HappyPath(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()

	if code := cmdCompletionBash(); code != 0 {
		t.Fatalf("cmdCompletionBash() = %d, want 0", code)
	}
	s := out.String()
	wants := []string{
		"# bash completion for gregale",
		"__gregale_cache_path",
		"__gregale_cache_slugs",
		"__gregale() {",
		"complete -F __gregale gregale",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("bash script missing %q", w)
		}
	}
}

// TestCmdCompletionBash_EmitsAllCommands asserts every top-level
// cliCommand shows up in the rendered dispatch blocks (one
// `if [ "$cmd" = %q ]; then` block per command).
func TestCmdCompletionBash_EmitsAllCommands(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()

	if code := cmdCompletionBash(); code != 0 {
		t.Fatalf("cmdCompletionBash() = %d, want 0", code)
	}
	s := out.String()
	for _, c := range cliCommands {
		want := `if [ "$cmd" = "` + c.Name + `"`
		if !strings.Contains(s, want) {
			t.Errorf("bash script missing dispatch for command %q (wanted %q)", c.Name, want)
		}
	}
}

// TestCmdCompletionZsh_HappyPath pins the zsh script: #compdef
// directive + _gregale function + _describe dispatcher + the
// final `_gregale "$@"` invocation.
func TestCmdCompletionZsh_HappyPath(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()

	if code := cmdCompletionZsh(); code != 0 {
		t.Fatalf("cmdCompletionZsh() = %d, want 0", code)
	}
	s := out.String()
	wants := []string{
		"#compdef gregale",
		"# zsh completion for gregale",
		"_gregale_cache_slugs",
		"_gregale() {",
		"_describe 'command' commands",
		"_gregale \"$@\"",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("zsh script missing %q", w)
		}
	}
}

// TestCmdCompletionZsh_EscapesApostrophes covers the escapeZshDQ
// branch — a description containing a single quote must be
// rendered verbatim (NOT escaped) so _arguments parses cleanly.
// This pins the documented contract at completion_zsh.go:18-25.
func TestCmdCompletionZsh_EscapesApostrophes(t *testing.T) {
	// Drive escapeZshDQ directly via the renderBash path-equivalent:
	// call the function with an input containing the four special
	// bytes (backslash, dquote, dollar, backtick) and assert they
	// are escaped while apostrophes pass through.
	got := escapeZshDQ("webhook's url: $100 \\path \"x\"")
	wants := []string{
		`\$100`,  // dollar escaped
		`\\path`, // backslash escaped
		`\"x\"`,  // double-quote escaped
		`'`,      // apostrophe NOT escaped
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("escapeZshDQ(input) missing %q (got %q)", w, got)
		}
	}
	// Apostrophe MUST pass through unescaped — the literal "webhook's"
	// substring is preserved exactly.
	if !strings.Contains(got, "webhook's") {
		t.Errorf("escapeZshDQ mangled apostrophe: got %q", got)
	}
}

// TestCmdCompletionFish_HappyPath pins the fish script: cache
// helper + one `complete -c gregale` per cliCommand. fish has no
// footer (renderFishFooter is a no-op, completion_fish.go:86).
func TestCmdCompletionFish_HappyPath(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()

	if code := cmdCompletionFish(); code != 0 {
		t.Fatalf("cmdCompletionFish() = %d, want 0", code)
	}
	s := out.String()
	wants := []string{
		"# fish completion for gregale",
		"function __gregale_cache_slugs",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("fish script missing %q", w)
		}
	}
	// Each top-level command should produce at least one
	// `complete -c gregale ... -a "<name>"` registration.
	for _, c := range cliCommands {
		needle := `complete -c gregale -f -n "__fish_use_subcommand" -a "` + c.Name + `"`
		if !strings.Contains(s, needle) {
			t.Errorf("fish script missing top-level registration for %q", c.Name)
		}
	}
}

// TestCmdCompletionPowershell_HappyPath pins the powershell script:
// Register-ArgumentCompleter directive + per-command switch arms
// + per-subcommand CompletionResult entries.
func TestCmdCompletionPowershell_HappyPath(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()

	if code := cmdCompletionPowershell(); code != 0 {
		t.Fatalf("cmdCompletionPowershell() = %d, want 0", code)
	}
	s := out.String()
	wants := []string{
		"# powershell completion for gregale",
		"function __gregaleCacheSlugs",
		"Register-ArgumentCompleter -Native -CommandName 'gregale'",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("powershell script missing %q", w)
		}
	}
	// Every top-level command should produce an `$tokens[1] -eq '<name>'`
	// switch arm so the completer walks the manifest at TAB time.
	for _, c := range cliCommands {
		needle := `$tokens[1] -eq '` + c.Name + `'`
		if !strings.Contains(s, needle) {
			t.Errorf("powershell script missing switch arm for %q", c.Name)
		}
	}
}

// TestCmdCompletionPowershell_EscapePS pins the single-quote
// escape doubling (completion_powershell.go:103-107). A description
// containing a literal apostrophe must double it so the PS
// single-quoted string remains a single shell token.
func TestCmdCompletionPowershell_EscapePS(t *testing.T) {
	if got := escapePS("webhook's url"); got != "webhook''s url" {
		t.Errorf("escapePS(apostrophe) = %q, want %q", got, "webhook''s url")
	}
	if got := escapePS("plain text"); got != "plain text" {
		t.Errorf("escapePS(plain) = %q, want unchanged", got)
	}
	if got := escapePS("a'b'c"); got != "a''b''c" {
		t.Errorf("escapePS(multiple) = %q, want %q", got, "a''b''c")
	}
}

// TestCmdCompletion_Dispatch pins the dispatcher routing in
// completion.go::cmdCompletion — every known shell routes to the
// matching renderer (exit 0); the hidden completion-cache-path
// subcommand prints a path; unknown subcommand returns exit 1 with
// a load-bearing-token error.
func TestCmdCompletion_Dispatch(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantCode  int
		wantInOut string // substring expected in captured stdout/stderr
	}{
		{name: "bash", args: []string{"bash"}, wantCode: 0, wantInOut: "complete -F __gregale gregale"},
		{name: "zsh", args: []string{"zsh"}, wantCode: 0, wantInOut: "#compdef gregale"},
		{name: "fish", args: []string{"fish"}, wantCode: 0, wantInOut: "function __gregale_cache_slugs"},
		{name: "powershell", args: []string{"powershell"}, wantCode: 0, wantInOut: "Register-ArgumentCompleter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, restore := captureOsStdoutCompletion(t)
			defer restore()
			if code := cmdCompletion(tc.args); code != tc.wantCode {
				t.Errorf("cmdCompletion(%v) = %d, want %d (out: %q)", tc.args, code, tc.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tc.wantInOut) {
				t.Errorf("cmdCompletion(%v) output missing %q (got %q)", tc.args, tc.wantInOut, out.String())
			}
		})
	}
}

// TestCmdCompletion_UnknownSubcommand asserts the unknown-shell
// branch in cmdCompletion (completion.go:79-82) returns exit 1 and
// prints a diagnostic containing the load-bearing token
// "unknown subcommand".
func TestCmdCompletion_UnknownSubcommand(t *testing.T) {
	var code int
	stderr := captureStderrRun(t, func() {
		code = cmdCompletion([]string{"tcsh"})
	})
	if code != 1 {
		t.Errorf("cmdCompletion(tcsh) = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' in stderr, got %q", stderr)
	}
}

// TestCmdCompletion_NoArgs asserts the empty-args branch prints
// usage (completion.go:56-58) and exits non-zero.
func TestCmdCompletion_NoArgs(t *testing.T) {
	var code int
	stderr := captureStderrRun(t, func() {
		code = cmdCompletion(nil)
	})
	if code == 0 {
		t.Errorf("cmdCompletion(nil) = 0, want non-zero")
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("expected usage message in stderr, got %q", stderr)
	}
}

// TestCmdCompletion_CachePathAndList pins the two hidden
// subcommands used by the shell completion functions at TAB time:
// completion-cache-path (prints a path) and completion-cache-list
// (operator stub: exit 0, no output).
func TestCmdCompletion_CachePathAndList(t *testing.T) {
	out, restore := captureOsStdoutCompletion(t)
	defer restore()
	if code := cmdCompletion([]string{"completion-cache-path"}); code != 0 {
		t.Errorf("cmdCompletion(completion-cache-path) = %d, want 0", code)
	}
	if out.String() == "" {
		t.Errorf("completion-cache-path produced no output, want a path")
	}

	out, restore = captureOsStdoutCompletion(t)
	defer restore()
	if code := cmdCompletion([]string{"completion-cache-list", "apps"}); code != 0 {
		t.Errorf("cmdCompletion(completion-cache-list apps) = %d, want 0", code)
	}
	// Operator stub: cmdCompletionCacheList returns 0 with no output
	// because gregalectl has no apps/orgs surface (completion.go:91-99).
	if out.String() != "" {
		t.Errorf("completion-cache-list emitted output %q, want empty (operator stub)", out.String())
	}
}

// TestCmdCompletion_CacheListMissingKind pins the missing-arg
// branch of cmdCompletion (completion.go:72-77) — calling
// completion-cache-list with no <kind> returns exit 1 and emits
// a diagnostic.
func TestCmdCompletion_CacheListMissingKind(t *testing.T) {
	var code int
	stderr := captureStderrRun(t, func() {
		code = cmdCompletion([]string{"completion-cache-list"})
	})
	if code != 1 {
		t.Errorf("cmdCompletion(completion-cache-list) = %d, want 1", code)
	}
	if !strings.Contains(stderr, "missing") {
		t.Errorf("expected 'missing' in diagnostic, got %q", stderr)
	}
}

// TestCachePathForScripts pins the helper that all four completion
// scripts invoke at TAB time — it must return a non-empty path
// (via api.NewCompletionCache().Path()).
func TestCachePathForScripts(t *testing.T) {
	got := cachePathForScripts()
	if got == "" {
		t.Errorf("cachePathForScripts() = \"\", want non-empty")
	}
	if !strings.HasPrefix(got, "/") {
		t.Errorf("cachePathForScripts() = %q, want absolute path", got)
	}
}
