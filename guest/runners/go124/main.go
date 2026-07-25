// go124 runner — hosts the customer's Go static-binary handler behind the §4.9
// envelope contract. Mirrors guest/runners/python312/main.go; the only
// material difference is the spawn: the customer's handler is a static Go
// binary (Railpack's go plan emits CGO_ENABLED=0 by default), so the runner
// execs the file directly with no interpreter argument. The runner still
// reads --handler <path> for symmetry, but in production the path is locked
// to /app/handler by imaged.handleDeployment (the entrypoint baked into
// the AppManifest). --runtime is informational.
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
)

type envelope struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Query   string            `json:"query"`
	BodyB64 string            `json:"body_b64"`
}

type response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	BodyB64 string            `json:"body_b64"`
}

func main() {
	runtime := flag.String("runtime", "go124", "runtime id (informational)")
	handlerPath := flag.String("handler", "/app/handler", "path to customer handler binary")
	flag.Parse()
	if *runtime != "go124" {
		log.Printf("warning: --runtime=%s ignored; only go124 is supported by this binary", *runtime)
	}
	if _, err := os.Stat(*handlerPath); err != nil {
		log.Fatalf("go124 runner: handler not found at %s: %v", *handlerPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(w, r, *handlerPath)
	})

	addr := ":8080"
	log.Printf("go124 runner: listening on %s (handler=%s)", addr, *handlerPath)
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec // bind-all is intentional inside the guest
		log.Fatalf("go124 runner: listen: %v", err)
	}
}

// handle runs the §4.9 envelope round-trip through the customer's
// handler. The runner is the request translator — it knows nothing
// about Go beyond "exec the file with the handler path" and "pipe
// the envelope JSON over stdin".
func handle(w http.ResponseWriter, r *http.Request, handlerPath string) {
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
		log.Printf("go124 runner: handler error: %v", err)
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
			log.Printf("go124 runner: bad body_b64: %v", err)
			return
		}
		_, _ = w.Write(decoded)
	}
}

// invokeHandler spawns the customer's static Go binary at handlerPath
// and pipes the request envelope over stdin; reads the response
// envelope from stdout.
func invokeHandler(ctx context.Context, handlerPath string, env envelope) (response, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, handlerPath)
	cmd.Env = append(os.Environ(), "FAAS_RUNTIME=go124")

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
//
// Note: the implementation preserves Go's canonical header spellings
// (not lowercased keys) and keeps only v[0] of multi-valued headers —
// the Node22 runner's doc comment is wrong on both points. The new
// runners match this implementation, not the comment.
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
