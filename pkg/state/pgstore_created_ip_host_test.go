// Regression pin for the inet-scan bug (review finding #1, PR #653
// mega-PR). The pgstore.go SELECT and RETURNING clauses for
// api_keys scan the `created_ip inet` column into a Go *string via
// pgx; pgx has no default codec for `inet → *string`, so every
// SELECT/RETURNING site had to wrap the column with
// `coalesce(host(created_ip), '')` to render it as `text` on the
// wire (the existing sessions.issued_ip inet column solves the
// same problem with the same wrap pattern).
//
// We pin the wrap via the SQL fragment in pgstore.go's source. A
// live Postgres round-trip would also detect the regression but
// only when a non-NULL created_ip row exists; a contributor could
// "fix" the test by relaxing the SQL query and the regression
// would re-emerge on the next non-NULL mint. Pinning the source
// closes that gap.
package state

import (
	"os"
	"strings"
	"testing"
)

const sourceFile = "pgstore.go"
const hostWrap = "coalesce(host(created_ip),'')"

// TestPgStore_CreatedIP_HostCoalesce pins that every SELECT or
// RETURNING clause in pgstore.go that touches `created_ip`
// carries the `coalesce(host(created_ip), '')` wrap so pgx scans
// the result into Go `*string` (column type is `inet`; pgx has no
// default codec for inet → *string).
//
// INSERT/UPDATE column-declaration lines + VALUES placeholder
// positions don't need the wrap (pgx encodes Go string → inet
// correctly on the write path; the inet → string scan only
// happens on the read path).
func TestPgStore_CreatedIP_HostCoalesce(t *testing.T) {
	data, err := os.ReadFile(sourceFile)
	if err != nil {
		t.Fatalf("read %s: %v", sourceFile, err)
	}

	// Fail-fast signal: at least 18 SELECT/RETURNING sites must
	// carry the wrap. The PR landed 20 sites. A future contributor
	// adding one more query without the wrap will trip this lower
	// bound if they also remove an existing one (the wrap count
	// goes down); a contributor who keeps all existing wraps and
	// adds the missing wrap to new queries keeps the count ≥ 18.
	wrapCount := strings.Count(string(data), hostWrap)
	const minWrapSites = 18
	if wrapCount < minWrapSites {
		t.Errorf("%s: only %d `%s` wraps found; want ≥ %d. Every SELECT/RETURNING clause touching `created_ip` must carry the wrap so pgx can scan the result into `*string` (column type is `inet`, no default codec). INSERT/UPDATE column declarations + VALUES placeholders are exempt.",
			sourceFile, wrapCount, hostWrap, minWrapSites)
	}

	// Tight pin: every line that mentions `created_ip` and is NOT
	// already wrapped must be an INSERT column-declaration. If the
	// line is a SELECT or RETURNING clause that lacks the wrap,
	// pin against the regression.
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "created_ip,") {
			continue
		}
		if strings.Contains(line, hostWrap) {
			continue
		}
		trim := strings.TrimSpace(line)
		// INSERT column declarations are bare-string literals that
		// begin a SQL fragment: `insert into api_keys (...)`.
		// The trim may start with ` (backtick + start-of-literal)
		// or just with the `insert` keyword, depending on how the
		// Go formatter folded the raw string. Both shapes exempt.
		if strings.HasPrefix(trim, "insert into api_keys (") ||
			strings.HasPrefix(trim, "`insert into api_keys (") {
			continue // exempt: INSERT column declaration
		}
		t.Errorf("%s:%d: unwrapped `created_ip` reference on what looks like a SELECT/RETURNING clause: %q\n"+
			"    Wrap with `%s as created_ip` so pgx scans into `*string`. "+
			"(Pre-existing sessions.issued_ip column uses the same pattern.)",
			sourceFile, i+1, trim, hostWrap)
	}
}
