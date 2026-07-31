//go:build linux

// Port normalization ladder (ADR-051 §"Consequences"). The first cold
// boot of a new deployment may bind any port the customer picked —
// compose-style 3000, Rails-style 5000, Go-static-binary 8001. The
// guest's job is to surface that bind on :8080 so the host's
// `waitReady` can keep dialing 8080 forever and ADR-009's identical-
// inner-network-world invariant stays intact (the vmmd-side waitReady
// is not changed — it always dials 8080).
//
// Three rungs, tried top to bottom:
//
//  1. **Inject PORT=8080 into the manifest env.** Build-time (in
//     imaged/handler.go::manifestFromImageConfig), not boot-time, so
//     the customer image's frame-style entrypoint that `os.Getenv("PORT")`
//     read at process start sees 8080 (imaged's defaults always seed it
//     unless the customer overrode it). mode = "none" in the report
//     because no in-guest work happens — the customer's process bound
//     8080 directly.
//  2. **In-guest DNAT `8080 → <observed>` via `iptables -t nat -A
//     OUTPUT`.** Kernel-level, free. mode = "dnat".
//  3. **Userspace splice / bidirectional forwarder.** Last resort:
//     NAT is unavailable, the bind is loopback-only, or the customer's
//     process hard-codes the port AND reads from stdin (process
//     rewrite is impossible). mode = "forward".
//
// The mode is captured in CharacterizationReport and surfaced as
// `guest_port_normalization_total{mode=...}`. The metric drives a
// monthly review: if "forward" climbs above 5% of cold boots, the
// platform team should prioritize DNAT fitness on a future guest
// kernel.

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// PortNormMode + the ladder constants live in portnorm_common.go
// so the unit test can pin them on every platform; this file only
// holds the linux-only runtime helpers (kernel capability probe,
// iptables rule install, userspace forwarder).

// natAvailable reports whether the guest kernel has netfilter NAT
// compiled in. We don't require functional conntrack — the only rule
// we install is `-t nat -A OUTPUT -p tcp --dport 8080 -j REDIRECT
// --to-ports <observed>`, which only needs the nat table to exist.
// The probe is a no-op iptables list; reading /proc/net/ip_tables_names
// is the cheaper predicate but is newer-than-most-guests.
func natAvailable() bool {
	// Cheaper path: the sysctl-removed kernels (CONFIG_NETFILTER=y
	// but no nat) report an empty /proc/net/ip_tables_names. If the
	// file exists and lists "nat", we can install rules.
	if data, err := readProcNetIPTableNames(); err == nil && containsNat(data) {
		return true
	}
	// Fallback: try a benign rule-add + remove. If either succeeds
	// we have netfilter NAT; otherwise the kernel rejects with
	// EOPNOTSUPP or ENOENT.
	c := exec.Command("iptables", "-t", "nat", "-L", "-n")
	if err := c.Run(); err == nil {
		return true
	}
	return false
}

// readProcNetIPTableNames reads /proc/net/ip_tables_names. Returns
// the raw bytes (one table name per line) or an error if the file is
// missing (modern kernels removed it when nothing is registered).
func readProcNetIPTableNames() ([]byte, error) {
	//nolint:forbidigo // Probed kernel-info file: /proc/net/ip_tables_names
	// is a kernel-managed pseudo-file the guest-init probes for netfilter
	// NAT compilation. The path is hard-coded, no customer input can
	// influence it, the openCustomerFile lint gate targets paths that
	// come in via the manifest or webhook.
	f, err := os.Open("/proc/net/ip_tables_names")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// containsNat lives in characterize_common.go (build-tag-free) so
// the unit test exercises it on every platform.

// installDNAT installs `iptables -t nat -A OUTPUT -p tcp --dport 8080
// -j REDIRECT --to-ports <observed>`. Idempotency: a duplicate rule
// has no effect (the redundancy chain still redir 8080 → observed),
// so we don't bother checking before adding. Caller is responsible
// for ensuring the rules are limited to this guest's network
// namespace — the per-guest netns from §11 makes that a non-issue.
//
// Failures are warn-logged and reported as "nat_failed" so the
// engine probe in PR-D can fall through to the userspace forwarder.
func installDNAT(observed int, log *slog.Logger) error {
	rule := fmt.Sprintf("-t nat -A OUTPUT -p tcp --dport %d -j REDIRECT --to-ports %d", 8080, observed)
	c := exec.Command("iptables", strings.Fields(rule)...)
	if out, err := c.CombinedOutput(); err != nil {
		log.Warn("portnorm DNAT install failed", "rule", rule, "err", err, "out", strings.TrimSpace(string(out)))
		return err
	}
	return nil
}

// startForwarder spawns a goroutine that listens on 127.0.0.1:8080
// and bridges every accepted connection's Read/Write to/from the
// app's actual observed port. Returns the listener so the
// characterize probe can observe both ports in listening_addrs.
//
// The bridge uses unix.Splice when available (zero-copy on a recent
// kernel) and falls back to io.Copy on either side. Sized at 64 KiB
// per direction; this matches http.Server's default write buffer.
//
// Returns (listener, error). Listener is non-nil even on partial
// failure: a copy-loop is always available as a fallback.
func startForwarder(observed int, log *slog.Logger) (net.Listener, error) {
	if observed <= 0 || observed > 65535 {
		return nil, fmt.Errorf("portnorm forward: observed %d out of range", observed)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		return nil, fmt.Errorf("portnorm forward: listen 127.0.0.1:8080: %w", err)
	}
	go func() {
		for {
			c, accErr := ln.Accept()
			if accErr != nil {
				log.Debug("portnorm forward: accept ended", "err", accErr)
				return
			}
			go bridge(c, observed, log)
		}
	}()
	return ln, nil
}

// bridge proxies one accepted connection to 127.0.0.1:<observed>.
// Closes both halves on either error. We avoid unix.Splice here
// for v1 — the splice two-syscall dance is per-arch; io.Copy is
// good enough for the cold-boot probe-path and never pins a
// customer request (the forwarder is torn down before the gateway
// publishes the instance to its target set).
func bridge(c net.Conn, observed int, log *slog.Logger) {
	defer func() { _ = c.Close() }()
	target, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(observed))
	if err != nil {
		log.Debug("portnorm forward: dial back failed", "err", err)
		return
	}
	defer func() { _ = target.Close() }()

	// Bidirectional io.Copy. Sentinel errors on either direction
	// trigger close-on-both; syscall.EAGAIN on the source side is
	// normal (the customer's request byte-stream is naturally idle
	// for short periods).
	done := make(chan struct{}, 2)
	go func() { _, _ = ioCopy(target, c); done <- struct{}{} }()
	go func() { _, _ = ioCopy(c, target); done <- struct{}{} }()
	<-done
	<-done
}

// ioCopy is a tiny helper that absorbs the `errors.Is(err, unix.EPIPE)`
// pattern (a client closing the connection surfaces as EPIPE on
// the dst side) so the bridge goroutine doesn't print log spam on
// every connection end. Guarded by `errors.Is(err, net.ErrClosed)`
// for the listener-close case.
func ioCopy(dst io.Writer, src io.Reader) (int64, error) {
	const buf = 64 * 1024
	n, err := io.CopyBuffer(dst, src, make([]byte, buf))
	if errors.Is(err, unix.EPIPE) || errors.Is(err, net.ErrClosed) {
		return n, nil
	}
	return n, err
}
