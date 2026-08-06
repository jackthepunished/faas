// python312 runner — hosts the customer's Python handler behind the §4.9
// envelope contract. Mirrors guest/runners/node22/main.go; only the
// interpreter binary differs.
//
// Why two near-identical files: the runner is a tiny static Go binary
// (~80 LOC). Splitting them keeps each one buildable + lintable on its
// own without a runtime-detection shim, and matches the per-runtime
// image split in images/. The shared envelope shape lives in pkg/api
// for any caller that wants to validate it.
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

type envelope struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Query   string            `json:"query"`
	BodyB64 string            `json:"body_b64"`
	// WaitUntilSec + TailPipePath are the waitUntil(post-response
	// tail) primitive fields (issue #667 / ADR-078). Default 0/empty
	// = no tail — backwards-compatible with pre-#667 handlers. The
	// tail host wiring lives in PR 3; PR 2 ships the envelope shape
	// only so the JSON-tag parity test can pin the wire spelling.
	WaitUntilSec int    `json:"wait_until_sec"`
	TailPipePath string `json:"tail_pipe_path,omitempty"`
}

type response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	BodyB64 string            `json:"body_b64"`
	// TailErrors is the per-task failure list (issue #667).
	// Debug-only — surfaced via runner stderr + schedd audit rows,
	// never via the HTTP response body.
	TailErrors []string `json:"tail_errors,omitempty"`
}

func main() {
	runtime := flag.String("runtime", "python312", "runtime id (informational)")
	handlerPath := flag.String("handler", "/app/handler.py", "path to customer handler")
	flag.Parse()
	if *runtime != "python312" {
		log.Printf("warning: --runtime=%s ignored; only python312 is supported by this binary", *runtime)
	}
	if _, err := os.Stat(*handlerPath); err != nil {
		log.Fatalf("python312 runner: handler not found at %s: %v", *handlerPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Issue #470 / PR #470-FU-B: framework_ready signal fires once
	// per wake when the runner's first non-5xx response lands.
	signal := internal.NewRunnerSignal("python312", time.Now())
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
	log.Printf("python312 runner: listening on %s (handler=%s)", addr, *handlerPath)
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec // bind-all is intentional inside the guest
		log.Fatalf("python312 runner: listen: %v", err)
	}
}

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
		log.Printf("python312 runner: handler error: %v", err)
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	w.WriteHeader(resp.Status)
	if resp.BodyB64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(resp.BodyB64)
		if err != nil {
			log.Printf("python312 runner: bad body_b64: %v", err)
			return
		}
		_, _ = w.Write(decoded)
	}
	// Issue #470 / PR #470-FU-B: fire the framework-ready signal
	// after the response has been written. Status < 500 → ready.
	if resp.Status < 500 {
		signal.SignalReady(time.Since(signal.StartTime()).Milliseconds())
	}
}

func invokeHandler(ctx context.Context, handlerPath string, env envelope) (response, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "python3", handlerPath)
	cmd.Env = append(os.Environ(), "FAAS_RUNTIME=python312")

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

func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
