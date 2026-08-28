// Package apidgrpc — auth client (ADR-127 PR-D).
//
// gatewayd-public dials apid's Auth service over the unix socket
// to translate a Bearer token into the matching (account_id, plan)
// tuple so the OTel spans writer can enforce per-plan telemetry
// caps without holding a direct Postgres connection (CLAUDE.md
// ownership: apid is the sole writer to api_keys; gatewayd-public
// never opens a direct connection for this path).
//
// The client surface mirrors RequestTelemetryClientImpl (PR-B):
// same dial pattern, same pkg/wire.DialContext, same lazy-conn
// model. The shape difference: Auth is a unary RPC (token lookup
// is rare on the hot path — the gateway caches the result for
// the duration of one OTLP POST envelope).

package apidgrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	apidpb "github.com/onebox-faas/faas/api/proto/onebox/faas/apid/v1"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
)

// AuthClient is the gatewayd-public-side interface the OTel
// handler holds. The interface is the union of every gRPC call
// gatewayd-public makes against apid's Auth service.
type AuthClient interface {
	// AuthenticateKey resolves a bearer token to the matching
	// (account_id, plan) tuple. The token is the raw value
	// from the customer's OTLP POST Authorization header (i.e.
	// the value AFTER `Bearer ` is stripped by the caller).
	// Errors:
	//   - nil error + (account_id="", plan="") means the token
	//     resolved to a known account (no error) but the
	//     response fields were empty — should not happen in
	//     practice; the handler treats it as a 401.
	//   - non-nil error maps to either Unauthenticated (hash
	//     miss / expired / revoked) or Internal (Postgres trip).
	//     The handler translates to 401 / 500 respectively.
	AuthenticateKey(ctx context.Context, token string) (accountID string, plan string, err error)
	Close() error
}

// AuthClientImpl is the production implementation of AuthClient.
// It owns the lazy gRPC connection to apid's unix socket.
type AuthClientImpl struct {
	conn *grpc.ClientConn
	cli  apidpb.AuthClient
}

// compile-time assertion that *AuthClientImpl satisfies AuthClient.
var _ AuthClient = (*AuthClientImpl)(nil)

// DialAuth opens a lazy gRPC connection to apid's unix socket
// for the Auth service. Same auth model as DialRequestTelemetry:
// socket DAC mode 0660 group `faas` is the only auth in v1.0;
// transport uses insecure credentials over a trusted local
// socket. Connection dials on first RPC; Dial never blocks on
// apid being up.
func DialAuth(ctx context.Context, target string, tlsCfg *tls.Config) (*AuthClientImpl, error) {
	if target == "" {
		return nil, errors.New("apidgrpc: empty apid target for auth")
	}
	conn, err := wire.DialContext(ctx, target, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("apidgrpc: dial apid %q for auth: %w", target, err)
	}
	return &AuthClientImpl{conn: conn, cli: apidpb.NewAuthClient(conn)}, nil
}

// NewAuthClient wraps an already-dialed connection (used by
// bufconn tests in pkg/apidgrpc/).
func NewAuthClient(conn *grpc.ClientConn) *AuthClientImpl {
	return &AuthClientImpl{conn: conn, cli: apidpb.NewAuthClient(conn)}
}

// AuthenticateKey resolves the bearer token to (account_id, plan).
// On a Unauthenticated RPC error the gateway treats the result as
// 401; on an Internal RPC error it logs + drops.
func (c *AuthClientImpl) AuthenticateKey(ctx context.Context, token string) (string, string, error) {
	resp, err := c.cli.AuthenticateKey(ctx, &apidpb.AuthenticateKeyRequest{Token: token})
	if err != nil {
		return "", "", fmt.Errorf("apidgrpc: AuthenticateKey: %w", err)
	}
	return resp.GetAccountId(), resp.GetPlan(), nil
}

// Close shuts down the underlying gRPC connection. Safe to call
// multiple times; subsequent calls are no-ops.
func (c *AuthClientImpl) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
