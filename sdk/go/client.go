package faas

import (
	"log/slog"

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
	// "no logging"; the Client substitutes a no-op logger on read.
	log *slog.Logger
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

	// Install the idempotency round-tripper BEFORE the option chain
	// runs so that any HTTP client set via WithHTTPClient wraps the
	// idempotency shim — the shim is the innermost transport, so it
	// injects the Idempotency-Key header before any retry or auth
	// middleware the caller layers on top.
	c.Client.HTTPClient().Transport = newIdempotencyRoundTripper(c.Client.HTTPClient().Transport)

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// APIError is re-exported as a type alias so callers can write
// errors.As(err, &faas.APIError{}) without importing the internal
// package. The wire shape is defined in internal/api/apierror.go.
type APIError = api.APIError

// Problem is re-exported as a type alias. It carries the canonical
// RFC 7807 fields (Status, Code, Title, Detail, Type, Instance) plus
// the platform-specific extensions (Limit, Observed, Docs).
type Problem = api.Problem

// ErrNoBody is re-exported as a value alias so errors.Is(err,
// faas.ErrNoBody) works without reaching into internal/api.
var ErrNoBody = api.ErrNoBody
