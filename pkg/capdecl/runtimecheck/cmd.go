// Package runtimecheck, cmd.go — the boot-time check helper
// each daemon's main.go calls after flag parsing. Centralises
// the slog/SetDefault/log-fail plumbing so every daemon's
// startup path is the same shape:
//
//	import (
//	    "github.com/onebox-faas/faas/pkg/capdecl"
//	    "github.com/onebox-faas/faas/pkg/capdecl/runtimecheck"
//	)
//	...
//	if err := runtimecheck.MustCheckOnBoot(capsDecl, log, nil); err != nil {
//	    // MustCheckOnBoot already logs + exits on failure;
//	    // returning here is for the success path's possible
//	    // warning surfacing.
//	    _ = err
//	}
//
// The check is silent on success (no log line — every boot
// would otherwise spam the journal with a known-good message)
// and loud on failure (a single Error line + non-zero exit).
// A future PR can wire `--strict-caps` flag to surface the
// "validated" log line for ops to grep; today the absence of
// the error line is the success signal.
package runtimecheck

import (
	"log/slog"
	"os"

	"github.com/onebox-faas/faas/pkg/capdecl"
)

// MustCheckOnBoot is the boot-time gate. On any capdecl
// violation the function logs a structured error line and
// calls os.Exit(1). On success it returns nil and the daemon
// continues its startup path.
//
// The function exists so each daemon's main.go has exactly
// one line of capdecl wiring — and a future code-shape
// reviewer can grep cmd/<daemon>/main.go for
// runtimecheck.MustCheckOnBoot to find every daemon that
// enforces the contract.
//
// opts may be nil (defaults to Options{}); pass
// &Options{StatusReader: ...} for tests that feed a fixture
// /proc/<pid>/status.
func MustCheckOnBoot(decl capdecl.Declaration, log *slog.Logger, opts *Options) error {
	if log == nil {
		log = slog.Default()
	}
	var o Options
	if opts != nil {
		o = *opts
	}
	if err := Check(decl, o); err != nil {
		log.Error("capdecl: boot check failed; refusing to start",
			"err", err,
			"declaration", decl.String(),
		)
		os.Exit(1)
	}
	return nil
}
