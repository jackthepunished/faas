// StrictSecretScanError is the typed error returned by the pack / env-push
// paths when --secret-scan=strict fires. It carries:
//
//   - Findings: every secretscan.Finding produced by both the .env pass
//     and the (optional) source-tree pass. The CLI doesn't surface only
//     the first finding — a customer who copies a Stripe key into two
//     files deserves to see both in one pass so they can fix both before
//     re-running.
//
//   - Hint: a one-line customer-facing nudge pointing at the canonical
//     remediation path. Same role as the Hint field on
//     NestedMarkerHintError (pack.go:564-573); the printErr dispatch
//     surfaces it under the JSON 422 envelope's `extra.hint` so
//     programmatic consumers can render their own UI without parsing
//     the free-form message.
//
//   - Err: the wrapped underlying error. Always nil today (the strict
//     mode short-circuits with a typed sentinel rather than a wrapped
//     scanner error), but the field is here for forward-compat with a
//     future "strict-fail-because-scanner-errored" path that should
//     still emit the same envelope shape.
//
// The error implements `errors.Is`/`errors.As` via Unwrap so callers can
// route on it without a type switch. The printErr dispatcher in
// commands.go handles the rendering; the wrapping itself stays here.
package main

import (
	"fmt"

	"github.com/onebox-faas/faas/pkg/secretscan"
)

// StrictSecretScanError is returned by scanAndRedactEnvFiles (pack.go)
// when mode == modeStrict and at least one Finding was produced. Mirrors
// the NestedMarkerHintError shape (Findings ↔ Dir, Hint ↔ Hint, Err ↔
// Err) so the printErr dispatch is a near-copy.
type StrictSecretScanError struct {
	Findings []secretscan.Finding
	Hint     string
	Err      error
}

// Error renders a single human-readable line. The full per-finding list
// is rendered by printErr via the errors.As path; this message is the
// one-shot summary shown when a non-JSON consumer runs `gregale deploy
// --secret-scan=strict` and the stdout is plain text.
func (e *StrictSecretScanError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("strict secret scan rejected the deploy: %d finding(s) — see hint (%s)",
		len(e.Findings), e.Hint)
}

// Unwrap returns the wrapped underlying error. Returns nil if Err was
// not set (today always).
func (e *StrictSecretScanError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
