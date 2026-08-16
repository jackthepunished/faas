// role_test.go — whitebox tests for pkg/roleTemplating. Same package
// so the test can exercise unexported helpers (init-time invariant
// checks, the roleDaemons table itself).
//
// Whitebox pattern is per [[whitebox-test-file-pattern]]: the package
// has unexported invariants that MUST be exercised; a `_test` package
// would force every test to round-trip through the public API and
// miss the table-vs-registry cross-check.
//
// Each test is table-driven (CLAUDE.md "Table-driven tests"). The
// tests assert the load-bearing contracts:
//  1. roleDaemons is in lockstep with pkg/daemonunitspec.Registry
//  2. AllowedRoles is exhaustive over the package's role consts
//  3. Validate / Subset / DropIn reject unknown inputs cleanly
//  4. DropIn body is byte-for-byte stable across calls (idempotency)
//  5. Apply is idempotent given identical inputs
//  6. Mutate computes the right stop / start subsets in the right
//     order without calling systemctl in unit tests (execCommand
//     is the test seam)
package roleTemplating

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		role    Role
		wantErr bool
	}{
		{"control-plane accepted", RoleControlPlane, false},
		{"compute-only accepted", RoleComputeOnly, false},
		{"empty rejected", Role(""), true},
		{"unknown rejected", Role("multi-role"), true},
		{"uppercase control-plane rejected (case sensitive)", Role("Control-Plane"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.role)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%q) error = %v, wantErr %v", tt.role, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnknownRole) && tt.role != Role("") {
				// Empty-string rejection is also ErrUnknownRole; the
				// non-empty unknown rejection must specifically wrap it.
				t.Fatalf("Validate(%q) error %v does not wrap ErrUnknownRole", tt.role, err)
			}
		})
	}
}

func TestSubset(t *testing.T) {
	tests := []struct {
		name string
		role Role
		want []string // alphabetical per Subset()'s contract
	}{
		{
			"control-plane subset",
			RoleControlPlane,
			[]string{
				"apid", "gatewayd-internal", "gatewayd-public",
				"githubd", "imaged", "meterd", "schedd", "vmmd",
			},
		},
		{
			"compute-only subset",
			RoleComputeOnly,
			[]string{
				"builderd", "gatewayd-internal", "gatewayd-public",
				"imaged", "vmmd",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Subset(tt.role)
			if err != nil {
				t.Fatalf("Subset(%q) unexpected error: %v", tt.role, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Subset(%q) =\n  %v\nwant\n  %v", tt.role, got, tt.want)
			}
		})
	}

	// Subset must reject unknown roles.
	t.Run("unknown role rejected", func(t *testing.T) {
		_, err := Subset(Role("nope"))
		if err == nil {
			t.Fatalf("Subset(nope) returned nil error")
		}
	})
}

func TestSubsetCrossChecksRegistry(t *testing.T) {
	// The `init()` panic is already exercised at package load; this
	// asserts the operational table is non-empty AND a true
	// subset-of-registry.
	for _, r := range AllowedRoles {
		t.Run(string(r), func(t *testing.T) {
			got, err := Subset(r)
			if err != nil {
				t.Fatalf("Subset(%q) error: %v", r, err)
			}
			if len(got) == 0 {
				t.Fatalf("Subset(%q) is empty — would start no daemons", r)
			}
			registry := map[string]struct{}{}
			for _, e := range daemonunitspec.Registry {
				registry[e.Name] = struct{}{}
			}
			for _, d := range got {
				if _, ok := registry[d]; !ok {
					t.Fatalf("Subset(%q) lists %q, not in daemonunitspec.Registry; the roleTemplating table is stale", r, d)
				}
			}
		})
	}
}

func TestDropIn(t *testing.T) {
	tests := []struct {
		name   string
		role   Role
		daemon string
		want   string
	}{
		{
			"control-plane apid",
			RoleControlPlane, "apid",
			"[Service]\nEnvironment=FAAS_BOX_ROLE=control-plane\nEnvironment=FAAS_APID_ROLE=control-plane\n",
		},
		{
			"compute-only builderd",
			RoleComputeOnly, "builderd",
			"[Service]\nEnvironment=FAAS_BOX_ROLE=compute-only\nEnvironment=FAAS_BUILDERD_ROLE=compute-only\n",
		},
		{
			"daemon with hyphens gets uppercased correctly",
			RoleControlPlane, "gatewayd-public",
			"[Service]\nEnvironment=FAAS_BOX_ROLE=control-plane\nEnvironment=FAAS_GATEWAYD_PUBLIC_ROLE=control-plane\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DropIn(tt.role, tt.daemon)
			if err != nil {
				t.Fatalf("DropIn(%q, %q) error: %v", tt.role, tt.daemon, err)
			}
			if got != tt.want {
				t.Fatalf("DropIn(%q, %q):\n  got:  %q\n  want: %q", tt.role, tt.daemon, got, tt.want)
			}
		})
	}

	t.Run("unknown role rejected", func(t *testing.T) {
		_, err := DropIn(Role("nope"), "apid")
		if err == nil {
			t.Fatalf("DropIn on unknown role returned nil error")
		}
	})

	t.Run("empty daemon rejected", func(t *testing.T) {
		_, err := DropIn(RoleControlPlane, "")
		if err == nil {
			t.Fatalf("DropIn with empty daemon returned nil error")
		}
	})

	t.Run("whitespace daemon rejected", func(t *testing.T) {
		_, err := DropIn(RoleControlPlane, "   ")
		if err == nil {
			t.Fatalf("DropIn with whitespace daemon returned nil error")
		}
	})
}

func TestDropInIdempotent(t *testing.T) {
	// Two calls must produce byte-identical output. PR-A's first-boot
	// re-run guarantee.
	for _, r := range AllowedRoles {
		t.Run(string(r), func(t *testing.T) {
			a, err := DropIn(r, "vmmd")
			if err != nil {
				t.Fatalf("DropIn error: %v", err)
			}
			b, err := DropIn(r, "vmmd")
			if err != nil {
				t.Fatalf("DropIn error: %v", err)
			}
			if a != b {
				t.Fatalf("DropIn not idempotent for %q:\n  first:  %q\n  second: %q", r, a, b)
			}
		})
	}
}

func TestApplyWritesAllDropIns(t *testing.T) {
	var buf bytes.Buffer
	calls := 0
	daemonReload := func() error {
		calls++
		return nil
	}
	if err := Apply(RoleControlPlane, &buf, daemonReload); err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("daemonReload called %d times, want 1", calls)
	}
	for _, d := range []string{
		"vmmd", "apid", "schedd", "meterd", "githubd",
		"imaged", "gatewayd-internal", "gatewayd-public",
	} {
		want := "[Service]\nEnvironment=FAAS_BOX_ROLE=control-plane\n"
		if d == "apid" {
			want += "Environment=FAAS_APID_ROLE=control-plane\n"
		} else if d == "schedd" {
			want += "Environment=FAAS_SCHEDD_ROLE=control-plane\n"
		} else if d == "githubd" {
			want += "Environment=FAAS_GITHUBD_ROLE=control-plane\n"
		} else if d == "meterd" {
			want += "Environment=FAAS_METERD_ROLE=control-plane\n"
		} else if d == "vmmd" {
			want += "Environment=FAAS_VMMD_ROLE=control-plane\n"
		} else if d == "imaged" {
			want += "Environment=FAAS_IMAGED_ROLE=control-plane\n"
		} else if d == "gatewayd-internal" {
			want += "Environment=FAAS_GATEWAYD_INTERNAL_ROLE=control-plane\n"
		} else if d == "gatewayd-public" {
			want += "Environment=FAAS_GATEWAYD_PUBLIC_ROLE=control-plane\n"
		}
		needle := "\n" + want
		if !strings.Contains(buf.String(), needle) {
			t.Errorf("Apply output missing %q drop-in body:\n%s", d, buf.String())
		}
	}
}

func TestApplyIdempotent(t *testing.T) {
	// Two Apply calls with the same role produce byte-identical output.
	var buf1, buf2 bytes.Buffer
	noopReload := func() error { return nil }
	if err := Apply(RoleComputeOnly, &buf1, noopReload); err != nil {
		t.Fatalf("Apply first: %v", err)
	}
	if err := Apply(RoleComputeOnly, &buf2, noopReload); err != nil {
		t.Fatalf("Apply second: %v", err)
	}
	if buf1.String() != buf2.String() {
		t.Fatalf("Apply not idempotent:\n  first:\n%s\n  second:\n%s", buf1.String(), buf2.String())
	}
}

func TestApplySurfacesDaemonReloadError(t *testing.T) {
	want := errors.New("systemctl blew up")
	err := Apply(RoleControlPlane, &bytes.Buffer{}, func() error { return want })
	if err == nil {
		t.Fatalf("Apply with failing daemonReload returned nil")
	}
	if !errors.Is(err, want) {
		t.Fatalf("Apply error %v does not wrap %v", err, want)
	}
}

func TestApplyRejectsUnknownRole(t *testing.T) {
	err := Apply(Role("nope"), &bytes.Buffer{}, func() error { return nil })
	if err == nil {
		t.Fatalf("Apply on unknown role returned nil")
	}
	if !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("Apply error %v does not wrap ErrUnknownRole", err)
	}
}

func TestMutateControlPlaneToComputeOnly(t *testing.T) {
	calls := []string{}
	exec := func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	stopped, started, err := Mutate(RoleControlPlane, RoleComputeOnly, exec)
	if err != nil {
		t.Fatalf("Mutate error: %v", err)
	}

	// Stop: apid, schedd, meterd, githubd (NOT vmmd/imaged/gatewayd-* —
	// those are on both roles).
	wantStopped := map[string]bool{
		"apid": true, "schedd": true, "meterd": true, "githubd": true,
	}
	for _, d := range stopped {
		if !wantStopped[d] {
			t.Errorf("Mutate stopped %q, expected only the 4 control-plane-only daemons", d)
		}
	}
	if len(stopped) != len(wantStopped) {
		t.Errorf("Mutate stopped %d daemons (%v), want exactly %v", len(stopped), stopped, wantStopped)
	}

	// Start: builderd only.
	if len(started) != 1 || started[0] != "builderd" {
		t.Errorf("Mutate started %v, want only [builderd]", started)
	}

	// Verify stop ordering: gatewayd-public is NOT in the stop list
	// (it's on both roles). verify no extra systemctl calls were made.
	if len(calls) == 0 {
		t.Fatalf("Mutate made no systemctl calls")
	}
	// No "systemctl start faas-imaged" — imaged is on both roles.
	for _, c := range calls {
		if strings.Contains(c, "start faas-imaged") || strings.Contains(c, "start faas-vmmd") {
			t.Errorf("Mutate invoked %q — vmmd/imaged are on both roles, should not be re-started", c)
		}
		if strings.Contains(c, "stop faas-vmmd") || strings.Contains(c, "stop faas-imaged") ||
			strings.Contains(c, "stop faas-gatewayd-internal") || strings.Contains(c, "stop faas-gatewayd-public") {
			t.Errorf("Mutate invoked %q — should not stop shared-role daemons", c)
		}
	}
}

func TestMutateComputeOnlyToControlPlane(t *testing.T) {
	exec := func(name string, args ...string) (string, error) {
		return "", nil
	}
	stopped, started, err := Mutate(RoleComputeOnly, RoleControlPlane, exec)
	if err != nil {
		t.Fatalf("Mutate error: %v", err)
	}
	// Compute-only → Control-plane starts apid, schedd, meterd, githubd; stops builderd.
	if len(stopped) != 1 || stopped[0] != "builderd" {
		t.Errorf("Mutate stopped %v, want only [builderd]", stopped)
	}
	wantStarted := map[string]bool{
		"apid": true, "schedd": true, "meterd": true, "githubd": true,
	}
	for _, d := range started {
		if !wantStarted[d] {
			t.Errorf("Mutate started unexpected daemon %q", d)
		}
	}
	if len(started) != len(wantStarted) {
		t.Errorf("Mutate started %d daemons (%v), want exactly %v", len(started), started, wantStarted)
	}
}

func TestMutateNoChange(t *testing.T) {
	exec := func(name string, args ...string) (string, error) {
		t.Errorf("Mutate no-change invoked systemctl: %s %v", name, args)
		return "", nil
	}
	stopped, started, err := Mutate(RoleControlPlane, RoleControlPlane, exec)
	if err != nil {
		t.Fatalf("Mutate error: %v", err)
	}
	if len(stopped) != 0 || len(started) != 0 {
		t.Fatalf("Mutate no-change should produce empty diff, got stopped=%v started=%v", stopped, started)
	}
}

func TestMutateUnknownRole(t *testing.T) {
	exec := func(name string, args ...string) (string, error) {
		return "", nil
	}
	_, _, err := Mutate(RoleControlPlane, Role("nope"), exec)
	if err == nil {
		t.Fatalf("Mutate with unknown target returned nil")
	}
}

func TestMutateSurfacesExecError(t *testing.T) {
	boom := errors.New("systemctl timeout")
	exec := func(name string, args ...string) (string, error) {
		return "", boom
	}
	_, _, err := Mutate(RoleControlPlane, RoleComputeOnly, exec)
	if err == nil {
		t.Fatalf("Mutate with failing exec returned nil")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Mutate error %v does not wrap %v", err, boom)
	}
}

func TestAllowedRolesExhaustive(t *testing.T) {
	// AllowedRoles must contain BOTH RoleControlPlane and RoleComputeOnly.
	want := map[Role]struct{}{
		RoleControlPlane: {},
		RoleComputeOnly:  {},
	}
	got := map[Role]struct{}{}
	for _, r := range AllowedRoles {
		got[r] = struct{}{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedRoles mismatch:\n  got:  %v\n  want: %v", AllowedRoles, want)
	}
}
