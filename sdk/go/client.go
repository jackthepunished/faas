package faas

import (
	"log/slog"
	"time"

	"github.com/poyrazK/faas-go/internal/api"
)

// Client is the public SDK client. It embeds *internal/api.Client, so
// every method on the internal client is reachable as a method on the
// public Client. The wrapper exists to (a) keep the public surface
// decoupled from the internal package — when PR 12 splits pkg/api,
// the public package's only dependency is the embedded *api.Client
// and the *APIError wire shape; (b) allow options-pattern construction
// without touching the internal Client; (c) carry the package-level
// public types (Errors sentinels, IdempotencyKey, Decoder) without
// re-exporting 60+ internal DTOs.
type Client struct {
	*api.Client
	// log is the slog logger attached via WithLogger. nil means
	// "no logging"; PR 4 installs the actual logging round-tripper
	// that invokes it.
	log *slog.Logger
	// retryMax and retryBackoff are set by WithRetry. PR 4 wires
	// the actual retry round-tripper that reads them. Stored on
	// the Client (not in package-level vars) so each Client gets
	// its own policy without a global.
	retryMax     int
	retryBackoff time.Duration
}

// NewClient builds a Client for baseURL with the given bearer token.
// Pass functional Options to customize HTTP transport, retry policy,
// deploy timeout, logger, or pre-set the token / base URL.
//
// An empty token disables the Authorization header. The device-code
// flow and the public status page are the only operations that work
// without auth.
//
//	c, err := faas.NewClient("https://api.example.com", os.Getenv("FAAS_TOKEN"),
//	    faas.WithDeployTimeout(5*time.Minute),
//	    faas.WithLogger(slog.Default()),
//	)
func NewClient(baseURL, token string, opts ...Option) (*Client, error) {
	// Build the internal Client first. Default timeout is 30s, matching
	// the existing daemon-side behavior. Options can replace http /
	// deployHTTP / token / baseURL after construction.
	inner := api.NewClient(baseURL, token)

	// Wrap into the public Client so the option chain can mutate
	// fields on the embedded *api.Client (baseURL, token, http,
	// deployHTTP) without ever exposing those internals.
	c := &Client{Client: inner}

	// Run the option chain FIRST. WithHTTPClient replaces
	// HTTPClient().Transport with the caller's; the idempotency
	// shim must be installed AFTER that so it sits inside the
	// caller's transport (innermost), injecting the Idempotency-Key
	// header before the caller's retry or auth middleware runs.
	// Installing the shim first would let WithHTTPClient overwrite
	// it with the caller's Transport — silently dropping the opt-in
	// key contract for callers who pass &http.Client{Timeout: …}.
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	// Install the idempotency round-tripper LAST. It wraps whatever
	// Transport the option chain left behind (the caller's custom
	// Transport, or http.DefaultTransport if WithHTTPClient wasn't
	// supplied) so the opt-in key contract holds in both cases.
	c.Client.HTTPClient().Transport = newIdempotencyRoundTripper(c.Client.HTTPClient().Transport)

	return c, nil
}

// APIError is re-exported as a type alias so callers can write
// errors.As(err, &faas.APIError{}) without importing the internal
// package. The wire shape is defined in internal/api/apierror.go.
type APIError = api.APIError

// Problem is re-exported as a type alias. It carries the canonical
// RFC 7807 fields (Type, Title, Status, Code, Detail) plus the
// platform-specific extensions (Limit, Observed, DocsURL,
// BillingPortalURL, PaddleCheckoutURL, TxID).
type Problem = api.Problem

// ErrNoBody is re-exported as a value alias so errors.Is(err,
// faas.ErrNoBody) works without reaching into internal/api.
var ErrNoBody = api.ErrNoBody
