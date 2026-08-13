package deploydiff

import (
	"encoding/json"
	"io"
)

// RenderJSON writes the [Diff] to w as stable JSON.
//
// Wire contract:
//   - Top-level: {"slug": "...", "plan": "...", "changes": [...], "breaks": [...], "blocking": bool}
//   - Field order inside objects is stable (Diff's struct tag order).
//   - Field order inside Change.Field is sorted ASC at render time so
//     `jq '.changes[] | .field'` is reproducible across deploys.
//   - Breaks sorted by Code ASC, errors before warns.
//
// The JSON path is the CI gate input (`gregale deploy --diff --json |
// jq '.blocking'`). Per memory `sdk-doreq-retry-after-copy-gotcha`,
// stable wire shape matters more than human aesthetics — a CI
// script depends on every field name and position.
//
// The output is indented for human inspection; a CI consumer
// should pipe through `jq -c` if it wants one-line records.
func RenderJSON(w io.Writer, d Diff) error {
	// Sort changes by field for stable wire shape.
	d.Changes = sortedChanges(d.Changes)
	// Sort breaks by code for stable wire shape; errors first.
	d.Breaks = sortedBreaks(d.Breaks)
	// Wrap so the gate's blocking bool is explicit on the wire.
	// A CI consumer reading `.blocking` doesn't have to re-scan
	// `.breaks[]` and pick the max severity.
	out := struct {
		Diff     Diff   `json:"diff"`
		Blocking bool   `json:"blocking"`
		Slug     string `json:"slug"`
		Plan     Plan   `json:"plan"`
	}{
		Diff:     d,
		Blocking: d.HasBlockingBreaks(),
		Slug:     d.Slug,
		Plan:     d.Plan,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// sortedChanges returns a copy of changes sorted by Field ASC.
// Stable, idempotent — repeated calls produce identical output.
func sortedChanges(in []Change) []Change {
	out := append([]Change(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Field > out[j].Field; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// sortedBreaks returns errors first, then warns, each sorted by
// Code ASC. Stable across deploys so CI gating is reproducible.
// Delegates to splitBreaksBySeverity (render_text.go) so the JSON
// and text renderers share one severity-partition implementation.
func sortedBreaks(in []Break) []Break {
	errs, warns := splitBreaksBySeverity(in)
	return append(errs, warns...)
}
