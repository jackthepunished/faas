// cmd/gregale/cmd_deploy_zero_config_test.go — integration coverage
// for the refactored `gregale deploy` zero-config path (issue #1182).
//
// These tests exercise the full pipeline end-to-end through
// cmdDeployTarball → resolveZeroConfigProvenance → gitArchiveHEAD
// → buildCreateRequest → createOrFetchApp → DeployTarball, against
// a stub apid. The git repo is a real `git init`d tempdir with a
// GitHub remote (so the zero-config branch fires), and the cwd is
// chdir'd into it for the duration of each test (chdir is
// restored by t.Cleanup).

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// initZeroConfigRepo creates a tempdir git repo with a GitHub
// `origin` remote and a single commit. Returns the tempdir path.
// Mirrors initTestRepo (git_local_test.go:281) but with the
// remote pre-wired so the zero-config branch recognises the
// repo as deployable without the operator running `git remote add`.
// The repo carries a package.json so resolveDeployShape picks the
// app shape (the cwd-auto-pack branch the zero-config path falls
// through to when no --function / --app override is passed).
func initZeroConfigRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test User"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	for name, body := range map[string]string{
		"README.md":    "hello\n",
		"package.json": "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"add", "package.json"},
		{"commit", "-q", "-m", "initial commit"},
		{"remote", "add", "origin", "git@github.com:acme/demo.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// withCwd chdirs into dir for the duration of the test and
// restores the original cwd on cleanup. Wraps the chdir pattern
// scattered across cli_test.go / git_local_test.go /
// cmd_deploy_annotations_test.go into a single helper so the
// new tests are uniform.
func withCwd(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
	})
}

// zeroConfigStubServer captures the route shape and returns the
// canned responses cmdDeployTarball's zero-config branch needs.
// The handler dispatcher is wired per-test so each case can
// customise its 409 vs 200 paths; the helper just owns the
// boilerplate routes (Whoami, SSE logs stream) that every test
// uses.
type zeroConfigStubServer struct {
	t        *testing.T
	srv      *httptest.Server
	routes   func(w http.ResponseWriter, r *http.Request) // test-specific routes
	gotCalls map[string]int
}

func newZeroConfigStubServer(t *testing.T, custom func(http.ResponseWriter, *http.Request, *zeroConfigStubServer)) *zeroConfigStubServer {
	z := &zeroConfigStubServer{t: t, gotCalls: map[string]int{}}
	z.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default routes common to every test.
		switch {
		case r.URL.Path == "/v1/account" && r.Method == "GET":
			z.gotCalls["whoami"]++
			_ = json.NewEncoder(w).Encode(api.AccountResponse{
				ID: "acct-1", Email: "ops@acme.test", Plan: "free",
				Status: "active",
			})
			return
		case strings.HasPrefix(r.URL.Path, "/v1/deployments/d1/logs"):
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: status\ndata: {\"status\":\"live\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
			return
		}
		// Test-specific routes (CreateApp, GetApp, DeployTarball).
		if custom != nil {
			custom(w, r, z)
			return
		}
		http.Error(w, "no", 404)
	}))
	t.Cleanup(z.srv.Close)
	return z
}

// TestDeployZeroConfig_HappyPath_NewApp pins the headline AC:
// a fresh repo with a GitHub origin, no flags, hits the normal
// buildCreateRequest → CreateApp → DeployTarball pipeline. The
// stub server records the order of /v1/apps and /v1/apps/{slug}/
// deployments so the test catches a regression to the old
// `cmdDeployZeroConfig` path that bypassed CreateApp.
func TestDeployZeroConfig_HappyPath_NewApp(t *testing.T) {
	repo := initZeroConfigRepo(t)
	withCwd(t, repo)

	stub := newZeroConfigStubServer(t, func(w http.ResponseWriter, r *http.Request, z *zeroConfigStubServer) {
		switch {
		case r.URL.Path == "/v1/apps" && r.Method == "POST":
			z.gotCalls["create"]++
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "a1", Slug: "demo"})
		case r.URL.Path == "/v1/apps/demo/deployments" && r.Method == "POST":
			z.gotCalls["deploy"]++
			// Drain the multipart body so the connection reuses cleanly.
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "d1", Status: "pending", AppID: "demo"})
		default:
			http.Error(w, "no", 404)
		}
	})
	t.Setenv("FAAS_API", stub.srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdDeployTarball([]string{"--name", "demo"}); code != 0 {
		t.Fatalf("zero-config deploy exit = %d, want 0", code)
	}
	if stub.gotCalls["create"] == 0 {
		t.Errorf("CreateApp should be called for a new slug; was the zero-config path short-circuited again?")
	}
	if stub.gotCalls["deploy"] == 0 {
		t.Errorf("DeployTarball should be called after CreateApp")
	}
	// Sanity: Whoami fired too (per-plan cap round-trip).
	if stub.gotCalls["whoami"] == 0 {
		t.Errorf("Whoami round-trip for per-plan cap should have fired")
	}
}

// TestDeployZeroConfig_409SameAccount_GETProbeAndPATCH pins the
// hybrid slug-conflict probe: CreateApp 409 + GetApp 200 →
// UpdateApp PATCH mirrors --require-authn (existing #560 contract).
// This is the in-account re-deploy path — the customer already
// owns the slug and is just shipping a new commit.
func TestDeployZeroConfig_409SameAccount_GETProbeAndPATCH(t *testing.T) {
	repo := initZeroConfigRepo(t)
	withCwd(t, repo)

	stub := newZeroConfigStubServer(t, func(w http.ResponseWriter, r *http.Request, z *zeroConfigStubServer) {
		switch {
		case r.URL.Path == "/v1/apps" && r.Method == "POST":
			z.gotCalls["create"]++
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(api.Problem{
				Status: 409, Code: api.CodeConflict, Title: "Conflict", Detail: "app exists",
			})
		case r.URL.Path == "/v1/apps/demo" && r.Method == "GET":
			z.gotCalls["getapp"]++
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "a1", Slug: "demo"})
		case r.URL.Path == "/v1/apps/demo" && r.Method == "PATCH":
			z.gotCalls["patch"]++
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "a1", Slug: "demo", RequireAuthn: true})
		case r.URL.Path == "/v1/apps/demo/deployments" && r.Method == "POST":
			z.gotCalls["deploy"]++
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "d1", Status: "pending", AppID: "demo"})
		default:
			http.Error(w, "no", 404)
		}
	})
	t.Setenv("FAAS_API", stub.srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	// --require-authn is the flag that triggers the PATCH on
	// the existing-app branch (preserve #560 UX).
	if code := cmdDeployTarball([]string{"--require-authn", "--name", "demo"}); code != 0 {
		t.Fatalf("zero-config deploy 409-same-account exit = %d, want 0", code)
	}
	if stub.gotCalls["getapp"] == 0 {
		t.Errorf("GetApp probe should fire after CreateApp 409 (hybrid slug-conflict disambiguation)")
	}
	if stub.gotCalls["patch"] == 0 {
		t.Errorf("UpdateApp PATCH should mirror --require-authn on the existing-app branch")
	}
	if stub.gotCalls["deploy"] == 0 {
		t.Errorf("DeployTarball should still run after the in-account PATCH")
	}
}

// TestDeployZeroConfig_409OtherAccount_HardFail pins the new
// safety boundary added in issue #1182: when CreateApp 409s and
// GetApp returns 404 (apid's silent IDOR 404 — the slug is owned
// by another account, or the row vanished in a race), the CLI
// hard-fails with a clear "slug already in use" message and does
// NOT upload the tarball.
func TestDeployZeroConfig_409OtherAccount_HardFail(t *testing.T) {
	repo := initZeroConfigRepo(t)
	withCwd(t, repo)

	stub := newZeroConfigStubServer(t, func(w http.ResponseWriter, r *http.Request, z *zeroConfigStubServer) {
		switch {
		case r.URL.Path == "/v1/apps" && r.Method == "POST":
			z.gotCalls["create"]++
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(api.Problem{
				Status: 409, Code: api.CodeConflict, Title: "Conflict", Detail: "slug taken",
			})
		case r.URL.Path == "/v1/apps/demo" && r.Method == "GET":
			z.gotCalls["getapp"]++
			http.Error(w, "no such app", http.StatusNotFound)
		case r.URL.Path == "/v1/apps/demo/deployments" && r.Method == "POST":
			z.gotCalls["deploy"]++
			t.Errorf("DeployTarball should NOT fire when GetApp probe 404s (other-account slug)")
		default:
			http.Error(w, "no", 404)
		}
	})
	t.Setenv("FAAS_API", stub.srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	if code := cmdDeployTarball([]string{"--name", "demo"}); code == 0 {
		t.Fatalf("zero-config deploy 409-other-account should hard-fail, got code 0")
	}
	if stub.gotCalls["getapp"] == 0 {
		t.Errorf("GetApp probe should fire after CreateApp 409 to disambiguate")
	}
	if stub.gotCalls["deploy"] != 0 {
		t.Errorf("DeployTarball fired even though slug belongs to another account")
	}
}

// TestDeployZeroConfig_JSONShape pins that the refactored path
// honours --json: stdout is a single JSON document describing the
// deployment, not the human "packing / build queued" lines.
// The legacy zero-config path always streamed logs regardless
// of --json (issue #1182 §3.2).
func TestDeployZeroConfig_JSONShape(t *testing.T) {
	repo := initZeroConfigRepo(t)
	withCwd(t, repo)

	stub := newZeroConfigStubServer(t, func(w http.ResponseWriter, r *http.Request, z *zeroConfigStubServer) {
		switch {
		case r.URL.Path == "/v1/apps" && r.Method == "POST":
			z.gotCalls["create"]++
			_ = json.NewEncoder(w).Encode(api.AppResponse{ID: "a1", Slug: "demo"})
		case r.URL.Path == "/v1/apps/demo/deployments" && r.Method == "POST":
			z.gotCalls["deploy"]++
			_, _ = io.Copy(io.Discard, r.Body)
			_ = json.NewEncoder(w).Encode(api.DeploymentResponse{ID: "d1", Status: "pending", AppID: "demo"})
		default:
			http.Error(w, "no", 404)
		}
	})
	t.Setenv("FAAS_API", stub.srv.URL)
	t.Setenv("FAAS_TOKEN", "fp_live_x")

	// Capture stdout to verify --json shape. The CLI renders --json
	// via writeJSON, which writes to the package-level osStdout
	// (commands3.go:45). Override it for the test.
	var stdout bytes.Buffer
	oldOut := osStdout
	osStdout = &stdout
	defer func() { osStdout = oldOut }()

	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	code := cmdDeployTarball([]string{"--json", "--name", "demo"})
	out := stdout.Bytes()

	if code != 0 {
		t.Fatalf("zero-config deploy --json exit = %d, want 0", code)
	}
	// Sanity: stdout must be a valid JSON document, not the human
	// "packing / build queued" lines. The deployment ID is the
	// load-bearing field — its presence confirms --json went
	// through DeployTarball's JSON render branch, not streamDeployLogs.
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("--json stdout should be a single JSON document, got parse error: %v\nstdout: %q", err, out)
	}
	if id, ok := doc["id"].(string); !ok || id != "d1" {
		t.Errorf("--json stdout missing deployment id; got: %v", doc)
	}
	// Negative assertion: no human "build queued" / "Deployed." line.
	if strings.Contains(string(out), "build queued") || strings.Contains(string(out), "Deployed.") {
		t.Errorf("--json stdout leaked human deploy log: %s", out)
	}
}