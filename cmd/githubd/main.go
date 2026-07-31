// Command githubd — GitHub App integration daemon (spec §14 M7.5, ADR-012).
//
// githubd owns: push-webhook receiver, Checks-API status writer, OAuth
// callback handler, per-repo install-token cache. It is the SOLE outbound
// caller to api.github.com (Checks + install-token exchange); its inbound
// public surface is gatewayd at /webhooks/github (HMAC-verified at the
// edge). It talks to apid over gRPC on /run/faas/githubd.sock
// (ADR-015 unix-socket DAC; apid is the only caller in v1.0).
//
// Slice 7 wires the daemon body (gRPC + HTTP listeners). Slice 8
// arms the OAuth + token-cache + Checks path: builds an AppAuth
// from /etc/faas/secrets/github-app.{id,pem}, a TokenCache for
// installation access tokens, a ChecksAPI for the Checks writer,
// and a RealService that implements the full gRPC contract.
package main

import (
	"context"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
	"github.com/onebox-faas/faas/pkg/githubd"
	"github.com/onebox-faas/faas/pkg/githubdgrpc"
	"github.com/onebox-faas/faas/pkg/secretbox"
	"github.com/onebox-faas/faas/pkg/wire"
)

// runDeps is the DI seam so tests can swap openDB / configPath /
// AppAuth / readKeyPEM without touching Postgres, /run/faas, or
// /etc/faas/secrets.
type runDeps struct {
	configPath string
	openDB     func(context.Context, string) (*pgxpool.Pool, error)
	readAppID  func() string
	readKeyPEM func() ([]byte, error)
	httpClient func() githubd.HTTPClient
	now        func() time.Time
}

func defaultDeps() runDeps {
	return runDeps{
		configPath: "/etc/faas/githubd.toml",
		openDB:     db.Open,
		readAppID:  func() string { return os.Getenv("FAAS_GITHUB_APP_ID") },
		readKeyPEM: readKeyPEMDefault,
		httpClient: func() githubd.HTTPClient { return http.DefaultClient },
		now:        time.Now,
	}
}

func main() {
	wire.Daemon("githubd", run)
}

func run(ctx context.Context, log *slog.Logger) error {
	return runWithDeps(ctx, log, defaultDeps())
}

func runWithDeps(ctx context.Context, log *slog.Logger, deps runDeps) error {
	cfg, err := LoadConfig(deps.configPath)
	if err != nil {
		return fmt.Errorf("githubd: config: %w", err)
	}

	pool, err := deps.openDB(ctx, "")
	if err != nil {
		return fmt.Errorf("githubd: open db: %w", err)
	}
	defer pool.Close()

	// Slice 7 Service skeleton (inbound webhook path).
	webhookSvc := githubd.NewService(log)
	webhookSvc.Bindings = noopBindings{}

	// Slice 8 RealService (OAuth + Checks). Auth may be nil if
	// the GitHub App credentials aren't provisioned — the daemon
	// stays up but every OAuth / Checks call returns an error.
	// This is "fail-closed but stay-up": the webhook path
	// continues to work for any installation that's already
	// configured its webhook out-of-band.
	var realSvc *githubd.RealService
	if appID := deps.readAppID(); appID != "" {
		keyPEM, kerr := deps.readKeyPEM()
		if kerr != nil {
			log.Warn("githubd: read app private key", "err", kerr)
		} else {
			clientID := os.Getenv("FAAS_GITHUB_APP_CLIENT_ID")
			clientSecret := os.Getenv("FAAS_GITHUB_APP_CLIENT_SECRET")
			auth, aerr := githubd.NewAppAuth(appID, keyPEM, deps.httpClient(), clientID, clientSecret)
			if aerr != nil {
				log.Warn("githubd: app auth init", "err", aerr)
			} else {
				tokens := githubd.NewTokenCache(auth, 5*time.Minute)
				// BindingsLookup is the seam that closes review
				// finding #1+#2: pkg/state.Store owns the binding
				// table (migration 00007), and githubd's Checks
				// writer threads the right installation_id per
				// repo through it instead of hardcoding install=1.
				storeAdapter := newStateBindingsAdapter(pool)
				installsAdapter := newStateInstallsAdapter(pool)
				checks, cerr := githubd.NewChecksAPI(tokens, deps.httpClient(), storeAdapter)
				if cerr != nil {
					return fmt.Errorf("githubd: new checks api: %w", cerr)
				}
				// PR-C: load the host age keypair so the install
				// token can be sealed at rest (SealOne at mint)
				// and unsealed at cold-start rehydrate (Open).
				// LoadHostKey enforces 0o400 perms (strict
				// equality, MEMORY.md/host-age-0400-loadcredential-decouple);
				// failure here is fatal — without the identity
				// we can't unseal existing rows.
				identity, ierr := secretbox.LoadHostKey(hostKeyPath())
				if ierr != nil {
					return fmt.Errorf("githubd: load host age identity: %w", ierr)
				}
				// Issue #316 / ADR-057: also load the rotation-aware
				// multi-identity slice from the same dir so install
				// tokens sealed under the previous host.age remain
				// unsealed during the 30-day overlap window.
				// Degrade to single-identity (with a Warn) if
				// LoadHostKeys fails — the box still unseals
				// current-keyed envelopes, just not previous-keyed ones.
				var identities []*age.X25519Identity
				if dir := filepath.Dir(hostKeyPath()); dir != "" {
					if ids, loadErr := secretbox.LoadHostKeys(dir); loadErr != nil {
						log.Warn("githubd: LoadHostKeys (rotation overlap) failed; install-token unseal will work only for envelopes sealed under the current host.age",
							"dir", dir, "err", loadErr.Error())
					} else {
						identities = ids
						if len(identities) > 1 {
							log.Info("githubd: rotation overlap active — install-token unseal falls back across current + previous host.age")
						}
					}
				}
				recipient, rerr := loadHostPubKey()
				if rerr != nil {
					return fmt.Errorf("githubd: load host age recipient: %w", rerr)
				}
				auditFn := newGithubdAuditFn(log)
				realSvc = githubd.NewRealService(auth, tokens, checks, storeAdapter, installsAdapter, recipient, identity, auditFn)
				if identities != nil {
					realSvc.Identities = identities
				}
				log.Info("githubd: OAuth + Checks wired", "app_id", appID)
			}
		}
	} else {
		log.Info("githubd: FAAS_GITHUB_APP_ID unset; OAuth + Checks disabled (webhook path only)")
	}

	// The gRPC server hands out the RealService (full slice 8
	// surface) when available, else falls back to a Unimplemented
	// stub so the gRPC plumbing stays healthy even without OAuth.
	gRPCImpl := githubdgrpc.Service(githubdgrpc.UnimplementedService{})
	if realSvc != nil {
		gRPCImpl = realSvc
	}

	// ops: one per-daemon Prometheus registry shared by every
	// observer in githubd (gRPC handlers + the inbound webhook
	// push). WebhookLoopbackHandler mounts it at GET /metrics on
	// the loopback :8083 mux (§11 loopback-only invariant; gatewayd
	// only forwards POST /webhooks/github, so GET /metrics can't
	// leak externally).
	ops := wire.NewOpsMetrics("githubd")
	srv := &githubd.Server{
		Service:     webhookSvc,
		Log:         log,
		Ops:         ops,
		GRPCServer:  githubdgrpc.New(gRPCImpl, ops, log),
		HTTPAddr:    cfg.HTTPAddr,
		SocketPath:  cfg.SocketPath,
		ListenAddr:  cfg.ListenAddr,
		TLSCertPath: cfg.TLSCertPath,
		TLSKeyPath:  cfg.TLSKeyPath,
		TLSCAPath:   cfg.TLSCAPath,
	}
	cleanup, errc, err := srv.Start(ctx)
	if err != nil {
		return fmt.Errorf("githubd: start: %w", err)
	}
	//nolint:contextcheck // shutdown ctx must outlive caller ctx.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cleanup(shutdownCtx)
	}()

	// Start the token-cache janitor if RealService is armed.
	if realSvc != nil && realSvc.Tokens != nil {
		stopJanitor := realSvc.Tokens.StartJanitor(ctx)
		defer stopJanitor()
	}

	select {
	case err := <-errc:
		return fmt.Errorf("githubd: listener: %w", err)
	case <-ctx.Done():
		log.Info("githubd stopping")
		return nil
	}
}

// noopBindings is a placeholder until slice 8 introduces the
// bindings table. Every GetAppBinding returns an empty struct (the
// service treats empty BindingID as "no binding").
type noopBindings struct{}

func (noopBindings) GetAppBinding(_ context.Context, _, _ string) (githubdgrpc.AppBinding, error) {
	return githubdgrpc.AppBinding{}, nil
}

// readKeyPEMDefault reads the GitHub App private key from
// FAAS_GITHUB_APP_KEY_PATH (default /etc/faas/secrets/github-app.pem,
// mode 0400 per spec §11). Returns an error if the file is missing
// or unreadable.
func readKeyPEMDefault() ([]byte, error) {
	path := os.Getenv("FAAS_GITHUB_APP_KEY_PATH")
	if path == "" {
		path = "/etc/faas/secrets/github-app.pem"
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled
	if err != nil {
		return nil, fmt.Errorf("githubd: read app key %q: %w", path, err)
	}
	return data, nil
}

// hostKeyPath returns the path to the host age private key. Used
// by ensureInstallToken's cold-start rehydrate path to unseal
// stored install tokens. The default matches the rest of the
// secrets tree (spec §11: /etc/faas/secrets/host.age, mode 0400
// root:root). An operator can override via FAAS_HOST_AGE_KEY.
//
// Issue #316 / ADR-057: the previous default had a stray `.key`
// suffix (/etc/faas/secrets/host.age.key) that didn't match any
// other component's path. After a host.age rotation the rename
// would have moved the canonical file to host.age.previous, but
// githubd would have continued looking for host.age.key and
// silently failed every unseal. Reconciled to host.age here so
// LoadHostKeys(dir) (current + previous) returns the same pair
// every daemon consumes.
func hostKeyPath() string {
	if p := os.Getenv("FAAS_HOST_AGE_KEY"); p != "" {
		return p
	}
	return secretbox.DefaultHostKeyPath
}

// hostPubKeyPath returns the path to the host age public key. Used
// by ExchangeOAuthCode's seal-at-mint path. Mode 0444 expected.
func hostPubKeyPath() string {
	if p := os.Getenv("FAAS_HOST_AGE_PUB"); p != "" {
		return p
	}
	return "/etc/faas/secrets/host.age.pub"
}

// loadHostPubKey reads the host age public key from disk and
// parses it as an X25519 recipient. The public half is
// world-readable, so no perm check beyond the file being readable.
// (Strict 0o400 enforcement is on the PRIVATE half via LoadHostKey;
// the public half just needs to be readable to the daemon.)
func loadHostPubKey() (*age.X25519Recipient, error) {
	path := hostPubKeyPath()
	data, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled
	if err != nil {
		return nil, fmt.Errorf("githubd: read host age pub %q: %w", path, err)
	}
	id, err := age.ParseX25519Recipient(string(data))
	if err != nil {
		return nil, fmt.Errorf("githubd: parse host age pub %q: %w", path, err)
	}
	return id, nil
}

// newGithubdAuditFn returns the AuditEvent callback RealService
// invokes to emit auth.install.* events. Today it just JSON-logs
// at info level; a future wiring can forward to apid's audit
// event emitter (PR-D scope — the inbound-webhook path needs the
// same audit sink so the §11 paper trail is unified).
//
// The event names match the apid-side audit taxonomy
// (auth.install.verified / .token_sealed / .takeover_rejected /
// .unauthenticated from PR-A + PR-B + PR-C).
func newGithubdAuditFn(log *slog.Logger) githubd.AuditEvent {
	return func(event string, accountID string, payload map[string]any) {
		log.Info("githubd audit",
			"event", event,
			"account_id", accountID,
			"payload", payload)
	}
}

// Compile-time guards: keep imports stable for tests / future slices.
var (
	_ = rsa.PrivateKey{}
	_ = depsAdapter{}
)

// depsAdapter is reserved for the test seam in pkg/githubd tests
// that import cmd/githubd internals.
type depsAdapter struct{}
