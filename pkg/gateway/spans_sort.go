// Package gateway — spans_sort.go (ADR-127 PR-D).
//
// sortSpansByDurationDesc sorts in place descending by DurationNanos.
// Kept in a separate file to avoid pulling "sort" into the
// accumulator file's imports for one call.

package gateway

import "sort"

func sortSpansByDurationDesc(s []summarizedSpan) {
	sort.Slice(s, func(i, j int) bool {
		return s[i].DurationNanos > s[j].DurationNanos
	})
}
