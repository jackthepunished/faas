// node22 runner — hosts the customer's Node handler behind the §4.9
// envelope contract. Reads --handler <path.js>, --runtime <node22>,
// serves :8080. /healthz returns 200 unconditionally (the runner itself
// is up; the handler is the customer's responsibility).
//
// §4.9 envelope (request):
//
//	{ "method":"POST", "path":"/foo", "headers":{...},
//	  "query":"a=1&b=2", "body_b64":"SGVsbG8=" }
//
// §4.9 envelope (response):
//
//	{ "status":200, "headers":{...}, "body_b64":"..." }
//
// The runner spawns the handler via `node <handler>` and writes the
// request envelope to its stdin. The handler writes the response
// envelope to stdout. One process per request — keeps the runner simple
// and the customer's handler stateless (the platform handles wake/park).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/onebox-faas/faas/guest/runners/internal"
)

// envelope matches the §4.9 request contract verbatim, extended with
// the waitUntil(post-response tail) primitive fields (issue #667 /
// ADR-078). Default 0/empty = no tail (backwards-compatible —
// pre-#667 handlers ignore the new fields, post-#667 handlers read
// them to drive ctx.waitUntil(promise)). The runner's tail host is
// the WaitGroup + per-task context.WithTimeout wired in PR 3 once
// guest-init lights up the 0x04 DGRAM path; PR 2 ships the envelope
// shape change only.
type envelope struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Query   string            `json:"query"`
	BodyB64 string            `json:"body_b64"`
	// WaitUntilSec is the per-task wall-clock ceiling for any
	// waitUntil(promise) the handler registers via the tail pipe
	// (issue #667 §"Rules"). 0 = feature disabled on this request;
	// the runner's tail host short-circuits to "no tail" and the
	// handler's waitUntil calls are silently dropped (matching
	// Vercel Edge / Cloudflare's pre-tail behaviour). Per-plan
	// value stamped by imaged at build time via BuildEnvWithSecrets
	// (PR 3).
	WaitUntilSec int `json:"wait_until_sec"`
	// TailPipePath is the JSONL pipe the handler appends one line
	// per waitUntil(promise) registration to. Empty string = no
	// tail (the handler's __faas_tail.js shim no-ops the waitUntil
	// global, so legacy customer code keeps working). Per-request
	// path under /tmp/faas-tail-<random>.jsonl (PR 3).
	TailPipePath string `json:"tail_pipe_path,omitempty"`
}

// response is the §4.9 response contract, extended with optional
// tail failure metadata (issue #667 / ADR-078). TailErrors is
// debug-only — surfaced to the customer via the runner's stderr +
// the schedd's wake.tail_failed audit row, but never to the
// HTTP response body (the issue forbids response rewrites). Empty
// slice = no tail or all tasks completed cleanly.
type response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	BodyB64 string            `json:"body_b64"`
	// TailErrors is the per-task failure list (timeouts,
	// handler-thrown errors, post-MaxBytes). The shape is a
	// JSON array of strings; omitted from the wire when empty
	// to keep the response envelope identical to pre-#667 for
	// handlers that do not use waitUntil.
	TailErrors []string `json:"tail_errors,omitempty"`
}

func main() {
	runtime := flag.String("runtime", "node22", "runtime id (informational)")
	handlerPath := flag.String("handler", "/app/node22.js", "path to customer handler")
	flag.Parse()
	if *runtime != "node22" {
		log.Printf("warning: --runtime=%s ignored; only node22 is supported by this binary", *runtime)
	}
	if _, err := os.Stat(*handlerPath); err != nil {
		log.Fatalf("node22 runner: handler not found at %s: %v", *handlerPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Issue #470 / PR #470-FU-B: the framework-ready signal fires
	// once per wake when the runner's first non-5xx response lands.
	// The runner's start time is captured here (runner boot ≈
	// guest-init boot; the host sees the wake as the wire start)
	// and the signal is fired from inside the request handler
	// after the handler's response envelope is parsed.
	signal := internal.NewRunnerSignal("node22", time.Now())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(w, r, *handlerPath, signal)
	})

	// Issue #460 / ADR-053 (PR-C): PORT env var carries the
	// per-deployment override port guest-init stamped onto the
	// exec'd env (see guest/init/main_linux.go::runAppWithEnv).
	// Falls back to 8080 for unit tests + non-PR-C paths.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("node22 runner: listening on %s (handler=%s)", addr, *handlerPath)
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec // bind-all is intentional inside the guest
		log.Fatalf("node22 runner: listen: %v", err)
	}
}

// handle runs the §4.9 envelope round-trip through the customer's
// handler. The runner is the request translator — it knows nothing
// about Node beyond "spawn the binary with the handler path" and "pipe
// the envelope JSON over stdin".
//
// Issue #470 / PR #470-FU-B: after the handler's response envelope
// is parsed, fires the framework-ready signal if the response
// status is non-5xx (the engine's captureWarmSnapshot waits on
// the first non-5xx response — 5xx is "framework still warming
// up", not "ready to capture"). The RunnerSignal's sync.Once
// collapses parallel calls into one.
func handle(w http.ResponseWriter, r *http.Request, handlerPath string, signal *internal.RunnerSignal) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	env := envelope{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: headerMap(r.Header),
		Query:   r.URL.RawQuery,
		BodyB64: base64.StdEncoding.EncodeToString(body),
	}

	resp, err := invokeHandler(r.Context(), handlerPath, env)
	if err != nil {
		log.Printf("node22 runner: handler error: %v", err)
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	w.WriteHeader(resp.Status)
	if resp.BodyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(resp.BodyB64)
		if err != nil {
			log.Printf("node22 runner: bad body_b64: %v", err)
			return
		}
		_, _ = w.Write(decoded)
	}
	// Fire the framework-ready signal after the response has been
	// written. Status < 500 → ready. 5xx is "framework still warming
	// up" — the engine's wait on the SQL column will continue.
	if resp.Status < 500 {
		signal.SignalReady(time.Since(signal.StartTime()).Milliseconds())
	}
}

// invokeHandler spawns `node <handlerPath>` and pipes the request
// envelope over stdin; reads the response envelope from stdout.
func invokeHandler(ctx context.Context, handlerPath string, env envelope) (response, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "node", handlerPath)
	cmd.Env = append(os.Environ(), "FAAS_RUNTIME=node22")

	var stdin bytes.Buffer
	if err := json.NewEncoder(&stdin).Encode(env); err != nil {
		return response{}, fmt.Errorf("encode envelope: %w", err)
	}
	cmd.Stdin = &stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return response{}, fmt.Errorf("handler exec: %w (stderr=%s)", err, stderr.String())
	}
	var resp response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		return response{}, fmt.Errorf("decode response: %w (stdout=%s)", err, stdout.String())
	}
	return resp, nil
}

// headerMap folds http.Header into the lowercase-string-keyed map the
// §4.9 envelope expects. Multi-value headers are joined with ", ".
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
