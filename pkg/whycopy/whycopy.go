// Package whycopy is the central explanation catalog for the
// error-explanations cluster (spec §6.4 amendment 1, ADR-110
// amendment 1). Each entry maps an RFC 7807 stable code to the
// customer-facing hint/why/fix prose that the CLI's 3-5 line
// renderer prints, plus an optional docs URL override and an
// optional per-code observed-value renderer.
//
// Why a central package: the prose is the load-bearing UX for
// retention; it must be reviewable in one place, table-driven
// tested, and tripwire-protected. Detection sites (commit 7-13
// of the cluster) attach hint/why/fix via WithHint/WithWhy/WithFix
// from the With chain methods — but the prose body lives here so
// the wording is consistent across every emit site.
//
// Modelled on pkg/statefuldenylist.Set: a static, table-driven
// catalog keyed by stable RFC 7807 code, with table-driven test
// coverage. The tripwire TestEveryCodeHasWhycopyEntry
// (cmd/gregale/lint_tripwires_test.go) fails the build if a new
// Code… constant is added in pkg/api/errors.go without a matching
// row here.
//
// Decorate is the single entry point; it copies the catalog row
// into the supplied *Problem. Detection sites should call Decorate
// right after the constructor so the wire Problem carries the
// full hint/why/fix/relevant_logs block on every code path.
package whycopy

import (
	"fmt"

	"github.com/onebox-faas/faas/pkg/api"
)

// Render is one row of the catalog. The fields are the customer-facing
// prose for one RFC 7807 stable code; Decorate copies them onto the
// supplied Problem. DocURL overrides Problem.DocsURL when non-empty;
// in practice the constructor's WithDocs call wins because Decorate
// runs after the constructor.
type Render struct {
	// Title overrides Problem.Title when non-empty. Most codes leave
	// this empty so the constructor's stable title wins.
	Title string
	// Hint is the single short next-action line shown on the CLI's
	// 3-5 line renderer. Mirrors SecretHint's shape — one line.
	Hint string
	// Why is the human-readable cause with the observed value. May be
	// multi-line (≤ 512 bytes; CLI tripwire enforces).
	Why string
	// Fix is the prescriptive remediation (1-3 lines).
	Fix string
	// DocsURL overrides Problem.DocsURL when non-empty.
	DocsURL string
	// Observed, when non-nil, returns the (why, fix) pair templated
	// with the observed value at detection time. Called by Decorate
	// when the caller supplies an observed value; nil = use the
	// static Why/Fix verbatim. The renderer must not mutate the
	// catalog's Render, so Observed returns strings, not *Problem.
	Observed func(observed any) (why, fix string)
}

// catalog is the static explanation table. Order does not matter;
// lookup is map-based. Adding a new code → add a row here.
// Tripwire: TestEveryCodeHasWhycopyEntry in
// cmd/gregale/lint_tripwires_test.go pins 1:1 membership.
var catalog = map[string]Render{
	api.CodeAppNotListening: {
		Title: "No process listening on $PORT",
		Hint:  "your app isn't accepting traffic on the port we expect",
		Why:   "the wake readiness probe found no listener on $PORT after the wake timeout — either nothing is bound, or your listener bound to a different port/address",
		Fix:   "• make sure your app listens on process.env.PORT (or 8080)\n• if you bind manually, bind to 0.0.0.0 not 127.0.0.1\n• run `gregale doctor` to scan your source for bind issues",
		Observed: func(observed any) (why, fix string) {
			if observed == nil {
				return "", ""
			}
			port, _ := observed.(string)
			if port == "" {
				return "", ""
			}
			return fmt.Sprintf("readiness probe dialed :%s and got no listener; your app is not bound to that port, or it bound to a different address.", port),
				"• check `app.listen(process.env.PORT)` or equivalent in your framework\n• if you bind manually, the bind address must be 0.0.0.0 (not 127.0.0.1)\n• run `gregale doctor` for a source-side preflight"
		},
	},
	api.CodeAppLoopbackBound: {
		Title: "Application bound to loopback",
		Hint:  "your app is listening on 127.0.0.1 — gateway traffic can't reach it",
		Why:   "the characterization probe found your listener bound to 127.0.0.1; the per-VM bridge forwards requests to 10.0.0.2, so loopback-only binds never receive traffic even though the readiness probe passed",
		Fix:   "• bind to 0.0.0.0 (or '::') not 127.0.0.1\n• if you use Express: `app.listen(PORT, '0.0.0.0')`\n• if you use FastAPI: `uvicorn --host 0.0.0.0`",
	},
	api.CodeAppArchMismatch: {
		Title: "Unsupported CPU architecture",
		Hint:  "your binary won't run on this control plane",
		Why:   "the build VM tried to exec your binary and the kernel returned ENOEXEC; the host runs linux/amd64 but your binary targets a different architecture (most commonly darwin/arm64 from a Mac dev box)",
		Fix:   "• if Go: `GOOS=linux GOARCH=amd64 go build`\n• if Rust: `cargo build --target x86_64-unknown-linux-gnu`\n• if a binary tarball: rebuild on linux/amd64 (or use a multi-arch base image)",
	},
	api.CodeEnvVarMissing: {
		Title: "Missing environment variable",
		Hint:  "your code references an env var we haven't been given",
		Why:   "the preflight scanner found a reference to $ENV_VAR_NAME in your source that is not declared in the app's env config; the runtime would crash on first access",
		Fix:   "• `gregale env set ENV_VAR_NAME <value>` (or use secrets for sensitive values)\n• or declare it as optional in the source if it's truly optional",
		Observed: func(observed any) (why, fix string) {
			if observed == nil {
				return "", ""
			}
			name, _ := observed.(string)
			if name == "" {
				return "", ""
			}
			return fmt.Sprintf("source references $%s but it is not declared in the app's env config.", name),
				fmt.Sprintf("• `gregale env set %s <value>`\n• or use secrets: `gregale secrets set %s` (rotates on each read)", name, name)
		},
	},
	api.CodeAppHealthzUnauthorized: {
		Title: "Health endpoint returning 401",
		Hint:  "your /healthz is gated behind auth — we treat that as 'down'",
		Why:   "the liveness probe hit /healthz and got 401 (or 403); after 3 consecutive 401s we flip the deployment to failed because we can't distinguish 'the app is up but the healthz path is gated' from 'the app is down'",
		Fix:   "• expose /healthz without auth (or at /healthz-public, configured via healthcheck_path)\n• if your framework auths every route by default, add an unauthenticated /healthz route",
	},
	api.CodeAppRuntimeOOM: {
		Title: "Container out of memory",
		Hint:  "your app exceeded the plan's RAM cap and was killed",
		Why:   "the cgroup memory controller killed the process because it exceeded memory.max (plan + 8 MB); the kernel OOM-killer fired inside the microVM",
		Fix:   "• upgrade to a plan with more RAM\n• trim in-memory state (caches, buffers, large request bodies held in memory)\n• if this is a build step, see /errors/build/limits#memory instead",
	},
	api.CodeDepInstallFailed: {
		Title: "Dependency installation failure",
		Hint:  "the build VM couldn't install your dependencies",
		Why:   "the build VM ran your dependency install step (npm install / pip install / go mod download / etc.) and it exited non-zero; the failure is in the build log",
		Fix:   "• check the build log for the failing command (`gregale logs <slug> --deployment <id>`)\n• most often: lockfile pins an incompatible version, or a private registry credential is missing",
		Observed: func(observed any) (why, fix string) {
			if observed == nil {
				return "", ""
			}
			pkg, _ := observed.(string)
			if pkg == "" {
				return "", ""
			}
			return fmt.Sprintf("%s install exited non-zero; the build log shows the failing command and the package manager's error.", pkg),
				fmt.Sprintf("• run `%s install` locally to reproduce\n• if a lockfile pins an incompatible version, update it\n• if a private registry is involved, check that credentials are in `gregale secrets`", pkg)
		},
	},
	api.CodeAppStartupTimeout: {
		Title: "Application startup timeout",
		Hint:  "your app didn't become ready in time",
		Why:   "the wake readiness probe waited the full boot timeout (35s by default) and your app's /healthz never returned 200; this is distinct from idle_timeout_s (which parks the instance, not the boot)",
		Fix:   "• if your app genuinely needs more than 35s, set `startup_timeout_s` higher (per-app config)\n• if it's a framework warm-up issue, defer work until after the /healthz listener is up\n• check `gregale logs <slug>` for the boot sequence",
	},
	api.CodeStatelessOnlyViolation: {
		Title: "Stateless-only platform",
		Hint:  "this app shape needs persistent storage we don't provide",
		Why:   "the deploy shape (or resolved base image) is a stateful one this platform does not support in year one — a Dockerfile with VOLUME, a top-level data/ or db/ directory, or a known stateful base image (postgres/redis/etc.)",
		Fix:   "• use a managed service (Neon, Upstash, S3, R2, MongoDB Atlas) and wire credentials via `gregale secrets`\n• if you need a quick start, `gregale init --template=s3-uploader` shows the pattern",
	},
}

// Decorate copies the catalog row for code onto p, returning p for
// chaining. When observed is non-nil and the catalog row has an
// Observed renderer, the Why/Fix fields are templated with the
// observed value; otherwise the static Why/Fix is used verbatim.
//
// Decorate overwrites Title/Hint/Why/Fix/DocsURL with the catalog
// row's values (when non-empty). The catalog is the single source
// of truth for customer-facing prose — a constructor's Title is
// scaffolding for the catalog row, not the other way around.
func Decorate(p *api.Problem, code string, observed any) *api.Problem {
	if p == nil {
		return nil
	}
	row, ok := catalog[code]
	if !ok {
		return p
	}
	if row.Title != "" {
		p.Title = row.Title
	}
	if row.Hint != "" {
		p.Hint = row.Hint
	}
	if observed != nil && row.Observed != nil {
		why, fix := row.Observed(observed)
		if why != "" {
			p.Why = why
		}
		if fix != "" {
			p.Fix = fix
		}
	} else {
		if row.Why != "" {
			p.Why = row.Why
		}
		if row.Fix != "" {
			p.Fix = row.Fix
		}
	}
	if row.DocsURL != "" {
		p.DocsURL = row.DocsURL
	}
	return p
}

// Lookup returns the catalog row for code (used by tests + the
// cmd/gregale lint tripwire). Returns (Render{}, false) when the
// code has no row.
func Lookup(code string) (Render, bool) {
	r, ok := catalog[code]
	return r, ok
}

// Codes returns the sorted list of codes that have a catalog row.
// Used by TestEveryCodeHasWhycopyEntry to assert 1:1 membership
// with pkg/api/errors.go Code… constants.
func Codes() []string {
	out := make([]string, 0, len(catalog))
	for code := range catalog {
		out = append(out, code)
	}
	return out
}
