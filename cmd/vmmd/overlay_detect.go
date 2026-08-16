// vmmd overlay IP detection (Mega-PR-B Commit 3).
//
// Wraps `tailscale ip -4` with CIDR-preference scoring so a multi-NIC
// host (subnet router, exit node) picks the IP that lives in the
// operator-declared overlay subnet, not whichever line tailscale
// happened to print first. Falls back to the legacy first-line
// behavior when no candidate matches PreferCIDR — preserves the v1
// single-host dev path.
//
// The detector is split out of defaultDetectOverlayIP so the
// scoring logic is unit-testable without shelling out; production
// callers go through defaultDetectOverlayIP, which constructs the
// zero-value detector.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
)

// OverlayDetector bundles the knobs that change how we pick an IP
// out of the tailscale response. Zero-value is the v1 behavior:
//
//   - TailscaleBinaryPath: LookPath("tailscale")
//   - PreferCIDR: empty (no preference)
//   - Run: cmd.Output-style exec.CommandContext("tailscale", "ip", "-4")
//
// Tests vary the three knobs to cover each scoring branch without
// shelling out.
type OverlayDetector struct {
	// TailscaleBinaryPath overrides exec.LookPath("tailscale"). Used
	// in tests to inject a binary that lives at a known path (or to
	// force LookPath failure without polluting PATH).
	TailscaleBinaryPath string

	// PreferCIDR is the overlay subnet that scores a candidate IP
	// higher than the rest. Empty prefix (zero value) means "no
	// preference" — every candidate scores equal and the first line
	// wins (v1 behavior). Setting this to api.DefaultOverlayCIDR()
	// yields the Tailscale-friendly selector; WireGuard/VPC overlays
	// pass the operator's AllowedIPs here.
	PreferCIDR netip.Prefix

	// Run produces the raw `tailscale ip -4` output. nil means
	// defaultDetectOverlayIP's production shell-out. Tests inject a
	// stub that returns canned bytes (no exec, no env mutation).
	Run func(ctx context.Context) ([]byte, error)
}

// detectOverlayIP picks the best IP from `tailscale ip -4` for the
// given PreferCIDR. Returns ("", nil) when tailscale isn't on PATH
// (preserves the legacy soft-success), ("", err) on actual exec
// failure or empty output (legacy behavior), or the highest-scoring
// IP (the first line, in iteration order, when multiple candidates
// tie on PreferCIDR).
func detectOverlayIP(ctx context.Context, det OverlayDetector) (string, error) {
	binary := det.TailscaleBinaryPath
	if binary == "" {
		lp, err := exec.LookPath("tailscale")
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return "", nil
			}
			return "", fmt.Errorf("tailscale LookPath: %w", err)
		}
		binary = lp
	}
	if !det.PreferCIDR.IsValid() && det.Run != nil {
		_ = det.PreferCIDR // intentional no-op: PreferCIDR empty + Run set is the "force stub, no scoring" path used by tests; the comment block above documents the rationale.
	}
	runner := det.Run
	if runner == nil {
		command := exec.CommandContext(ctx, binary, "ip", "-4")
		command.Env = append(os.Environ(), "TS_NO_LOGS_NO_SUPPORT=true")
		runner = func(ctx context.Context) ([]byte, error) {
			return command.Output()
		}
	}
	out, err := runner(ctx)
	if err != nil {
		return "", fmt.Errorf("tailscale ip -4: %w", err)
	}
	if len(out) == 0 {
		return "", errors.New("tailscale ip -4 returned empty")
	}
	addrs, err := parseTailscaleIPLines(out)
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", errors.New("tailscale ip -4 returned no IPv4 candidates")
	}
	best := scoreByCIDR(addrs, det.PreferCIDR)
	return best.String(), nil
}

// parseTailscaleIPLines turns the multi-line `tailscale ip -4`
// output into a slice of IPv4 candidates. Skips blank lines and
// IPv6 addresses (the `-4` flag already filters, but a misconfigured
// tailscale.conf that hands us a v6-only answer still produces no
// parse error — we just return an empty slice). Trailing whitespace
// is trimmed per line. Garbage lines that aren't parseable IPs are
// an error, so a corrupted tailscale.conf doesn't silently flip to
// a "no candidates" answer.
func parseTailscaleIPLines(out []byte) ([]netip.Addr, error) {
	var addrs []netip.Addr
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		ip, err := netip.ParseAddr(line)
		if err != nil {
			return nil, fmt.Errorf("parse tailscale line %q: %w", line, err)
		}
		if !ip.Is4() {
			// Skip IPv6 candidates silently — a v6-only tailscale
			// output is valid (`-4` plus v6 happens on dual-stack
			// exit nodes), and the legacy first-line contract only
			// ever expected v4.
			continue
		}
		addrs = append(addrs, ip)
	}
	return addrs, nil
}

// scoreByCIDR picks the candidate that lives in det.PreferCIDR
// when one (or more) candidates match. When det.PreferCIDR is
// invalid (zero-value), every candidate scores equal and addrs[0]
// wins (v1 first-line behavior). When two or more candidates tie
// on scoring, the one earlier in `addrs` wins (stable order — the
// caller can rely on this for assertion).
func scoreByCIDR(addrs []netip.Addr, prefer netip.Prefix) netip.Addr {
	if len(addrs) == 0 {
		return netip.Addr{}
	}
	if !prefer.IsValid() {
		return addrs[0]
	}
	for _, a := range addrs {
		if prefer.Contains(a) {
			return a
		}
	}
	// No candidate matched; fall back to first-line so a misconfigured
	// CIDR doesn't lose the v1 contract (an operator who forgot to
	// set overlay.cidr still gets the host's Tailscale IP).
	return addrs[0]
}
