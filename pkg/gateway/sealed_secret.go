// Sealed-secret adapter shim for the gateway package. The
// gateway package must NOT import pkg/secretbox directly to
// avoid the cycle (pkg/secretbox imports pkg/wire which
// pkg/gateway also imports). Instead, cmd/gatewayd-public
// wires OpenBytesDNSProvider at startup. The shim defaults to
// returning errSecretBoxUnconfigured so a fresh binary that
// forgot to wire the unseal helper fails loudly at the FIRST
// DNS handoff attempt — not silently no-op'ing (review
// finding #6 corrected the previous docstring which claimed
// the shim "panics"; it returns an error).
//
// Precedent: pkg/webhook/secretbox_adapter.go (ADR-076).
package gateway

import (
	"errors"
	"fmt"
)

// errSecretBoxUnconfigured is the default OpenBytesDNSProvider
// value — returns an error so cmd/gatewayd-public's startup
// log surfaces "dns_provider_unconfigured" if the wiring was
// forgotten. The Hetzner DNS provider returns this error from
// its constructor; the orchestrator never gets a half-wired
// provider.
var errSecretBoxUnconfigured = errors.New("pkg/gateway: OpenBytesDNSProvider not configured — wire cmd/gatewayd-public/main.go at startup")

// OpenBytesDNSProvider is the namespace-sealed unseal helper
// for the DNS_PROVIDER namespace. Set by cmd/gatewayd-public
// at startup. The DNS_PROVIDER namespace is distinct from the
// webhook APP_WEBHOOK namespace (ADR-076) and the registry
// REGISTRY_AUTH namespace (ADR-062) so a leaked blob from one
// surface cannot decrypt another.
var OpenBytesDNSProvider = func(sealed []byte) ([]byte, error) {
	return nil, fmt.Errorf("%w (sealed=%d bytes)", errSecretBoxUnconfigured, len(sealed))
}

// secretboxOpenDNSProvider is the internal alias used by the
// Hetzner provider. Same shape — kept as a separate name so the
// wiring code in cmd/gatewayd-public can swap one without
// touching the other (a future v1.1 may route the DNS_PROVIDER
// unseal through a different namespace).
//
// REVIEW NOTE (finding #7, second review): this is a value
// bind, not a reference bind. If a future test reassigns
// `OpenBytesDNSProvider = ...`, `secretboxOpenDNSProvider`
// here keeps the original default-returning closure. The
// production wiring in cmd/gatewayd-public/main.go reassigns
// BOTH at startup to avoid the trap, but the indirection
// stays in place so swapping one without the other is a
// obvious-by-grep mistake rather than a silent no-op.
var secretboxOpenDNSProvider = OpenBytesDNSProvider
