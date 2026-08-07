// Sealed-secret adapter shim for the gateway package. The
// gateway package must NOT import pkg/secretbox directly to
// avoid the cycle (pkg/secretbox imports pkg/wire which
// pkg/gateway also imports). Instead, cmd/gatewayd-public
// wires OpenBytesDNSProvider at startup. The shim defaults to
// returning errSecretBoxUnconfigured so a fresh binary that
// forgot to wire the unseal helper fails loudly at the FIRST
// DNS handoff attempt — not silently no-op'ing.
//
// Real precedent: cmd/gatewayd-internal/public_auth_unsealer.go
// (an interface-shaped unsealer that loads host.age identities
// at boot and calls secretbox.OpenBytesMulti). The DNS provider
// uses the var-shaped seam because pkg/gateway must not import
// pkg/secretbox (cycle) AND the call site in main.go runs
// before DI structs are wired; the package-level var lets the
// reassignment happen at the top of run() with no helper.
package gateway

import (
	"errors"
	"fmt"
)

// errSecretBoxUnconfigured is the default OpenBytesDNSProvider
// value — returns an error so cmd/gatewayd-public's startup
// log surfaces "dns_provider_unconfigured" if the wiring was
// forgotten. The Cloudflare DNS provider returns this error
// from its constructor; the orchestrator never gets a
// half-wired provider.
var errSecretBoxUnconfigured = errors.New("pkg/gateway: OpenBytesDNSProvider not configured — wire cmd/gatewayd-public/main.go at startup")

// OpenBytesDNSProvider is the namespace-sealed unseal helper
// for the DNS_PROVIDER namespace. Set by cmd/gatewayd-public
// at startup. The DNS_PROVIDER namespace is distinct from the
// webhook APP_WEBHOOK namespace (ADR-076) and the registry
// REGISTRY_AUTH namespace (ADR-062) so a leaked blob from one
// surface cannot decrypt another.
//
// Signature: (sealed []byte) ([]byte, error). The Cloudflare
// provider round-trips through openDNSProviderToken which
// discards the namespace tag returned by secretbox.OpenBytesMulti
// (we've already routed by namespace at the wire level — the
// unseal call only fires for DNS_PROVIDER blobs).
var OpenBytesDNSProvider = func(sealed []byte) ([]byte, error) {
	return nil, fmt.Errorf("%w (sealed=%d bytes)", errSecretBoxUnconfigured, len(sealed))
}

// openDNSProviderToken is the var-shaped seam the DNS provider
// constructors call. By default it round-trips through
// OpenBytesDNSProvider (the externally-reassignable var). Tests
// reassign openDNSProviderToken directly to inject a fake
// unseal helper without touching the package-level var (the
// value-bind trap noted in this file's previous revision).
var openDNSProviderToken = func(sealed []byte) ([]byte, error) {
	return OpenBytesDNSProvider(sealed)
}
