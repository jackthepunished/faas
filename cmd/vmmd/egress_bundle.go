// Package main contains vmmd's runtime helpers (cmd/vmmd/main.go
// is the daemon entry point; the auxiliary helpers live alongside).
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// EgressBundle is the operator-managed egress CIDR set vmmd reads
// from /etc/faas/egress/operator_allowlist.toml at startup AND on
// SIGHUP-reload (issue #679 / PR-A). Every CIDR listed here is
// additive to every tenant's apps.egress_allowlist — operators can
// ONLY ADD reachability, never subtract.
//
// The bundle is sorted + dedup'd at load time. /0 CIDRs and
// per-entry parse failures are dropped with a Warn so a single bad
// entry does not poison the rest of the file. v4/v6 partition
// happens at render time (netns.Config.ForwardAllowlistRule /
// ForwardAllowlistRule6 read prefix.Addr().Is4()), not here.
type EgressBundle struct {
	// CIDRs is sorted (by .String()) and dedup'd. /0 entries are
	// excluded; a non-empty + slice means "operator bundle
	// active".
	CIDRs []netip.Prefix
}

// bundleFile is the on-disk TOML shape. Flat list keeps the loader
// trivial; if operator provenance metadata is needed later, add
// it as a sibling [[group]] cidrs = [...] section.
type bundleFile struct {
	CIDRs []string `toml:"cidrs"`
}

// LoadEgressBundle reads the bundle from path and returns the
// parsed EgressBundle. Missing file is not an error (returns
// zero-value bundle = "no operator additions"). TOML parse
// failure returns an error (fail-loud so the operator notices).
//
// Per-entry CIDR parse errors, /0 CIDRs (per the
// apps_egress_allowlist_cidr ADR-032 contract), and duplicates
// are Warned and dropped; the rest of the file still loads.
// Empty after filtering is also Warned but not an error — a
// comment-only file is legal.
func LoadEgressBundle(path string, log *slog.Logger) (EgressBundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EgressBundle{}, nil
		}
		return EgressBundle{}, fmt.Errorf("vmmd: egress bundle: read %q: %w", path, err)
	}
	if len(b) == 0 {
		return EgressBundle{}, nil
	}
	var raw bundleFile
	if err := toml.Unmarshal(b, &raw); err != nil {
		return EgressBundle{}, fmt.Errorf("vmmd: egress bundle: parse %q: %w", path, err)
	}
	seen := make(map[string]struct{}, len(raw.CIDRs))
	out := make([]netip.Prefix, 0, len(raw.CIDRs))
	for _, s := range raw.CIDRs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		prefix, perr := netip.ParsePrefix(s)
		if perr != nil {
			log.Warn("vmmd: egress bundle: dropping invalid CIDR",
				"path", path, "cidr", s, "err", perr)
			continue
		}
		if prefix.Bits() == 0 {
			// ADR-032 non-/0 contract — same gate the per-app
			// validator and the Wake-side parser enforce.
			log.Warn("vmmd: egress bundle: dropping /0 CIDR (ADR-032 non-/0 contract)",
				"path", path, "cidr", s)
			continue
		}
		key := prefix.String()
		if _, dup := seen[key]; dup {
			log.Warn("vmmd: egress bundle: dropping duplicate CIDR",
				"path", path, "cidr", key)
			continue
		}
		seen[key] = struct{}{}
		out = append(out, prefix)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	if len(out) == 0 {
		log.Warn("vmmd: egress bundle: empty after filtering (comment-only or all entries rejected)",
			"path", path)
	}
	return EgressBundle{CIDRs: out}, nil
}

// watchEgressBundleReload is the SIGHUP-driven egress-bundle
// reload goroutine (issue #679 / PR-A). On every hupCh
// receive, it re-reads the bundle from path and forwards the
// resulting CIDR slice to mgr.SetEgressOperatorBundle. A
// failed reload leaves the prior bundle live (best-effort:
// the daemon never refuses to keep running on a bundle
// error — a missing or malformed bundle just means the
// operator bundle is empty).
//
// The function never mutates hupCh from the inside. Production
// wraps it via signal.Notify(hupCh, syscall.SIGHUP); tests
// inject their own channel. Mirrors watchLogLevelReload's
// pattern (pkg/wire/daemon.go:194) so future readers can
// reuse the same shape.
//
// Empty path = "no operator bundle configured" — the goroutine
// exits cleanly on the first SIGHUP-after-config-clear.
func watchEgressBundleReload(ctx context.Context, mgr egressBundleTarget, path string, log *slog.Logger, hupCh <-chan os.Signal) {
	if path == "" {
		// No bundle configured. Skip the reload goroutine
		// entirely; the channel stays buffered but never gets
		// read. The SIGHUP-driven log-level reload in
		// pkg/wire/daemon.go still fires — that one is wired
		// at the wire layer, independent of this.
		log.Debug("vmmd: egress bundle reload disabled (no path configured)")
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			log.Info("vmmd: SIGHUP received, reloading egress bundle")
			bundle, err := LoadEgressBundle(path, log)
			if err != nil {
				log.Warn("vmmd: egress bundle reload failed; keeping prior bundle",
					"path", path, "err", err)
				continue
			}
			mgr.SetEgressOperatorBundle(bundle.CIDRs)
			log.Info("vmmd: egress bundle reloaded",
				"path", path, "cidrs", len(bundle.CIDRs))
		}
	}
}

// egressBundleTarget is the narrow surface the SIGHUP reload
// goroutine needs from *fcvm.Manager. Defined as an interface
// so tests can stub the Manager without booting a real
// fcvm.Manager (issue #679 / PR-A).
type egressBundleTarget interface {
	SetEgressOperatorBundle(cidrs []netip.Prefix)
}
