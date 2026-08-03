package floor

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestDecide_Total covers every (Floor, Concurrency, Plan, Class,
// Cooldown, Ceiling) tuple maps to exactly one Outcome. Pure-function
// table tests; no I/O.
func TestDecide_Total(t *testing.T) {
	now := time.Now()
	lastScaleOut := now.Add(-30 * time.Second)
	cooldownS := 60
	ramOK := AppStats{
		AppID:            "app-1",
		AccountID:        "acct-1",
		Plan:             api.PlanHobby,
		Floor:            2,
		Concurrency:      0,
		MaxConcurrency:   2,
		RAMMB:            256,
		WorkloadClass:    state.WorkloadClassHTTP,
		LastScaleOutAt:   time.Time{},
		ScaleOutCooldownS: 0,
		Now:              now,
		IsRamCeiling:     false,
	}
	cases := []struct {
		name     string
		mutate   func(*AppStats)
		want     Outcome
		admitNow bool
	}{
		{
			name:   "floor zero → disabled",
			mutate: func(s *AppStats) { s.Floor = 0 },
			want:   OutcomeDisabled,
		},
		{
			name:   "plan gate off → disabled",
			mutate: func(s *AppStats) { s.Plan = api.PlanFree },
			want:   OutcomeDisabled,
		},
		{
			name:   "worker class → disabled",
			mutate: func(s *AppStats) { s.WorkloadClass = state.WorkloadClassWorker },
			want:   OutcomeDisabled,
		},
		{
			name:   "concurrency already at floor → floor_met",
			mutate: func(s *AppStats) { s.Concurrency = 2 },
			want:   OutcomeFloorMet,
		},
		{
			name:   "concurrency above floor → floor_met",
			mutate: func(s *AppStats) { s.Concurrency = 3 },
			want:   OutcomeFloorMet,
		},
		{
			// floor_met wins over at_capacity because we already
			// have what we need; admit further is unnecessary.
			// at_capacity only fires when floor > MaxConcurrency
			// (defense-in-depth; can't happen under plan-cap clamp).
			name:   "concurrency at max_concurrency (floor met) → floor_met",
			mutate: func(s *AppStats) { s.Concurrency = 2; s.MaxConcurrency = 2 },
			want:   OutcomeFloorMet,
		},
		{
			name:   "concurrency at max_concurrency (floor not met) → at_capacity",
			mutate: func(s *AppStats) { s.Floor = 3; s.MaxConcurrency = 2; s.Concurrency = 2 },
			want:   OutcomeAtCapacity,
		},
		{
			name:   "cooldown in effect → cooldown_held",
			mutate: func(s *AppStats) { s.LastScaleOutAt = lastScaleOut; s.ScaleOutCooldownS = cooldownS },
			want:   OutcomeCooldownHeld,
		},
		{
			name:   "cooldown zero (no opt-in) → admit",
			mutate: func(s *AppStats) { s.LastScaleOutAt = lastScaleOut; s.ScaleOutCooldownS = 0 },
			want:   OutcomeAdmit,
			admitNow: true,
		},
		{
			name:   "cooldown elapsed → admit",
			mutate: func(s *AppStats) { s.LastScaleOutAt = now.Add(-120 * time.Second); s.ScaleOutCooldownS = cooldownS },
			want:   OutcomeAdmit,
			admitNow: true,
		},
		{
			name:   "backoff in effect → backoff_held",
			mutate: func(s *AppStats) { s.BackoffUntil = now.Add(30 * time.Second) },
			want:   OutcomeBackoffHeld,
		},
		{
			name:   "backoff elapsed → admit",
			mutate: func(s *AppStats) { s.BackoffUntil = now.Add(-1 * time.Second) },
			want:   OutcomeAdmit,
			admitNow: true,
		},
		{
			name:   "ram ceiling breached → ram_ceiling",
			mutate: func(s *AppStats) { s.IsRamCeiling = true },
			want:   OutcomeRamCeiling,
		},
		{
			name:   "happy path → admit",
			mutate: func(s *AppStats) {},
			want:   OutcomeAdmit,
			admitNow: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := ramOK
			c.mutate(&s)
			got := decide(s)
			if got.Outcome != c.want {
				t.Errorf("decide(%+v) outcome = %s, want %s", s, got.Outcome, c.want)
			}
			if got.AdmitNow != c.admitNow {
				t.Errorf("decide(%+v) AdmitNow = %v, want %v", s, got.AdmitNow, c.admitNow)
			}
		})
	}
}

// TestEffectiveCooldown pins the residual cooldown arithmetic.
func TestEffectiveCooldown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		last      time.Time
		cooldownS int
		wantZero  bool
	}{
		{
			name:      "never stamped → zero",
			last:      time.Time{},
			cooldownS: 60,
			wantZero:  true,
		},
		{
			name:      "cooldown disabled → zero",
			last:      now,
			cooldownS: 0,
			wantZero:  true,
		},
		{
			name:      "elapsed → zero",
			last:      now.Add(-120 * time.Second),
			cooldownS: 60,
			wantZero:  true,
		},
		{
			name:      "in window → positive residual",
			last:      now.Add(-30 * time.Second),
			cooldownS: 60,
			wantZero:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveCooldown(c.last, c.cooldownS, now)
			if c.wantZero && got != 0 {
				t.Errorf("effectiveCooldown = %v, want 0", got)
			}
			if !c.wantZero && got <= 0 {
				t.Errorf("effectiveCooldown = %v, want > 0", got)
			}
		})
	}
}
