// Cert-expiry refresher for the gateway_tls_cert_expiry_seconds gauge
// (ADR-024 H3, spec §12). certmagic v0.25.4's Cache keeps full iteration
// private (cache.getAllCerts is unexported); the public surface for
// "what certs does this daemon have?" is the Storage interface that
// certmagic itself uses. We own the on-disk layout: certmagic's
// FileStorage writes certs under
//
//	<StorageDir>/certificates/<issuerKey>/<domain>/<domain>.crt
//
// (verified against /Users/poyrazk/go/pkg/mod/github.com/caddyserver/certmagic@v0.25.4/storage.go:230-235
// and filestorage.go:118-130). Walking that tree once per interval — and
// parsing each .crt's leaf for NotAfter — is the only public-API path
// to "soonest expiry across cached certs".
//
// The refresher is best-effort: a transient parse error on one cert
// does not fail the loop. A consistent "no certs on disk yet" case is
// silent — the gauge stays at +Inf (the prometheus.Gauge default) and
// the alert rule's `< 14 * 86400` expression correctly returns false
// for Inf, so no spurious page fires pre-traffic.

package gateway

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certmagicCertDir is the top-level directory under cfg.StorageDir where
// certmagic's FileStorage writes issued/renewed certs. See storage.go:357
// (`prefixCerts = "certificates"`) in the vendored certmagic package.
const certmagicCertDir = "certificates"

// StartCertExpiryRefresher walks cfg.StorageDir every interval and writes
// the minimum remaining lifetime across cached certs to
// m.SetTLSCertExpiry. Returns a stop() closure the caller MUST invoke to
// halt the ticker on shutdown; main wires stop() into the signal-driven
// shutdown path.
//
// interval is typically 5 min — LE certs have a 90-day lifetime and
// certmagic's renew loop starts at the 30-day mark, so a 5-min poll
// gives the §12 alert plenty of headroom and keeps file I/O negligible
// (one filepath.Walk per tick, only the .crt files touched).
//
// storageDir is the same path certmagic writes to — the role installs
// it as faas:faas 0700, and the daemon runs as user faas, so reading
// is straightforward; nothing here writes to the dir.
//
// m may be nil for the unit-test path; SetTLSCertExpiry is nil-safe.
//
// log receives a single Warn per transient error (a single unparseable
// PEM, a directory-not-empty on rotation) and nothing else — a healthy
// refresher is silent.
func StartCertExpiryRefresher(ctx context.Context, storageDir string, m *Metrics, interval time.Duration, log *slog.Logger) (stop func()) {
	// Mirror pkg/gateway/idle.go's ticker pattern: one ticker, one done
	// channel that ctx propagates. The stop() closure channels into
	// done; the goroutine exits on whichever fires first.
	done := make(chan struct{})
	if log == nil {
		log = slog.Default()
	}

	go func() {
		// First tick fires after one interval, not immediately. The
		// daemon typically has zero certs at boot anyway; firing
		// instantly would log a "no certs found" on every restart.
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				if err := refreshCertExpiryOnce(ctx, storageDir, m, log); err != nil {
					log.Warn("gateway: cert expiry refresh", "err", err)
				}
			}
		}
	}()

	return func() {
		// Non-blocking close so a stop() after ctx-cancel doesn't wedge.
		// done is buffered-by-channel-closure semantics — a receive on a
		// closed channel returns immediately, which is what we want.
		select {
		case <-done:
		default:
			close(done)
		}
	}
}

// refreshCertExpiryOnce is the per-tick body of StartCertExpiryRefresher.
// Split out so the unit test can drive a single pass without spinning a
// ticker. Returns a non-nil error only when the storage walk itself
// fails (storageDir missing, permission denied); per-cert parse errors
// are logged inside walkCerts and do not fail the call.
func refreshCertExpiryOnce(ctx context.Context, storageDir string, m *Metrics, log *slog.Logger) error {
	if storageDir == "" {
		return errors.New("gateway: empty cert storage dir")
	}
	certsRoot := filepath.Join(storageDir, certmagicCertDir)
	soonest, found, err := walkCerts(ctx, certsRoot, log)
	if err != nil {
		return err
	}
	if !found {
		// No certs on disk — leave the gauge at its +Inf default. The
		// alert's `<` expression handles Inf correctly (always false),
		// so a fresh daemon or a freshly-cut-over box does not page.
		return nil
	}
	// time.Until(soonest) is the time delta to the soonest-expiring
	// cert; it goes negative when a cert on disk is already past its
	// NotAfter. We deliberately do NOT clamp to 0: the gauge should
	// report the actual delta (negative = expired) so the page rule's
	// `gateway_tls_cert_expiry_seconds < 14 * 86400` fires without
	// ambiguity. A clamp-to-0 would let the `> 0` guard the rule used
	// to carry filter out the alert — see PR #345 review.
	remaining := time.Until(soonest)
	m.SetTLSCertExpiry(remaining)
	return nil
}

// walkCerts enumerates every .crt file under certsRoot and returns the
// soonest NotAfter, plus a found=true flag when at least one cert was
// successfully parsed. Per-cert parse errors are logged + skipped so a
// single broken PEM does not stop the gauge from refreshing.
//
// We stop walking as soon as we encounter an fs.ErrNotExist on the root
// or on a subdir-remove during a renewal — that path is "no certs"
// (found=false), not an error, so the caller leaves the gauge untouched.
func walkCerts(ctx context.Context, certsRoot string, log *slog.Logger) (soonest time.Time, found bool, err error) {
	if log == nil {
		// Tests pass nil; the package-level slog.Default() is the
		// safe-fallback for parse-error logging so a nil-tolerant test
		// path doesn't panic.
		log = slog.Default()
	}
	_, statErr := os.Stat(certsRoot)
	if errors.Is(statErr, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if statErr != nil {
		return time.Time{}, false, statErr
	}
	// Initial sentinel: a real time.Time far in the future, chosen so
	// time.Time.Before works correctly. time.Unix(math.MaxInt64, 0) is
	// not safe — Go's time.Time comparison uses nanosecond arithmetic
	// that overflows at MaxInt64 scale and silently returns the wrong
	// sign. year-9999 is well inside the safe range and "no real cert
	// has a NotAfter that far out" (LE caps at 90 days), so the first
	// real parse always wins.
	soonest = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	err = filepath.WalkDir(certsRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A subdirectory disappeared mid-walk (concurrent renewal
			// by certmagic's own goroutine). Treat as "no more certs
			// from here" rather than failing the whole call.
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".crt") {
			return nil
		}
		na, parseErr := parseCertNotAfter(path)
		if parseErr != nil {
			log.Warn("gateway: skip cert (parse failed)", "path", path, "err", parseErr)
			return nil
		}
		found = true
		if na.Before(soonest) {
			soonest = na
		}
		return nil
	})
	if err != nil {
		return time.Time{}, false, err
	}
	if !found {
		// Reset the sentinel so the caller sees a fresh zero time
		// rather than the year-9999 sentinel (matches the no-cert case).
		return time.Time{}, false, nil
	}
	return soonest, true, nil
}

// parseCertNotAfter decodes the first PEM block in path and returns the
// leaf cert's NotAfter. We use x509.ParseCertificate directly (not
// tls.X509KeyPair, which would also need a private key) — NotAfter is
// a per-leaf field, not a chain field, so chain validation is wasted
// here. A bundle-style file (fullchain.pem) hits the first CERTIFICATE
// block which is the leaf in standard certmagic ordering, matching
// SiteCert's single-cert write.
//
// Returns a non-nil error on read/decode failure or PEM-block-not-found;
// the caller logs and continues so a single broken PEM does not stall
// the gauge refresh.
func parseCertNotAfter(path string) (time.Time, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	// pem.Decode returns the first PEM block; for a single-cert .crt
	// written by certmagic's SiteCert that's the leaf. For a hypothetical
	// bundle-style file, the leaf is the first block by RFC 5246 ordering.
	block, _ := pem.Decode(raw)
	if block == nil {
		return time.Time{}, errors.New("no PEM block found")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}
