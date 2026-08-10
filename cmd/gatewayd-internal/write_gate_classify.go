// write_gate_classify.go — pre-handler classifier that
// decides whether the request is a write, a carve-out, a
// loop attempt, or an apid-bound read. Extracted from
// write_gate.go so the ServeHTTP body stays under the
// 50-line budget (CLAUDE.md "Handlers ≤ 50 lines — extract").
//
// Every classification path returns either (decision, true)
// meaning "I made the call; do NOT also increment via the
// default counter" or (decision, false) meaning "fall through
// to the default next.ServeHTTP path". The metric accounting
// lives in write_gate.go::ServeHTTP; this file is a pure
// decision-tree.
package main

import (
	"net/http"

	"github.com/onebox-faas/faas/pkg/gateway/writegate"
)

// classifyResult is the gate's pre-handler verdict.
type classifyResult struct {
	// decision is one of the constants below.
	decision classifyDecision
	// authKind is the closed label for the metric. Recorded
	// here so ServeHTTP doesn't need to recompute it.
	authKind writegate.AuthKind
}

// classifyDecision enumerates the 8 outcomes from write_gate.go.
type classifyDecision int

const (
	decisionBypass classifyDecision = iota
	decisionLoop
	decisionSameBox
	decisionUnreachable
	decisionCookieRedirect
	decisionRelayed
	decisionMTLSFailure
	decisionError
)

// classifyRequest inspects r + resolver state and returns
// the gate's verdict. The function is intentionally pure —
// no ResponseWriter, no side effects. The caller (ServeHTTP)
// is responsible for emitting the metric + response based on
// the returned decision.
//
// Order of checks (load-bearing — reordering changes
// observable behaviour on the cookie / bearer boundary):
//
//  1. IsWriteRequest && isApidPath — short-circuit reads +
//     carve-outs + non-apid paths to `decisionBypass`. The
//     predicate already enforces method + path; this branch
//     only fires for genuine write mutations on the apid
//     surface.
//
//  2. IsLoopAttempt — inbound sentinel present. Must run
//     BEFORE resolver lookup so a spoofed sentinel cannot
//     trigger a leader-resolution DB hit on every loop probe.
//
//  3. resolver.Current — drives decisions 3..7. The resolver
//     has its own caching (CachedLeaderResolver, B3) so the
//     lookup is a mutex read in the steady state.
//
//  4. auth-kind branching — only reached when the resolver
//     says we're a standby. Cookie → 307; bearer/anonymous →
//     relay via LeaderHTTPClient. authKind is computed once
//     and threaded into the decision so the metric increment
//     uses the same value the relay or redirect sees.
func (g *writeGate) classifyRequest(r *http.Request, leaderName string, isMe bool) classifyResult {
	// 1. Reads, carve-outs, non-apid paths bypass entirely.
	//    This is the single most common case in steady state.
	if !writegate.IsWriteRequest(r) || !g.isApidPath(r.URL.Path) {
		return classifyResult{decision: decisionBypass}
	}

	// 2. Loop guard. Sentinel presence rejects even on the
	//    leader — a relay should never see the header, but a
	//    spoofed one can. The receiving leader's
	//    pre-Relay check (loop_prevented path) and ours are
	//    symmetric.
	if writegate.IsLoopAttempt(r) {
		return classifyResult{
			decision: decisionLoop,
			authKind: writegate.AuthKindOf(r),
		}
	}

	// 3. Resolver — already evaluated by the caller before
	//    classification so the same name/isMe is reused for
	//    every retry inside the gate (the resolver may flip
	//    mid-call; this is acceptable because the gate's
	//    response is per-request and the next request will
	//    see the fresh state).
	authKind := writegate.AuthKindOf(r)

	// 3a. Same-box path.
	if isMe {
		return classifyResult{
			decision: decisionSameBox,
			authKind: authKind,
		}
	}

	// 3b. No leader (resolver returned empty).
	if leaderName == "" {
		return classifyResult{
			decision: decisionUnreachable,
			authKind: authKind,
		}
	}

	// 4. Cookie-only on standby → 307. Bearer / anonymous →
	//    relay. The split lives here so write_gate_respond.go
	//    can switch on a single decision constant.
	if authKind == writegate.AuthCookie {
		return classifyResult{
			decision: decisionCookieRedirect,
			authKind: authKind,
		}
	}
	return classifyResult{
		decision: decisionRelayed,
		authKind: authKind,
	}
}
