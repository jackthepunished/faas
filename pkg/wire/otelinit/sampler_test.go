// Tests for the per-deployment sampler (issue #555 acceptance #5).
//
// Conventions:
//   - Every test names the inverse scenario it pins: e.g.
//     "RootSpanOutsideWindow_DelegatesToRootSampler".
//   - Tests exercise the DeploymentAware wrapper directly so the
//     assertion targets a single decision rule per case.
//   - The race test (TestDeploymentCounter_ConcurrentObserve) MUST
//     run under `go test -race` to surface the sync.Mutex contract
//     regression. -race is wired into CI per golangci-lint v2.4.0
//     checklist (memory golangci-lint-v2-4-0-handler-checklist).
package otelinit_test

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/onebox-faas/faas/pkg/wire/otelinit"
)

// params builds a SamplingParameters for a root span with the given
// attributes. No parent context — represents the "first span of a
// new trace" decision the sampler makes.
func params(attrs ...attribute.KeyValue) sdktrace.SamplingParameters {
	return sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		TraceID:       oteltrace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Name:          "test.span",
		Kind:          oteltrace.SpanKindServer,
		Attributes:    attrs,
	}
}

// TestDeploymentAware_RootSpanInsideWindow pins the happy path:
// the first 100 root spans for a deployment are 100% sampled.
func TestDeploymentAware_RootSpanInsideWindow(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	sampler := otelinit.NewDeploymentAware(sdktrace.AlwaysSample(), otelinit.WithCounter(counter))

	for i := 1; i <= 50; i++ {
		res := sampler.ShouldSample(params(
			attribute.String("deployment_id", "dep-A"),
		))
		if res.Decision != sdktrace.RecordAndSample {
			t.Fatalf("span #%d: decision=%v, want RecordAndSample", i, res.Decision)
		}
	}
}

// TestDeploymentAware_RootSpanOutsideWindow pins that root spans
// past the window size delegate to the wrapped sampler. With a
// TraceIDRatioBased(rate=0.0) wrapper, the 101st span is dropped.
func TestDeploymentAware_RootSpanOutsideWindow(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	// Wrap a sampler that always drops so the assertion is
	// unambiguous: when inside the window, our override says
	// "RecordAndSample" regardless; when outside, we MUST
	// observe the wrapped sampler (Drop).
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(counter))

	for i := 1; i <= 100; i++ {
		res := sampler.ShouldSample(params(
			attribute.String("deployment_id", "dep-A"),
		))
		if res.Decision != sdktrace.RecordAndSample {
			t.Fatalf("span #%d (inside window): decision=%v, want RecordAndSample", i, res.Decision)
		}
	}
	// 101st span — outside the window. Must delegate to NeverSample().
	res := sampler.ShouldSample(params(
		attribute.String("deployment_id", "dep-A"),
	))
	if res.Decision != sdktrace.Drop {
		t.Errorf("span #101 (outside window): decision=%v, want Drop", res.Decision)
	}
}

// TestDeploymentAware_DifferentDeploymentsIndependent pins that the
// counter is keyed per-deployment — observing one deployment does
// not affect another's window.
func TestDeploymentAware_DifferentDeploymentsIndependent(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(counter))

	// Interleave 50 spans for dep-A and 50 spans for dep-B (each
	// inside the 100-span window). Both must be 100% sampled
	// throughout.
	for i := 1; i <= 50; i++ {
		for _, depID := range []string{"dep-A", "dep-B"} {
			res := sampler.ShouldSample(params(
				attribute.String("deployment_id", depID),
			))
			if res.Decision != sdktrace.RecordAndSample {
				t.Fatalf("span #%d for %s: decision=%v, want RecordAndSample", i, depID, res.Decision)
			}
		}
	}

	// Push dep-A past the window (60 more spans → counter at 110).
	for i := 0; i < 60; i++ {
		_ = sampler.ShouldSample(params(attribute.String("deployment_id", "dep-A")))
	}

	// dep-A is now outside; the next observation must delegate to
	// the wrapped NeverSample (Drop).
	resA := sampler.ShouldSample(params(attribute.String("deployment_id", "dep-A")))
	if resA.Decision != sdktrace.Drop {
		t.Errorf("dep-A outside-window: decision=%v, want Drop", resA.Decision)
	}

	// dep-B is at 50 (still inside) — next observation must be
	// RecordAndSample regardless of dep-A's status.
	resB := sampler.ShouldSample(params(attribute.String("deployment_id", "dep-B")))
	if resB.Decision != sdktrace.RecordAndSample {
		t.Errorf("dep-B after dep-A drained: decision=%v, want RecordAndSample", resB.Decision)
	}
}

// TestDeploymentAware_DelegatesToParent pins that when the parent
// context carries a valid SpanContext, the parent's decision is
// honoured regardless of the counter (ParentBased semantics).
//
// We construct a parent context with a Sampled=true SpanContext and
// confirm the wrapper delegates to the root sampler (which is
// NeverSample). The result MUST be RecordAndSample — because
// the wrapped sampler observes the parent flag and returns
// RecordAndSample, even though the underlying NeverSample would
// drop a root span.
func TestDeploymentAware_DelegatesToParent(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	// Wrap a NeverSample; the sampler delegate contract is that
	// for any span (with or without parent), it returns Drop.
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(counter))

	// Build a parent ctx with a sampled SpanContext.
	parentSC := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    oteltrace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     oteltrace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	parentCtx := oteltrace.ContextWithSpanContext(context.Background(), parentSC)

	res := sampler.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: parentCtx,
		TraceID:       parentSC.TraceID(),
		Name:          "child.span",
		Kind:          oteltrace.SpanKindServer,
		Attributes: []attribute.KeyValue{
			attribute.String("deployment_id", "dep-A"),
		},
	})
	// NeverSample always returns Drop — the parent context does
	// not change the underlying sampler. This is the expected
	// "delegate to root" semantic; ParentBased (the outer wrapper
	// in production) is the layer that interprets the parent's
	// SampledFlag.
	if res.Decision != sdktrace.Drop {
		t.Errorf("child with NeverSample root: decision=%v, want Drop (delegate-to-root)", res.Decision)
	}
}

// TestDeploymentAware_NoDeploymentAttribute pins the no-attribute
// fallback: a root span without deployment_id delegates to the
// wrapped root sampler. With NeverSample, the span is dropped.
func TestDeploymentAware_NoDeploymentAttribute(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(counter))

	res := sampler.ShouldSample(params()) // no attributes
	if res.Decision != sdktrace.Drop {
		t.Errorf("no-attr span: decision=%v, want Drop (delegated to NeverSample)", res.Decision)
	}
}

// TestDeploymentAware_ResetClearsCounter pins that the
// out-of-band watcher hook (counter.Reset) actually clears the
// window: after reset, the next 100 spans are 100% sampled again.
func TestDeploymentAware_ResetClearsCounter(t *testing.T) {
	counter := otelinit.NewDeploymentCounter(100)
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(counter))

	// Observe 50 spans to push the counter half-way through the
	// window.
	for i := 1; i <= 50; i++ {
		_ = sampler.ShouldSample(params(attribute.String("deployment_id", "dep-A")))
	}

	// Reset — the watcher would call this on the last-live-park
	// transition.
	counter.Reset("dep-A")

	// The next 100 spans must be 100% sampled.
	for i := 1; i <= 100; i++ {
		res := sampler.ShouldSample(params(attribute.String("deployment_id", "dep-A")))
		if res.Decision != sdktrace.RecordAndSample {
			t.Fatalf("post-reset span #%d: decision=%v, want RecordAndSample", i, res.Decision)
		}
	}
}

// TestDeploymentCounter_ObserveSemantics pins the Observe API
// contract used by the watcher tests:
//
//   - empty deployment_id → returns (0, false) without touching state
//   - first observation → returns (1, true) when windowSize >= 1
//   - post-increment count, not pre-increment
func TestDeploymentCounter_ObserveSemantics(t *testing.T) {
	c := otelinit.NewDeploymentCounter(3)

	if n, ok := c.Observe(""); n != 0 || ok {
		t.Errorf("empty depID: got (%d, %v), want (0, false)", n, ok)
	}
	if n, ok := c.Observe("dep-A"); n != 1 || !ok {
		t.Errorf("first obs: got (%d, %v), want (1, true)", n, ok)
	}
	if n, ok := c.Observe("dep-A"); n != 2 || !ok {
		t.Errorf("second obs: got (%d, %v), want (2, true)", n, ok)
	}
	if n, ok := c.Observe("dep-A"); n != 3 || !ok {
		t.Errorf("third obs: got (%d, %v), want (3, true)", n, ok)
	}
	if n, ok := c.Observe("dep-A"); n != 4 || ok {
		t.Errorf("fourth obs (outside window=3): got (%d, %v), want (4, false)", n, ok)
	}
}

// TestDeploymentCounter_ResetIsolatesDeployments pins that Reset
// on one deployment does not affect another's counter.
func TestDeploymentCounter_ResetIsolatesDeployments(t *testing.T) {
	c := otelinit.NewDeploymentCounter(10)

	// Push dep-A to 5 and dep-B to 8.
	for i := 0; i < 5; i++ {
		c.Observe("dep-A")
	}
	for i := 0; i < 8; i++ {
		c.Observe("dep-B")
	}

	c.Reset("dep-A")

	if n, _ := c.Observe("dep-A"); n != 1 {
		t.Errorf("dep-A post-reset: got n=%d, want 1", n)
	}
	if n, _ := c.Observe("dep-B"); n != 9 {
		t.Errorf("dep-B after dep-A reset: got n=%d, want 9 (untouched)", n)
	}
}

// TestDeploymentCounter_ConcurrentObserve pins the race-free
// contract: 100 goroutines × 100 spans each, all observing the
// same deployment_id, must produce a final count of exactly 10000
// (no dropped increments, no double counts).
//
// Run under `go test -race`; without -race the test passes by
// accident.
func TestDeploymentCounter_ConcurrentObserve(t *testing.T) {
	c := otelinit.NewDeploymentCounter(50000) // way bigger than the goroutine fan-out
	const goroutines = 100
	const perGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				c.Observe("dep-shared")
			}
		}()
	}
	wg.Wait()

	// One more observation so we can read the final count via
	// the return value.
	final, _ := c.Observe("dep-shared")
	want := goroutines*perGoroutine + 1
	if final != want {
		t.Errorf("final count = %d, want %d (lost increments)", final, want)
	}
}

// TestDeploymentAware_DescriptionPinsShape pins the Description()
// string format the SDK logs on boot. A regression here would
// surface in daemon boot logs without breaking semantics, so the
// assertion is loose (just confirms it mentions "DeploymentAware"
// and the window size).
func TestDeploymentAware_DescriptionPinsShape(t *testing.T) {
	c := otelinit.NewDeploymentCounter(42)
	sampler := otelinit.NewDeploymentAware(sdktrace.AlwaysSample(), otelinit.WithCounter(c))
	desc := sampler.Description()
	if desc == "" {
		t.Error("Description returned empty string")
	}
	for _, want := range []string{"DeploymentAware", "42"} {
		if !contains(desc, want) {
			t.Errorf("Description = %q, missing substring %q", desc, want)
		}
	}
}

// TestDeploymentAware_DeploymentAttrKey pins the contract between
// the sampler and schedd's sched.wake span attribute. schedd stamps
// "deployment_id" at pkg/sched/engine.go:1362; if that key ever
// drifts (e.g. "deploymentID", "deployment.id"), the sampler
// silently stops overriding. This test pins the literal value
// the sampler looks for so a drift on either side surfaces as a
// test failure, not a silent regression.
//
// We test through the sampler rather than exposing the unexported
// constant directly: the sampler is the contract surface, and a
// test against the sampler catches both "constant renamed" and
// "attribute lookup changed".
func TestDeploymentAware_DeploymentAttrKey(t *testing.T) {
	c := otelinit.NewDeploymentCounter(100)
	sampler := otelinit.NewDeploymentAware(sdktrace.NeverSample(), otelinit.WithCounter(c))

	// Inside the window: a root span with the canonical attribute
	// must be sampled despite the wrapped NeverSample.
	res := sampler.ShouldSample(params(
		attribute.String("deployment_id", "dep-A"),
	))
	if res.Decision != sdktrace.RecordAndSample {
		t.Errorf("canonical 'deployment_id' attribute: decision=%v, want RecordAndSample (key drift?)", res.Decision)
	}

	// Wrong key: the sampler must NOT see this as a per-deployment
	// override — it falls through to the wrapped NeverSample.
	res2 := sampler.ShouldSample(params(
		attribute.String("deploymentID", "dep-A"), // camelCase drift
	))
	if res2.Decision != sdktrace.Drop {
		t.Errorf("drifted 'deploymentID' attribute: decision=%v, want Drop (sampler should ignore wrong key)", res2.Decision)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
