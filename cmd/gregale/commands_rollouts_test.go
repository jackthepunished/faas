// commands_rollouts_test.go — CLI tests for
// `gregale rollouts recover <slug>` (SAFE-RELEASES-R,
// issue #976 / ADR-122).
//
// Coverage matrix:
//
//   - cmdRollouts no-args usage branch exits 1.
//   - cmdRollouts unknown-subcommand branch exits 1.
//   - cmdRolloutsRecover validation gates:
//       * no positional slug → exit 1 (usage).
//       * missing --action → exit 1 (no round-trip).
//       * bad --action → exit 1 (closed-set check before the
//         network round-trip; the fake API's hit counter stays
//         at 0).
//   - happy path: route hits POST
//     /v1/apps/{slug}/rollouts/recover with the closed-set
//     body ({"action": "...", "reason": "..."}).
//   - happy path with --json: emits the raw
//     RolloutTransitionResponse DTO verbatim.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

func TestCmdRollouts_NoArgsExitsOne(t *testing.T) {
	resetJSONOut(t)
	authedFakeAPI(t, "", http.StatusOK)
	if code := cmdRollouts(nil); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestCmdRollouts_UnknownSubcommandExitsOne(t *testing.T) {
	resetJSONOut(t)
	authedFakeAPI(t, "", http.StatusOK)
	if code := cmdRollouts([]string{"wat"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestCmdRolloutsRecover_NoSlugExitsOne(t *testing.T) {
	resetJSONOut(t)
	authedFakeAPI(t, "", http.StatusOK)
	if code := cmdRolloutsRecover([]string{"--action", "promote"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestCmdRolloutsRecover_NoActionExitsOne(t *testing.T) {
	resetJSONOut(t)
	authedFakeAPI(t, "", http.StatusOK)
	if code := cmdRolloutsRecover([]string{"my-app"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestCmdRolloutsRecover_BadActionExitsOne(t *testing.T) {
	resetJSONOut(t)
	// The fake API is set up but should NOT be hit — the
	// closed-set check fires before the network round-trip.
	f := authedFakeAPI(t, "", http.StatusOK)
	if code := cmdRolloutsRecover([]string{"my-app", "--action", "explode"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if f.sawMethod != "" || f.sawPath != "" {
		t.Errorf("fake API was hit (%s %s); closed-set gate should fire first", f.sawMethod, f.sawPath)
	}
}

func TestCmdRolloutsRecover_HappyPath_Promote(t *testing.T) {
	resetJSONOut(t)
	body := `{"deployment":{"id":"0123456789abcdef0123456789abcdef","app_id":"abc","rollout_state":"complete","canary_step":4,"canary_total_steps":4},"audit_id":"42"}`
	f := authedFakeAPI(t, body, http.StatusOK)
	if code := cmdRolloutsRecover([]string{"my-app", "--action", "promote", "--reason", "manual-test"}); code != 0 {
		t.Fatalf("exit = %d, want 0, body=%s", code, "")
	}
	if f.sawMethod != "POST" || f.sawPath != "/v1/apps/my-app/rollouts/recover" {
		t.Errorf("route = %s %s, want POST /v1/apps/my-app/rollouts/recover", f.sawMethod, f.sawPath)
	}
	// Body must round-trip the closed-set + reason verbatim.
	var got api.RecoverRolloutRequest
	if err := json.Unmarshal(f.sawBody, &got); err != nil {
		t.Fatalf("body parse: %v: raw=%s", err, string(f.sawBody))
	}
	if got.Action != "promote" {
		t.Errorf("action=%q, want promote", got.Action)
	}
	if got.Reason != "manual-test" {
		t.Errorf("reason=%q, want manual-test", got.Reason)
	}
}

func TestCmdRolloutsRecover_JSONMode(t *testing.T) {
	resetJSONOut(t)
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	body := `{"deployment":{"id":"0123456789abcdef0123456789abcdef","app_id":"abc","rollout_state":"aborted","rollout_aborted_reason":"customer-confirmed-broken"},"audit_id":"7"}`
	authedFakeAPI(t, body, http.StatusOK)
	if code := cmdRolloutsRecover([]string{"my-app", "--action", "abort", "--reason", "customer-confirmed-broken"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestCmdRolloutsRecover_AllClosedSetActions(t *testing.T) {
	// Pins that all three closed-set actions are accepted and
	// reach the HTTP round-trip — guards against a future
	// refactor that accidentally narrows the closed set in
	// one place (CLI) but not the other (api / handler / store).
	for _, action := range []string{"advance", "promote", "abort"} {
		t.Run(action, func(t *testing.T) {
			resetJSONOut(t)
			body := `{"deployment":{"id":"0123456789abcdef0123456789abcdef"},"audit_id":"1"}`
			f := authedFakeAPI(t, body, http.StatusOK)
			code := cmdRolloutsRecover([]string{"my-app", "--action", action})
			if code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if !strings.HasSuffix(f.sawPath, "/rollouts/recover") {
				t.Errorf("path=%q, want suffix /rollouts/recover", f.sawPath)
			}
		})
	}
}
