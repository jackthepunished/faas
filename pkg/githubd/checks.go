// Checks API writer (slice 8, ADR-012).
//
// Every state transition in the build pipeline writes a check-run
// back to GitHub so the commit's "✓" / "✗" icon updates
// immediately. The phase mapping is:
//
//	CheckPhaseQueued    → "queued"
//	CheckPhaseBuilding  → "in_progress"
//	CheckPhaseLive      → "completed" / "success"
//	CheckPhaseFailed    → "completed" / "failure"
//
// GitHub requires idempotent check-run writes to avoid creating
// duplicates on retry. We use the (repo, sha, phase) tuple as the
// dedup key — the same phase transition for the same commit is
// always the same check-run; subsequent calls hit
// PATCH /repos/{owner}/{repo}/check-runs/{id} instead of POSTing
// a new one.
//
// Idempotency storage lives in pkg/state (slice 8 adds the table
// to migration 00006).
package githubd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/onebox-faas/faas/pkg/githubdgrpc"
)

// ChecksWriter is the business seam. The real impl is ChecksAPI;
// tests inject a recording fake.
type ChecksWriter interface {
	WriteCheck(ctx context.Context, repoFullName, commitSHA string, phase githubdgrpc.CheckPhase, logsURL, summary string) error
}

// BindingsLookup is the seam that closes review finding #1+#2: it
// maps a repo full-name to the installation_id whose per-install
// access token the Checks writer should use. Production wires it
// to pkg/state.Store.InstallationIDForRepo; tests can pass a stub
// that returns a fixed id or ErrNotFound.
//
// The split-out interface (rather than depending on pkg/state
// directly) keeps the githubd package independent of the apid
// package's persistence layer — a slice 8 architectural decision
// that survives even after the bindings live in Postgres.
type BindingsLookup interface {
	InstallationIDForRepo(ctx context.Context, repoFullName string) (int64, error)
}

// ChecksAPI writes check-runs to api.github.com.
type ChecksAPI struct {
	Tokens   *TokenCache // provides the installation token per installation_id
	HTTP     HTTPClient
	Bindings BindingsLookup // repo → installation_id (review finding #1+#2 closure)
}

// NewChecksAPI builds a ChecksAPI. tokens may be nil for tests
// that don't exercise the HTTP path. bindings may be nil only when
// tokens is also nil — the gRPC checks path always needs both.
// We refuse the (nil, nil) combo explicitly so a missing wiring
// fails fast at startup rather than at first check-run write.
func NewChecksAPI(tokens *TokenCache, hc HTTPClient, bindings BindingsLookup) (*ChecksAPI, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	if tokens == nil && bindings != nil {
		return nil, fmt.Errorf("githubd: ChecksAPI: tokens=nil with bindings!=nil is not a valid configuration")
	}
	return &ChecksAPI{Tokens: tokens, HTTP: hc, Bindings: bindings}, nil
}

// checkRunRequest is the body shape POST /repos/{o}/{r}/check-runs
// expects. We only fill the fields github cares about for the
// commit-icon update.
type checkRunRequest struct {
	Name       string          `json:"name"`
	HeadSHA    string          `json:"head_sha"`
	Status     string          `json:"status"`
	Conclusion string          `json:"conclusion,omitempty"`
	DetailsURL string          `json:"details_url,omitempty"`
	Output     *checkRunOutput `json:"output,omitempty"`
	ExternalID string          `json:"external_id,omitempty"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// checkRunResponse is the shape GitHub returns from POST/PATCH.
type checkRunResponse struct {
	ID int64 `json:"id"`
}

// WriteCheck posts a check-run for (repo, sha, phase). Idempotency
// is the caller's responsibility — this method always creates a
// new check-run; the StateStore-wrapped variant (NewStatefulChecks)
// is the one slice 8 callers should use.
func (c *ChecksAPI) WriteCheck(ctx context.Context, repoFullName, commitSHA string, phase githubdgrpc.CheckPhase, logsURL, summary string) error {
	if repoFullName == "" || commitSHA == "" {
		return fmt.Errorf("githubd: repo and sha required for check-run")
	}
	tokens, err := c.tokensForRepo(ctx, repoFullName)
	if err != nil {
		return err
	}
	body, err := json.Marshal(checkRunRequest{
		Name:       "faas / build",
		HeadSHA:    commitSHA,
		Status:     phaseToStatus(phase),
		Conclusion: phaseToConclusion(phase),
		DetailsURL: logsURL,
		Output: &checkRunOutput{
			Title:   phaseTitle(phase),
			Summary: summary,
		},
		ExternalID: fmt.Sprintf("faas/%s/%s", repoFullName, commitSHA),
	})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/check-runs", GitHubAPI, repoFullName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tokens)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "faas-githubd/1.0")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("githubd: write check-run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("githubd: write check-run: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out checkRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("githubd: decode check-run response: %w", err)
	}
	return nil
}

// tokensForRepo resolves the installation token for the repo's
// installation. This used to hardcode installation_id=1 (every
// account shared the same install); review finding #1+#2 forces
// the reverse-lookup via BindingsLookup so each repo gets its own
// install token (or we fail closed with an explicit error rather
// than sending the request as the wrong account).
//
// Returns an error when the BindingsLookup is unset, when no app
// is bound to the repo, or when the per-install token exchange
// fails. We deliberately do NOT fall back to installation_id=1:
// §11 least-privilege forbids one customer's check-run from
// shipping under another customer's installation.
func (c *ChecksAPI) tokensForRepo(ctx context.Context, repoFullName string) (string, error) {
	if c.Tokens == nil {
		return "", fmt.Errorf("githubd: token cache not configured (slice 8)")
	}
	if c.Bindings == nil {
		return "", fmt.Errorf("githubd: bindings lookup not configured (review finding #1+#2)")
	}
	installID, err := c.Bindings.InstallationIDForRepo(ctx, repoFullName)
	if err != nil {
		if errors.Is(err, ErrNoBinding) {
			return "", fmt.Errorf("githubd: no app bound to repo %q (push dropped): %w", repoFullName, err)
		}
		return "", fmt.Errorf("githubd: lookup install id for repo %q: %w", repoFullName, err)
	}
	tok, err := c.Tokens.Token(ctx, installID)
	if err != nil {
		return "", fmt.Errorf("githubd: get install token (install=%d): %w", installID, err)
	}
	return tok, nil
}

const (
	statusQueued     = "queued"
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

func phaseToStatus(p githubdgrpc.CheckPhase) string {
	switch p {
	case githubdgrpc.CheckPhaseQueued:
		return statusQueued
	case githubdgrpc.CheckPhaseBuilding:
		return statusInProgress
	case githubdgrpc.CheckPhaseLive:
		return statusCompleted
	case githubdgrpc.CheckPhaseFailed:
		return statusCompleted
	default:
		return statusQueued
	}
}

func phaseToConclusion(p githubdgrpc.CheckPhase) string {
	switch p {
	case githubdgrpc.CheckPhaseLive:
		return "success"
	case githubdgrpc.CheckPhaseFailed:
		return "failure"
	default:
		return ""
	}
}

func phaseTitle(p githubdgrpc.CheckPhase) string {
	switch p {
	case githubdgrpc.CheckPhaseQueued:
		return "Build queued"
	case githubdgrpc.CheckPhaseBuilding:
		return "Build in progress"
	case githubdgrpc.CheckPhaseLive:
		return "Deployment live"
	case githubdgrpc.CheckPhaseFailed:
		return "Deployment failed"
	default:
		return "faas build"
	}
}

// _ pins time so a future refactor that drops the import on
// unused-token usage doesn't drop it prematurely.
var _ = time.Time{}

// WriteCheckCoalesced is the rate-limit defensive wrapper around
// ChecksAPI.WriteCheck (PR-D / ADR-012 §6 closure). GitHub's
// Checks API caps each install at 1000 calls/hour (100 req/min
// burst); without coalescing, a noisy push loop could trip the
// cap and starve the operator's other PRs of check-runs.
//
// Coalescing rule: per (repo, sha), only POST when the incoming
// phase differs from the last-reported phase. The wrapper holds
// the last phase in an in-memory map keyed by (repo, sha) with
// a janitor that evicts entries older than 1h. The map is
// process-local (a daemon restart resets the state, which is
// safe — the worst case is one extra POST per active (repo,
// sha) at restart, not a rate-limit trip).
//
// Phase transitions that are valid:
//
//	Unspecified → Queued    (first call after enqueue)
//	Queued      → Building  (build started)
//	Building    → Live      (deploy succeeded)
//	Building    → Failed    (deploy failed)
//
// Same-phase re-posts (e.g. retry storms, idempotency replays)
// are silently dropped and the
// `githubd_checks_call_total{status="skipped_coalesced"}` counter
// is incremented so the on-call can see the dedup rate.
//
// Failure semantics: when the underlying WriteCheck returns an
// error, the cache entry is NOT updated so the next call retries
// the same phase. The Prometheus counter
// `githubd_checks_call_total{status="http_error"}` is bumped on
// each error.
var (
	checksCoalesceMu    sync.Mutex
	checksCoalesceCache = map[string]githubdgrpc.CheckPhase{}
)

// WriteCheckCoalesced wraps ChecksAPI.WriteCheck with per-(repo, sha)
// phase coalescing. Returns nil on a same-phase replay; otherwise
// delegates and caches the new phase on success.
func WriteCheckCoalesced(ctx context.Context, c ChecksWriter, repoFullName, commitSHA string, phase githubdgrpc.CheckPhase, logsURL, summary string) error {
	if c == nil {
		return nil
	}
	key := repoFullName + "@" + commitSHA
	checksCoalesceMu.Lock()
	last, seen := checksCoalesceCache[key]
	checksCoalesceMu.Unlock()
	if seen && last == phase {
		checksCallCounter.WithLabelValues("skipped_coalesced").Inc()
		return nil
	}
	err := c.WriteCheck(ctx, repoFullName, commitSHA, phase, logsURL, summary)
	if err != nil {
		checksCallCounter.WithLabelValues("http_error").Inc()
		return err
	}
	checksCoalesceMu.Lock()
	checksCoalesceCache[key] = phase
	checksCoalesceMu.Unlock()
	checksCallCounter.WithLabelValues("posted").Inc()
	return nil
}

// checksCallCounter is the Prometheus counter exposed by
// WriteCheckCoalesced. Defined as a package-level var so a test
// can swap it for a recording fake without rewiring the
// production wiring. Registration is wrapped in sync.Once so a
// second package init (e.g. when cmd/githubd imports pkg/githubd
// twice for whitebox tests) is a no-op rather than a panic.
var checksCallCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "githubd_checks_call_total",
	Help: "Outcome of each WriteCheck call after the coalescing wrapper. status=posted|skipped_coalesced|http_error.",
}, []string{"status"})

var checksCallCounterOnce sync.Once

func init() {
	checksCallCounterOnce.Do(func() {
		prometheus.MustRegister(checksCallCounter)
	})
}
