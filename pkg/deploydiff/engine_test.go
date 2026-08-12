package deploydiff

import (
	"encoding/json"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestCompute_PointerAwareAppConfig — the wire form's nil-vs-explicit
// distinction must survive the diff. *int(nil) = "don't touch";
// *int(&v) = "set to v". This is the contract per
// [pr-819-openapi-nullable-3-1] in memory.
func TestCompute_PointerAwareAppConfig(t *testing.T) {
	base := &api.AppResponse{
		Slug: "api", RAMMB: 256, MaxConcurrency: 2,
		StreamingEnabled: true, RequireAuthn: false,
	}
	baseline := Baseline{App: base}

	t.Run("nil fields produce no changes", func(t *testing.T) {
		got := Compute("api", baseline, Pending{})
		if len(got.Changes) != 0 {
			t.Fatalf("nil fields should produce 0 changes; got %+v", got.Changes)
		}
	})

	t.Run("non-nil equal value produces no change", func(t *testing.T) {
		v := 256
		got := Compute("api", baseline, Pending{
			AppConfig: AppConfigPatch{RAMMB: &v},
		})
		if len(got.Changes) != 0 {
			t.Fatalf("equal value should produce 0 changes; got %+v", got.Changes)
		}
	})

	t.Run("non-nil different value produces a Change", func(t *testing.T) {
		v := 512
		got := Compute("api", baseline, Pending{
			AppConfig: AppConfigPatch{RAMMB: &v},
		})
		if len(got.Changes) != 1 {
			t.Fatalf("expected 1 change; got %d", len(got.Changes))
		}
		c := got.Changes[0]
		if c.Field != "memory" || c.Kind != ChangeModify {
			t.Fatalf("unexpected change: %+v", c)
		}
		if c.Before.Value != 256 || c.After.Value != 512 {
			t.Fatalf("before/after wrong: before=%v after=%v", c.Before.Value, c.After.Value)
		}
	})

	t.Run("explicit false on boolean is not nil", func(t *testing.T) {
		f := false
		got := Compute("api", baseline, Pending{
			AppConfig: AppConfigPatch{StreamingEnabled: &f},
		})
		if len(got.Changes) != 1 {
			t.Fatalf("expected 1 change for streaming_enabled: false→true→false; got %d", len(got.Changes))
		}
	})
}

// TestCompute_FreshApp — every non-nil Pending field is a new-value
// Change when the app does not exist yet.
func TestCompute_FreshApp(t *testing.T) {
	v := 256
	c := 2
	got := Compute("new-app", Baseline{}, Pending{
		AppConfig: AppConfigPatch{RAMMB: &v, MaxConcurrency: &c},
	})
	if len(got.Changes) != 2 {
		t.Fatalf("expected 2 add-changes; got %d: %+v", len(got.Changes), got.Changes)
	}
	for _, ch := range got.Changes {
		if ch.Kind != ChangeAdd {
			t.Fatalf("fresh-app changes should be Add; got %s for %s", ch.Kind, ch.Field)
		}
	}
}

// TestCompute_EnvByScope — per-scope env diff: add, remove, modify.
func TestCompute_EnvByScope(t *testing.T) {
	baseline := Baseline{
		EnvByScope: map[string][]string{
			"default": {"FOO", "BAR"},
		},
	}
	pending := Pending{
		EnvByScope: map[string][]PendingEnv{
			"default": {{Key: "BAR"}, {Key: "BAZ", Value: "z"}},
			"staging": {{Key: "DEBUG", Value: "1"}},
		},
	}
	got := Compute("api", baseline, pending)

	// FOO removed, BAZ added (default), DEBUG added (staging).
	wantAdds := map[string]bool{
		"environment.default.BAZ":   false,
		"environment.staging.DEBUG": false,
	}
	wantRemoves := map[string]bool{
		"environment.default.FOO": false,
	}
	for _, c := range got.Changes {
		switch c.Kind {
		case ChangeAdd:
			if _, ok := wantAdds[c.Field]; !ok {
				t.Fatalf("unexpected add: %s", c.Field)
			}
			wantAdds[c.Field] = true
		case ChangeRemove:
			if _, ok := wantRemoves[c.Field]; !ok {
				t.Fatalf("unexpected remove: %s", c.Field)
			}
			wantRemoves[c.Field] = true
		}
	}
	for k, seen := range wantAdds {
		if !seen {
			t.Fatalf("missing add: %s", k)
		}
	}
	for k, seen := range wantRemoves {
		if !seen {
			t.Fatalf("missing remove: %s", k)
		}
	}
}

// TestCompute_Crons — (schedule, path) unique key per migration 00210.
func TestCompute_Crons(t *testing.T) {
	baseline := Baseline{
		Crons: []api.CronResponse{
			{ID: "c1", Schedule: "* * * * *", Path: "/old", Enabled: true},
		},
	}
	pending := Pending{
		Crons: []api.CreateCronRequest{
			{Schedule: "* * * * *", Path: "/new"},
			{Schedule: "0 * * * *", Path: "/hourly"},
		},
	}
	got := Compute("api", baseline, pending)

	removed := false
	added1 := false
	added2 := false
	for _, c := range got.Changes {
		switch {
		case c.Kind == ChangeRemove && c.Field == "cron[* * * * * /old]":
			removed = true
		case c.Kind == ChangeAdd && c.Field == "cron[* * * * * /new]":
			added1 = true
		case c.Kind == ChangeAdd && c.Field == "cron[0 * * * * /hourly]":
			added2 = true
		}
	}
	if !removed || !added1 || !added2 {
		t.Fatalf("cron diff wrong: removed=%v added1=%v added2=%v", removed, added1, added2)
	}
}

// TestCompute_EdgeRules — (kind, host, path) is the stable identity;
// priority / methods / enabled / action flips are "modify".
func TestCompute_EdgeRules(t *testing.T) {
	mkAction := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"path": s})
		return b
	}
	baseline := Baseline{
		EdgeRules: []api.EdgeRuleResponse{
			{
				ID: "e1", MatchHost: "api.example.com", MatchPath: "/v1",
				MatchMethods: []string{"GET"}, Priority: 10, Enabled: true,
				Kind: "route", Action: mkAction("/v1"),
			},
		},
	}
	pending := Pending{
		EdgeRules: []api.CreateEdgeRuleRequest{
			{
				MatchHost: "api.example.com", MatchPath: "/v1",
				MatchMethods: []string{"GET", "POST"}, Priority: intPtr(5),
				Kind: "route", Action: mkAction("/v2"),
			},
			{
				MatchHost: "api.example.com", MatchPath: "/payments",
				Kind: "route", Action: mkAction("/payments"),
			},
		},
	}
	got := Compute("api", baseline, pending)

	modified := false
	added := false
	for _, c := range got.Changes {
		switch c.Field {
		case "edge_rule[route api.example.com/v1]":
			if c.Kind == ChangeModify {
				modified = true
			}
		case "edge_rule[route api.example.com/payments]":
			if c.Kind == ChangeAdd {
				added = true
			}
		}
	}
	if !modified || !added {
		t.Fatalf("edge rule diff wrong: modified=%v added=%v", modified, added)
	}
}

// TestCompute_DeploymentImmutable — image / manifest entrypoint /
// port / healthz changes emit a "would_create_deployment" Break,
// not a Change. Per dto.go:1326: deployment fields are immutable
// post-create except min_instances.
func TestCompute_DeploymentImmutable(t *testing.T) {
	base := &api.DeploymentResponse{
		ImageDigest:         "ghcr.io/me/api@sha256:old",
		OverrideEntrypoint:  []string{"/app/server"},
		OverridePort:        8080,
		OverrideHealthcheck: &api.DeploymentHealthcheck{Path: "/healthz"},
	}
	baseline := Baseline{App: &api.AppResponse{Slug: "api"}, LatestDeployment: base}

	// Image change → Break.
	pending := Pending{
		ImageRef: "ghcr.io/me/api@sha256:new",
	}
	got := Compute("api", baseline, pending)
	found := false
	for _, b := range got.Breaks {
		if b.Code == "would_create_deployment" {
			found = true
		}
	}
	if !found {
		t.Fatalf("image change should emit would_create_deployment break; got %+v", got.Breaks)
	}

	// Entrypoint change → Break.
	pending = Pending{
		Manifest: &api.AppManifest{
			Entrypoint: []string{"/app/new-server"},
			Port:       8080,
			Healthz:    "/healthz",
		},
	}
	got = Compute("api", baseline, pending)
	found = false
	for _, b := range got.Breaks {
		if b.Code == "would_create_deployment" {
			found = true
		}
	}
	if !found {
		t.Fatalf("entrypoint change should emit would_create_deployment break; got %+v", got.Breaks)
	}
}

// TestCompute_HasBlockingBreaks — the gate's exit-1 input.
func TestCompute_HasBlockingBreaks(t *testing.T) {
	d := Diff{
		Breaks: []Break{
			{Code: "warn_only", Severity: "warn"},
		},
	}
	if d.HasBlockingBreaks() {
		t.Fatal("warn-only diff should not block")
	}
	d.Breaks = append(d.Breaks, Break{Code: "real", Severity: "error"})
	if !d.HasBlockingBreaks() {
		t.Fatal("error-severity break should block")
	}
}

// TestEnvByScopeFromList — the wire-shape helper. Both the flat
// list and the nested EnvByScope shape must round-trip.
func TestEnvByScopeFromList(t *testing.T) {
	t.Run("nested scope shape", func(t *testing.T) {
		got := EnvByScopeFromList(api.AppEnvListResponse{
			EnvByScope: api.EnvByScope{
				"default": []api.ScopedAppEnvResponse{
					{Key: "FOO"}, {Key: "BAR"},
				},
				"staging": []api.ScopedAppEnvResponse{
					{Key: "DEBUG"},
				},
			},
		})
		if len(got["default"]) != 2 || len(got["staging"]) != 1 {
			t.Fatalf("nested scope parse wrong: %+v", got)
		}
	})
	t.Run("flat shape falls back to default scope", func(t *testing.T) {
		got := EnvByScopeFromList(api.AppEnvListResponse{
			Env: []api.AppEnvResponse{{Key: "FOO"}, {Key: "BAR"}},
		})
		if len(got[api.DefaultEnvScope]) != 2 {
			t.Fatalf("flat shape should populate default scope: %+v", got)
		}
	})
}

// TestStringSliceEqual — order-independent equality.
func TestStringSliceEqual(t *testing.T) {
	if !stringSliceEqual([]string{"a", "b"}, []string{"b", "a"}) {
		t.Fatal("set equality should ignore order")
	}
	if stringSliceEqual([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("different lengths should not be equal")
	}
}

// mkAction builds a tiny json.RawMessage for edge-rule Action
// fields in tests.
func mkAction(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": s})
	return b
}

// intPtr returns &v; used for pointer-typed fields on
// api.CreateEdgeRuleRequest (Priority, Enabled).
func intPtr(v int) *int { return &v }
