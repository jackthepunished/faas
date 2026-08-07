// Sealed-secret adapter shim for the gateway package. The
// gateway package must NOT import pkg/secretbox directly to
// avoid the cycle (pkg/secretbox imports pkg/wire which
// pkg/gateway also imports). Instead, cmd/gatewayd-public
// wires OpenBytesDNSProvider at startup. The shim defaults to
// returning an error — a fresh binary panics loudly with a
// helpful message rather than silently no-op'ing the DNS
// handoff.
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
var secretboxOpenDNSProvider = OpenBytesDNSProvider