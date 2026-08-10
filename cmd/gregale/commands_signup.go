// gregale signup [--email-only EMAIL] (issue #311, UX §2.2).
//
// Two modes:
//
//  1. Interactive (default): prompts for email + password + confirm,
//     calls POST /v1/auth/signup, saves the freshly-minted API key
//     via saveToken(), and prints the first-run quickstart. Same
//     exit-state as `gregale login`.
//
//  2. --email-only EMAIL: calls POST /v1/auth/signup/magic-link and
//     prints "Check your email". Symmetric with the web magic-link
//     flow; no token is minted on the CLI side.
//
// The interactive path is one round-trip — the server returns the
// api_key payload in the signup response, so the CLI never has to
// follow up with a `/login` call. Idempotent on (email, password):
// a re-signup with the same credentials returns a fresh key.

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/auth"
)

const dispatchSignup = "signup"

// cmdSignup is the dispatch entry point. Mirrors the `dispatchLogin`
// pattern: a flag.NewFlagSet, positional-arg count check, then route
// to the interactive or magic-link helper.
func cmdSignup(args []string) int {
	fs := flag.NewFlagSet("signup", flag.ContinueOnError)
	fs.Usage = func() {
		PrintUsage(os.Stderr, "usage: gregale signup [--email-only EMAIL]", "auth")
	}
	emailOnly := fs.String("email-only", "", "send a one-time signup link to this email (no password prompt)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		PrintUsage(os.Stderr, "usage: gregale signup [--email-only EMAIL]", "auth")
		return 1
	}
	if *emailOnly != "" {
		return signupMagicLink(*emailOnly)
	}
	return signupInteractive()
}

// signupInteractive is the default path. Stdin-driven: email, password,
// confirm. Mis-matched passwords or a weak password short-circuit
// before any HTTP round-trip (kept local for honesty — the server
// would reject anyway with the same error).
func signupInteractive() int {
	br := bufio.NewReader(osStdin)

	emailRaw, err := br.ReadString('\n')
	if err != nil {
		return printErr("Could not read email", err)
	}
	email := strings.ToLower(strings.TrimSpace(emailRaw))
	if !looksLikeCLIEmail(email) {
		return printErr("Invalid email", fmt.Errorf("email must look like local@domain.tld"))
	}

	pw1, err := readPasswordLineFrom(br)
	if err != nil {
		return printErr("Could not read password", err)
	}
	if err := auth.Validate(pw1); err != nil {
		return printErr("Password too weak", err)
	}

	pw2, err := readPasswordLineFrom(br)
	if err != nil {
		return printErr("Could not read confirm", err)
	}
	if pw1 != pw2 {
		return printErr("Passwords do not match", fmt.Errorf("the two passwords differ"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewClient(apiBase(), "")
	resp, err := c.PostAuthSignup(ctx, email, pw1)
	if err != nil {
		return printErr("Signup failed", err)
	}
	return finalizeLogin(ctx, c, resp.APIKey.Plaintext, api.AccountResponse{
		ID:    resp.AccountID,
		Email: resp.Email,
		Plan:  resp.Plan,
	})
}

// signupMagicLink is the no-password path. The server always returns
// 200 with the same body regardless of whether the email is bound,
// unbound, malformed, or missing — so the CLI just prints the link
// guidance and exits 0. Anti-enumeration closure lives on the server.
func signupMagicLink(email string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewClient(apiBase(), "")
	if err := c.PostAuthSignupMagicLink(ctx, email); err != nil {
		return printErr("Could not send magic link", err)
	}
	_, _ = fmt.Fprintln(osStdout, "Check your email — a one-time signup link is on the way.")
	return 0
}

// looksLikeCLIEmail is a deliberately frugal client-side check.
// `local@domain.tld` with at least one dot in the domain; this is
// NOT a substitute for the server's full validation, just a
// front-of-house guard so the CLI can render "Invalid email"
// before any HTTP round-trip on a typo.
func looksLikeCLIEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return strings.IndexByte(s[at+1:], '.') >= 0
}

// readPasswordLineFrom reads one line from the supplied reader and
// trims the trailing newline. We pass the reader in (rather than
// creating a fresh bufio.NewReader(osStdin) on every call) because
// the pipe-backed test seam only writes once to the pipe — a
// second reader sees EOF instead of the next line. The CLI
// intentionally does NOT switch to echo-off mode here — the typed
// password is visible in the terminal during a local signup.
// Silent-echo support is a separate UX polish (item G10 in the
// issue #311 plan).
func readPasswordLineFrom(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
