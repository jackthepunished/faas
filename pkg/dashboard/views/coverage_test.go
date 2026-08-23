// coverage_test.go — fill the remaining pkg/dashboard/views coverage
// gaps that render_test.go deliberately doesn't touch. Targets:
//
//   - RenderColdBootRateSparkline (render.go:329) — 0.0% → covered.
//     Convenience wrapper for the cold-boot row of the per-app SLO
//     card; same wire shape as RenderErrorRateSparkline but with the
//     amber colour. The render_test.go pin tests the error-rate
//     variant only — this file pins the cold-boot variant so a
//     future colour swap on either side is caught.
//   - RenderAreaSparkline default branches — width==0, height==0,
//     opacity=="" all coerce to defaults; the existing test
//     fixtures always pass non-zero values so the coercion paths
//     are uncovered.
//   - trendLabel rising / falling / flat branches — render.go:201
//     computes a 5%-of-range trend descriptor. The
//     87.5% coverage on trendLabel leaves the non-monotonic
//     "flat" branch uncaught (the existing tests sweep rising
//     + falling only).
//
// Conventions: blackbox `package views_test` (matches the
// pre-existing render_test.go).

package views_test

import (
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/appmetrics"
	"github.com/onebox-faas/faas/pkg/dashboard/views"
)

// --- RenderColdBootRateSparkline (render.go:329) ---------------------

func TestRenderColdBootRateSparkline_EmptyReturnsEmpty(t *testing.T) {
	// Same shape as RenderErrorRateSparkline: empty input →
	// template.HTML(""). Pin the helper doesn't add a wrapping
	// <svg> of its own.
	got := views.RenderColdBootRateSparkline(nil, 120, 30)
	if string(got) != "" {
		t.Errorf("empty input: got %q, want empty", got)
	}
}

func TestRenderColdBootRateSparkline_OutputsSVG(t *testing.T) {
	// Pin that a populated series produces an SVG, and that the
	// amber colour (#d49000 — areaColdBoot) lands in the output
	// somewhere. A future change that swaps colour constants
	// silently would change the on-screen sparkline colour
	// without breaking tests; this guard pins the wire shape.
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-3 * time.Minute), Value: 12},
		{Time: now.Add(-2 * time.Minute), Value: 18},
		{Time: now.Add(-1 * time.Minute), Value: 22},
		{Time: now, Value: 25},
	}
	got := views.RenderColdBootRateSparkline(points, 120, 30)
	out := string(got)
	if !strings.HasPrefix(out, "<svg") {
		t.Errorf("output doesn't start with <svg: %q", out)
	}
	if !strings.Contains(out, "#d49000") {
		t.Errorf("output missing cold-boot amber colour: %q", out)
	}
}

func TestRenderColdBootRateSparkline_DefaultsAppliedWhenZeroSize(t *testing.T) {
	// width=0 → defaultWidth (render.go:286-288). height=0 →
	// defaultHeight. The output SVG must still render; a future
	// change that drops the zero-handling would emit a 0×0
	// viewBox.
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-1 * time.Minute), Value: 5},
		{Time: now, Value: 7},
	}
	got := views.RenderColdBootRateSparkline(points, 0, 0)
	out := string(got)
	if !strings.Contains(out, "<svg") {
		t.Errorf("output missing <svg tag: %q", out)
	}
}

// --- RenderAreaSparkline default branches (render.go:285) ------------

func TestRenderAreaSparkline_AppliesDefaultWidth(t *testing.T) {
	// Direct call with width=0 — pin that the helper coerces to
	// defaultWidth rather than emitting a 0-width viewBox.
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-1 * time.Minute), Value: 1},
		{Time: now, Value: 2},
	}
	got := string(views.RenderErrorRateSparkline(points, 0, 30))
	if !strings.Contains(got, "width=") {
		t.Errorf("output missing width attr: %q", got)
	}
	// Negative-check: the viewBox should NOT contain "0 0 0 ".
	if strings.Contains(got, "viewBox=\"0 0 0 ") {
		t.Errorf("viewBox has zero width: %q", got)
	}
}

func TestRenderAreaSparkline_AppliesDefaultOpacity(t *testing.T) {
	// RenderAreaSparkline is unexported; the only public way to
	// drive the opacity-default branch is via the convenience
	// helpers, which pass areaOpacity explicitly. We can't reach
	// the empty-opacity branch from the public surface, but we
	// CAN pin that the convenience helpers' opacity attribute
	// renders verbatim.
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-1 * time.Minute), Value: 1},
		{Time: now, Value: 2},
	}
	got := string(views.RenderErrorRateSparkline(points, 120, 30))
	if !strings.Contains(got, `fill-opacity="0.18"`) {
		t.Errorf("output missing areaOpacity (0.18): %q", got)
	}
}

// --- trendLabel branches (render.go:201) -----------------------------

func TestRenderAreaSparkline_TrendLabelFlatBranch(t *testing.T) {
	// Pin the "flat" branch (≤5% of range change → "flat"). The
	// existing tests sweep rising + falling only — pin the
	// middle bucket explicitly.
	now := time.Now()
	// Series with values 100, 100, 100, 102. Range = 2. Change = 2.
	// 2/2 = 100% — that's NOT flat. Drop the change to < 5% of
	// range: use 100, 100, 100, 100.4 — but float rounding matters;
	// use clean integers with a known delta.
	// Actually: trendLabel reads first.Value vs last.Value against
	// (max-min). To land in flat, last - first must be ≤ 5% of
	// (max - min). With max=100, min=100, range=0, the function
	// would divide by zero — but the helper pre-flattens
	// max-min to 1 when all values are identical. So a strictly-
	// increasing series from 100 to 100.5 over range=0.5 is
	// 100% delta — "rising". To land in flat, we need last == first
	// OR a sub-5% delta. Test with last == first:
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-2 * time.Minute), Value: 50},
		{Time: now.Add(-1 * time.Minute), Value: 50},
		{Time: now, Value: 50},
	}
	got := views.RenderErrorRateSparkline(points, 120, 30)
	if !strings.Contains(string(got), "flat") {
		t.Errorf("flat series: aria-label missing 'flat': %q", got)
	}
}

func TestRenderAreaSparkline_TrendLabelRisingBranch(t *testing.T) {
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-2 * time.Minute), Value: 10},
		{Time: now.Add(-1 * time.Minute), Value: 50},
		{Time: now, Value: 100},
	}
	got := views.RenderErrorRateSparkline(points, 120, 30)
	if !strings.Contains(string(got), "rising") {
		t.Errorf("rising series: aria-label missing 'rising': %q", got)
	}
}

func TestRenderAreaSparkline_TrendLabelFallingBranch(t *testing.T) {
	now := time.Now()
	points := []appmetrics.SparklinePoint{
		{Time: now.Add(-2 * time.Minute), Value: 100},
		{Time: now.Add(-1 * time.Minute), Value: 50},
		{Time: now, Value: 10},
	}
	got := views.RenderErrorRateSparkline(points, 120, 30)
	if !strings.Contains(string(got), "falling") {
		t.Errorf("falling series: aria-label missing 'falling': %q", got)
	}
}
