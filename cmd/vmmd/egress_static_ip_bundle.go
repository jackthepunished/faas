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

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// StaticEgressIPBundle (ADR-119) is the operator-supplied static
// egress IP set vmmd aliases onto br-tenants at startup AND on
// SIGHUP-reload. Each entry is a customer-supplied IPv4 that maps
// to a tenant app; the renderer in pkg/netns/config.go emits a
// per-VM MASQUERADE-sibling rule that SNATs tenant egress to the
// matching IP. The alias itself is what makes the kernel accept
// the customer's IP as a source on this host.
//
// The bundle is sorted + dedup'd at load time. Reserved-range
// entries (RFC1918, link-local, multicast, loopback, CGN) are
// rejected at load with a Warn — the same deny set apid enforces
// at the API layer, mirrored here so an operator typo can't pin a
// reserved IP through the side door. IPv6 entries are also
// rejected (v6 deferred per ADR-119 §3).
//
// The file is a flat list keyed by (app_slug, ip). Multiple
// entries for the same app are collapsed with the last one
// winning; the wire path on schedd's side re-validates per-app
// quota so a malformed entry can't bypass it.
type StaticEgressIPBundle struct {
	// Entries is sorted (by .AppID) and dedup'd (by app_id; last
	// IP for a given app wins). Reserved / invalid entries are
	// excluded.
	Entries []StaticEgressIPEntry
}

// StaticEgressIPEntry is one (appID, ip) tuple from the TOML.
// Type is defined here (cmd/vmmd is the only producer) but
// pkg/fcvm.Manager.SetStaticEgressIPAliases consumes the same
// shape — the cmd/vmmd package depends on pkg/fcvm, not the
// other way around, so this type stays here.
type StaticEgressIPEntry = fcvm.StaticEgressIPEntry

// staticEgressIPFile is the on-disk TOML shape. Same flat-list
// shape as the operator-allowlist bundle so the loader stays
// trivial. AppID is required; ip is required.
type staticEgressIPFile struct {
	Entries []struct {
		AppID string `toml:"app_id"`
		IP    string `toml:"ip"`
	} `toml:"entries"`
}

// LoadStaticEgressIPBundle reads the bundle from path. Missing
// file returns the zero-value bundle (= "no static IPs
// configured"). Per-entry parse errors, reserved-range IPs, and
// malformed rows are Warned and dropped; the rest of the file
// still loads.
//
// Mirrors LoadEgressBundle's fail-loud-on-parse / fail-soft-on-
// missing posture. The same deny set used in
// validCustomerStaticEgressIP (pkg/fcvm/manager.go) and the apid
// handler is reproduced here so the deny semantics don't drift
// across layers. A future PR may factor the deny set into
// pkg/netns/denylist.go — the current duplication is intentional
// to avoid a hot import edge from cmd/vmmd into pkg/api.
func LoadStaticEgressIPBundle(path string, log *slog.Logger) (StaticEgressIPBundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StaticEgressIPBundle{}, nil
		}
		return StaticEgressIPBundle{}, fmt.Errorf("vmmd: static egress IP bundle: read %q: %w", path, err)
	}
	if len(b) == 0 {
		return StaticEgressIPBundle{}, nil
	}
	var raw staticEgressIPFile
	if err := toml.Unmarshal(b, &raw); err != nil {
		return StaticEgressIPBundle{}, fmt.Errorf("vmmd: static egress IP bundle: parse %q: %w", path, err)
	}
	seen := make(map[string]netip.Addr, len(raw.Entries))
	out := make([]StaticEgressIPEntry, 0, len(raw.Entries))
	for _, e := range raw.Entries {
		appID := strings.TrimSpace(e.AppID)
		ipStr := strings.TrimSpace(e.IP)
		if appID == "" {
			log.Warn("vmmd: static egress IP bundle: dropping entry with empty app_id",
				"path", path, "ip", ipStr)
			continue
		}
		if ipStr == "" {
			log.Warn("vmmd: static egress IP bundle: dropping entry with empty ip",
				"path", path, "app_id", appID)
			continue
		}
		ip, perr := netip.ParseAddr(ipStr)
		if perr != nil {
			log.Warn("vmmd: static egress IP bundle: dropping invalid IP",
				"path", path, "app_id", appID, "ip", ipStr, "err", perr)
			continue
		}
		if !ip.Is4() {
			log.Warn("vmmd: static egress IP bundle: dropping IPv6 entry (v6 deferred per ADR-119)",
				"path", path, "app_id", appID, "ip", ipStr)
			continue
		}
		if !validStaticEgressIPAddr(ip) {
			log.Warn("vmmd: static egress IP bundle: dropping reserved-range IP",
				"path", path, "app_id", appID, "ip", ipStr)
			continue
		}
		// Last-wins per appID. The pkg/api/limits.go per-app
		// quota of 1 (Scale plan in v1) is enforced upstream
		// by the apid handler; the TOML is the operator-side
		// mirror that runs once at startup.
		seen[appID] = ip
	}
	for appID, ip := range seen {
		out = append(out, StaticEgressIPEntry{AppID: appID, IP: ip})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppID < out[j].AppID })
	if len(out) == 0 && len(raw.Entries) > 0 {
		log.Warn("vmmd: static egress IP bundle: empty after filtering (all entries rejected)",
			"path", path)
	}
	return StaticEgressIPBundle{Entries: out}, nil
}

// validStaticEgressIPAddr is the operator-side deny set. Mirrors
// validCustomerStaticEgressIP in pkg/fcvm/manager.go and the
// deny set the apid handler enforces. Kept in sync by the
// comment on each of the three call sites — a future PR factors
// this into pkg/netns/denylist.go. The TEST-NET ranges
// (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) are
// deliberately NOT denied here: customers may legitimately
// BYOIP from a public block that overlaps these — denying
// would mean a customer asking for "203.0.113.42" gets
// rejected even though that's a perfectly legal public IP
// for them to bring.
func validStaticEgressIPAddr(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, deny := range []string{
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"100.64.0.0/10",  // CGN
		"169.254.0.0/16", // link-local (defence in depth)
		"224.0.0.0/4",    // multicast (defence in depth)
	} {
		prefix, err := netip.ParsePrefix(deny)
		if err != nil {
			continue
		}
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// watchStaticEgressIPBundleReload is the SIGHUP-driven reload
// goroutine for the static-IP TOML (ADR-119). On every hupCh
// receive, it re-reads the bundle from path and forwards the
// resulting entries to mgr.SetStaticEgressIPAliases so the bridge
// alias set on br-tenants stays in sync with the operator file.
//
// Empty path = "no static IP bundle configured" — the goroutine
// skips cleanly (same posture as watchEgressBundleReload above).
// The same SIGHUP signal drives both watchers in production;
// vmmd main wires hupCh once and shares it across both.
//
// Failure model: a missing file is not an error (returns
// zero-value bundle = "remove all aliases"). A malformed file
// keeps the prior alias set live — the reload never silently
// strips a customer's IP because of a parse glitch.
func watchStaticEgressIPBundleReload(ctx context.Context, mgr staticEgressIPTarget, path string, log *slog.Logger, hupCh <-chan os.Signal) {
	if path == "" {
		log.Debug("vmmd: static egress IP bundle reload disabled (no path configured)")
		return
	}
	// Startup load: install aliases before any Wake observes
	// the bridge, so a fresh vmmd with a non-empty bundle has
	// the customer IPs aliased before any per-VM SNAT rule
	// fires. A missing file is benign (zero entries = "remove
	// all aliases"); a malformed file is Warned and the prior
	// alias set stays live.
	if bundle, err := LoadStaticEgressIPBundle(path, log); err != nil {
		log.Warn("vmmd: static egress IP bundle startup load failed; running with prior alias set",
			"path", path, "err", err)
	} else {
		mgr.SetStaticEgressIPAliases(bundle.Entries)
		log.Info("vmmd: static egress IP bundle loaded at startup",
			"path", path, "entries", len(bundle.Entries))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-hupCh:
			log.Info("vmmd: SIGHUP received, reloading static egress IP bundle")
			bundle, err := LoadStaticEgressIPBundle(path, log)
			if err != nil {
				log.Warn("vmmd: static egress IP bundle reload failed; keeping prior alias set",
					"path", path, "err", err)
				continue
			}
			mgr.SetStaticEgressIPAliases(bundle.Entries)
			log.Info("vmmd: static egress IP bundle reloaded",
				"path", path, "entries", len(bundle.Entries))
		}
	}
}

// staticEgressIPTarget is the narrow surface the SIGHUP reload
// goroutine needs from *fcvm.Manager. Defined as an interface so
// tests can stub the Manager without booting a real fcvm.Manager
// (same posture as egressBundleTarget above).
type staticEgressIPTarget interface {
	SetStaticEgressIPAliases(entries []StaticEgressIPEntry)
}
