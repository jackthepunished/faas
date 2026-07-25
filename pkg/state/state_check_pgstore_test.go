package state_test

import (
	"context"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/state"
)

// TestPgStore_InstancesStateCheck_AcceptsAllMachineStates is the
// load-bearing tripwire for ADR-034. Before migration 00035 the
// instances_state_check excluded 'snapshotting' and 'failed' (the
// two values schedd writes via the public Store surface — see
// engine.go:1048 and the six StateFailed writes at lines 438/554/
// 603/784/815/825). The drift was masked in CI because the engine
// tests use MemStore (no CHECK) and no existing test seeded a
// 'snapshotting' / 'failed' row via CreateInstance against real PG.
//
// This test iterates every state in machine.go::States and
// persists one row per value through the production write path.
// If the DB CHECK excludes any value, the test fails with
// SQLSTATE 23514 check_violation — the exact failure mode that
// would hit schedd in production today.
//
// The companion test,
// TestPgStore_InstancesStateCheck_SetMatchesMachineStates, pins
// the inverse direction: the literals extracted from the live
// `instances_state_check` definition must equal `States ∪ {pending}`.
// Together they catch drift in either direction.
func TestPgStore_InstancesStateCheck_AcceptsAllMachineStates(t *testing.T) {
	s, ctx := pgStore(t)
	_, appID, depID := seedLiveDeploy(t, s, ctx)
	nodeID := resolveDefaultLocal(t, ctx, s)

	for _, st := range state.States {
		t.Run(string(st), func(t *testing.T) {
			// One row per state. The state machine's legal edges
			// (transitions map) don't apply here — we're not
			// transitioning, we're seeding. The DB CHECK is the
			// only gate, and it must accept every value in
			// machine.go::States.
			ins, err := s.CreateInstance(ctx, appID, depID, string(st), 256, nodeID, "")
			if err != nil {
				t.Fatalf("CreateInstance(state=%s) = %v, want nil "+
					"(DB CHECK rejected a value listed in machine.go::States; "+
					"see ADR-034)", string(st), err)
			}
			if ins.State != string(st) {
				t.Errorf("CreateInstance round-trip state mismatch: "+
					"got %q, want %q", ins.State, string(st))
			}
		})
	}
}

// TestPgStore_InstancesStateCheck_SetMatchesMachineStates is the
// inverse tripwire. It queries pg_constraint for the live CHECK
// definition on `instances.state`, extracts the literal set
// (`'parked'`, `'waking'`, …), and asserts equality with
// `States ∪ {pending}`. The 'pending' value is a row-creation
// state with no Go constant (it appears in the transition map
// implicitly as the implicit "before cold_booting" pre-state),
// but it is referenced by migration 00028's partial index and
// must be in the CHECK set so new instance rows can land in
// 'pending' before the first boot.
//
// A future drift in either direction lands here:
//
//   * Go adds 'quota_evicting' to States but the migration
//     doesn't widen the CHECK → this test fails with
//     "literal set missing value 'quota_evicting'".
//   * A future migration narrows the CHECK (e.g. drops 'failed'
//     again) → this test fails with "literal set has unexpected
//     value 'failed' missing from States ∪ {pending}".
//
// The companion test
// (TestPgStore_InstancesStateCheck_AcceptsAllMachineStates) catches
// the "Go added a value but DB doesn't accept" direction; this
// test catches the "DB allows a value Go doesn't list" direction.
func TestPgStore_InstancesStateCheck_SetMatchesMachineStates(t *testing.T) {
	// This test only needs schema introspection (pg_constraint),
	// so we open a fresh pool directly. Calling pgStore(t) would
	// stand up a state.PgStore we don't need; the redundant
	// instance allocation isn't free, and the test is purely
	// about the CHECK definition, not the Store surface.
	pool := pgtest.Open(t)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Query pg_constraint for the live CHECK definition. The
	// consrc column has been replaced by pg_get_constraintdef in
	// modern Postgres (consrc is deprecated since 8.0 and may be
	// NULL; pg_get_constraintdef is the supported path).
	var consDef string
	err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(oid)
		from pg_constraint
		where conrelid = 'instances'::regclass
		  and conname = 'instances_state_check'
	`).Scan(&consDef)
	if err != nil {
		t.Fatalf("read instances_state_check definition: %v", err)
	}

	// The consdef looks like:
	//
	//   CHECK (state IN ('pending', 'parked', 'waking', ...))
	//
	// Extract the literal set. The function parseCheckLiterals
	// (below) is intentionally tiny — it parses one well-known
	// shape — so any future drift in the rendering surfaces
	// here as a parse failure rather than a silent miss.
	got := parseCheckLiterals(t, consDef, "instances_state_check")

	// Build the expected set: States ∪ {pending}.
	want := map[string]bool{}
	for _, st := range state.States {
		want[string(st)] = true
	}
	want["pending"] = true

	// Same-length first; the symmetric-diff loop below is
	// clearer with both sides pre-validated.
	if len(got) != len(want) {
		t.Fatalf("instances_state_check has %d values, want %d "+
			"(States ∪ {pending} = %d); got %v",
			len(got), len(want), len(want), got)
	}

	// Symmetric diff. Print only the offending values, not the
	// full set, so a future reviewer can scan the diff in
	// seconds.
	for v := range got {
		if !want[v] {
			t.Errorf("instances_state_check has unexpected value %q "+
				"(not in States ∪ {pending})", v)
		}
	}
	for v := range want {
		if !got[v] {
			t.Errorf("instances_state_check missing value %q "+
				"(required by States ∪ {pending})", v)
		}
	}
}

// TestPgStore_InstancesStateCheck_RejectsBogusState pins the
// inverse contract: the DB is not a no-op. A row with state =
// 'bogus' MUST be rejected with SQLSTATE 23514. If this ever
// fails, the CHECK has been disabled (or widened to text without
// constraints) and every other test in this file is meaningless.
func TestPgStore_InstancesStateCheck_RejectsBogusState(t *testing.T) {
	s, ctx := pgStore(t)
	_, appID, depID := seedLiveDeploy(t, s, ctx)
	nodeID := resolveDefaultLocal(t, ctx, s)

	_, err := s.CreateInstance(ctx, appID, depID, "bogus", 256, nodeID, "")
	if err == nil {
		t.Fatal("CreateInstance(state=bogus) = nil, want SQLSTATE 23514 check_violation")
	}
	// SQLSTATE 23514 is the standard check_violation. The
	// go-pgx driver surfaces it as either a *pgconn.PgError with
	// Code == "23514" or wrapped inside a higher-level error; we
	// substring-match the code to be driver-agnostic.
	if !strings.Contains(err.Error(), "23514") {
		t.Fatalf("CreateInstance(state=bogus) error = %v, want SQLSTATE 23514", err)
	}
}

// TestPgStore_InstancesStateCheck_RejectsInjection is the security
// pin. The CHECK must be a literal-set membership check, not a
// pattern match — a `state ~ '^[a-z_]+$'` style guard would let
// `'snapshotting'; drop table instances; --` ride through. A
// pg_get_constraintdef-only test (above) doesn't catch this; this
// test does, by attempting an obviously-non-CHECK-shaped value.
//
// The CREATE ROLE syntax is the simplest payload that demonstrates
// the difference — SQL-injection-by-column-value is impossible
// because CreateInstance uses sqlc-style parameterized queries
// (pgstore.go:1619), but the test is here as a cheap belt-and-
// braces against any future regression that swaps the parameterized
// query for a string-built one (the "no string-built queries" rule
// in CLAUDE.md).
func TestPgStore_InstancesStateCheck_RejectsInjection(t *testing.T) {
	s, ctx := pgStore(t)
	_, appID, depID := seedLiveDeploy(t, s, ctx)
	nodeID := resolveDefaultLocal(t, ctx, s)

	// Per ADR-017, PgStore writes are parameterized; the literal
	// string is sent as a parameter, not concatenated into the SQL
	// body. The CHECK must reject it as a non-member.
	_, err := s.CreateInstance(ctx, appID, depID,
		"bogus'); drop table instances; --", 256, nodeID, "")
	if err == nil {
		t.Fatal("CreateInstance(state=injection) = nil, want SQLSTATE 23514")
	}
	if !strings.Contains(err.Error(), "23514") {
		t.Fatalf("CreateInstance(state=injection) error = %v, want SQLSTATE 23514", err)
	}
}

// parseCheckLiterals extracts the literal set from a CHECK
// definition rendered by pg_get_constraintdef. Postgres rewrites
// `state IN ('a', 'b', 'c')` to one of two shapes depending on the
// length:
//
//   * short set (≤ a few elements): `state IN ('a', 'b', 'c')`
//   * longer set: `state = ANY (ARRAY['a'::text, 'b'::text, ...])`
//
// Both forms are observed in this repo (the SCHECK for the
// instances_state_check set is past the ANY-form threshold).
// The parser handles both, plus a `state = 'a' OR state = 'b' …`
// disjunctive form a future migration could write. Anything
// else is a parse failure — the conservative shape is deliberate:
// if a future migration writes the CHECK in a way that's no
// longer parseable here, the test fails loudly rather than
// silently passing with an empty set.
//
// The regex is anchored on the captured group so the function
// returns only the literal values, not the surrounding SQL.
func parseCheckLiterals(t *testing.T, def, name string) map[string]bool {
	t.Helper()
	// Strip the outer (...) wrapper.
	open := strings.Index(def, "(")
	close := strings.LastIndex(def, ")")
	if open < 0 || close < 0 || close <= open {
		t.Fatalf("%s: cannot find (...) in definition %q", name, def)
	}
	inner := def[open+1 : close]

	// Normalise whitespace once for the dispatch.
	upper := strings.ToUpper(inner)

	switch {
	case strings.Contains(upper, "ANY (ARRAY[") || strings.Contains(upper, "ANY(ARRAY["):
		// ANY(ARRAY[...]) form. Extract the ARRAY content.
		arrStart := strings.Index(upper, "ARRAY[")
		if arrStart < 0 {
			t.Fatalf("%s: ANY(...) without ARRAY[...] in %q", name, inner)
		}
		arrEnd := strings.LastIndex(upper, "]")
		if arrEnd < 0 || arrEnd <= arrStart {
			t.Fatalf("%s: ARRAY[...] not closed in %q", name, inner)
		}
		// Use the original-case slice so quote-escaping is right.
		arr := inner[arrStart+6 : arrEnd]
		return parseLiteralList(t, name, arr, "ANY(ARRAY[...])")

	case strings.Contains(upper, " IN "):
		// IN (...) form. Strip everything up to and including
		// the IN keyword.
		inPos := strings.Index(upper, " IN ")
		after := inner[inPos+4:]
		return parseLiteralList(t, name, after, "IN (...)")

	case strings.Contains(upper, " = "):
		// Bare `state = 'a'` form (one literal) or
		// disjunctive `state = 'a' OR state = 'b' OR ...` form.
		// Split on " OR " (case-insensitive) and parse each side.
		parts := splitScopedOR(inner)
		out := map[string]bool{}
		for _, p := range parts {
			// Each part is `state = 'literal'`. Take the last
			// quoted string.
			lit := lastQuotedLiteral(t, p, name)
			out[lit] = true
		}
		return out

	default:
		t.Fatalf("%s: unrecognised CHECK shape in %q", name, def)
		return nil
	}
}

// parseLiteralList takes the inside of an IN (...) or ARRAY[...]
// clause and returns the literal set. Each element is a quoted
// SQL literal like `'foo'::text` or `'foo'`; we strip the
// surrounding single quotes and the optional `::type` cast.
func parseLiteralList(t *testing.T, name, raw, form string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, raw := range strings.Split(raw, ",") {
		lit := strings.TrimSpace(raw)
		if len(lit) < 2 || lit[0] != '\'' {
			// Empty / malformed — future-proof against the parser
			// running past the end of the list.
			continue
		}
		// Find the closing quote (skip SQL-escaped '' pairs).
		end := 1
		for end < len(lit) {
			if lit[end] == '\'' {
				if end+1 < len(lit) && lit[end+1] == '\'' {
					// SQL-escaped quote — skip both.
					end += 2
					continue
				}
				break
			}
			end++
		}
		if end >= len(lit) {
			t.Fatalf("%s (%s): literal %q not closed", name, form, lit)
		}
		unquoted := lit[1:end]
		unquoted = strings.ReplaceAll(unquoted, "''", "'")
		// Strip the optional `::type` cast. We only support the
		// shape Postgres actually emits for `state IN (...)` —
		// `'<val>'::text` or `'<val>'::character varying`.
		if cast := strings.Index(unquoted, "::"); cast >= 0 {
			unquoted = unquoted[:cast]
		}
		out[unquoted] = true
	}
	return out
}

// splitScopedOR splits an expression on top-level ` OR ` tokens.
// Not a full SQL parser — sufficient for the `state = 'a' OR
// state = 'b' OR ...` shape a future migration could write.
// Parentheses-in-OR is not supported (the CHECK would not be
// emitted in that shape by Postgres anyway).
func splitScopedOR(expr string) []string {
	parts := []string{expr}
	upper := strings.ToUpper(expr)
	for {
		idx := strings.Index(upper, " OR ")
		if idx < 0 {
			return parts
		}
		last := parts[len(parts)-1]
		parts[len(parts)-1] = strings.TrimSpace(last[:idx])
		parts = append(parts, strings.TrimSpace(last[idx+4:]))
		upper = upper[idx+4:]
	}
}

// lastQuotedLiteral extracts the last single-quoted string in
// expr. Used for the disjunctive `state = 'a' OR state = 'b' OR
// ...` form where each disjunct is `state = '<literal>'`.
func lastQuotedLiteral(t *testing.T, expr, name string) string {
	t.Helper()
	// Find the last quote pair.
	upper := expr
	// Walk from the end.
	for i := len(upper) - 1; i >= 1; i-- {
		if upper[i] != '\'' {
			continue
		}
		// Walk back to the opening quote.
		j := i - 1
		for j >= 0 {
			if upper[j] == '\'' && (j == 0 || upper[j-1] != '\'') {
				break
			}
			j--
		}
		if j < 0 {
			t.Fatalf("%s: no opening quote in %q", name, expr)
		}
		lit := upper[j+1 : i]
		// Unescape SQL doubled quotes.
		lit = strings.ReplaceAll(lit, "''", "'")
		// Strip the optional `::type` cast.
		if cast := strings.Index(lit, "::"); cast >= 0 {
			lit = lit[:cast]
		}
		return lit
	}
	t.Fatalf("%s: no quoted literal in %q", name, expr)
	return ""
}
