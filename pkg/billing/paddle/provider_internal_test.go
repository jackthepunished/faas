package paddle

// Internal-package tests for the Paddle provider constructor. Lives
// in `package paddle` (not paddle_test) so it can reach the
// package-private paddleNew / paddleNewSandbox function variables
// that NewProvider delegates to. The seam mirrors FlushFnForTest
// (provider.go:250) and SignForTestForTest (webhook.go).
//
// TestNewProvider_PropagatesSDKInitError pins the B2 release
// invariant: when the underlying paddle-go SDK constructor returns
// an error, NewProvider must surface it as a returned error and the
// loader must refuse to start the daemon. Today the SDK's New /
// NewSandbox only fail on programmer error (invalid options) — but
// the operator should never see a half-constructed *Provider that
// later trips a per-method nil-guard at the first customer request.

import (
	"errors"
	"strings"
	"testing"

	"github.com/PaddleHQ/paddle-go-sdk/v5"
)

func TestNewProvider_PropagatesSDKInitError(t *testing.T) {
	t.Parallel()

	// Save and restore the seam so the stub doesn't leak to other
	// tests in the package. Paddle's package-level test binary
	// (sandbox_test.go) shares this process; same-process tests
	// can run in any order under -parallel.
	origSandbox := paddleNewSandbox
	origProd := paddleNew
	t.Cleanup(func() {
		paddleNewSandbox = origSandbox
		paddleNew = origProd
	})

	stubErr := errors.New("simulated SDK init failure")
	paddleNewSandbox = func(string, ...paddle.Option) (*paddle.SDK, error) {
		return nil, stubErr
	}
	paddleNew = func(string, ...paddle.Option) (*paddle.SDK, error) {
		return nil, stubErr
	}

	p, err := NewProvider("test-key", "test-secret", true, nil)
	if err == nil {
		t.Fatal("NewProvider returned nil error; expected wrapped init error")
	}
	if p != nil {
		t.Errorf("NewProvider returned non-nil *Provider (%v) on init error", p)
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("err = %v; want errors.Is(err, stubErr) to be true (wrapped)", err)
	}
	if !strings.Contains(err.Error(), "paddle: SDK init") {
		t.Errorf("err message = %q; want it to carry the \"paddle: SDK init\" prefix for operator debugging", err.Error())
	}

	// NewProviderWithDedupe must propagate the same error — its body
	// is `p, err := NewProvider(...); if err != nil { return nil, err }`.
	p, err = NewProviderWithDedupe("test-key", true, nil, nil)
	if err == nil {
		t.Fatal("NewProviderWithDedupe returned nil error; expected wrapped init error")
	}
	if p != nil {
		t.Errorf("NewProviderWithDedupe returned non-nil *Provider (%v) on init error", p)
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("NewProviderWithDedupe err = %v; want errors.Is(err, stubErr)", err)
	}
}
