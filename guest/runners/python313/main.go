// python313 runner — Python 3.13 variant of the §4.9 function runner.
// Hosts the customer's Python 3.13 handler behind the envelope
// contract. Mirrors guest/runners/python312/main.go; only the
// runtime id differs — the handler filename stays /app/handler.py
// because Python handlers are already version-neutral on the wire
// (the base image in images/runner-python313.Dockerfile is the
// only version-bearing surface).
//
// Differs from guest/runners/python312/main.go only in:
//   - default --runtime flag value ("python313")
//   - the runtime-mismatch guard text + env var name
//
// §4.9 envelope shape is identical to the python312 runner; the
// payload contract is in pkg/api for any caller that wants to
// validate it.
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
	runtime := flag.String("runtime", "python313", "runtime id (informational)")
	handlerPath := flag.String("handler", "/app/handler.py", "path to customer handler")
	flag.Parse()
	if *runtime != "python313" {
		log.Printf("warning: --runtime=%s ignored; only python313 is supported by this binary", *runtime)
	}
	if _, err := os.Stat(*handlerPath); err != nil {
		log.Fatalf("python313 runner: handler not found at %s: %v", *handlerPath, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(w, r, *handlerPath)
	})

	addr := ":8080"
	log.Printf("python313 runner: listening on %s (handler=%s)", addr, *handlerPath)
	if err := http.ListenAndServe(addr, mux); err != nil { //nolint:gosec // bind-all is intentional inside the guest
		log.Fatalf("python313 runner: listen: %v", err)
	}
}

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
		log.Printf("python313 runner: handler error: %v", err)
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
			log.Printf("python313 runner: bad body_b64: %v", err)
			return
		}
		_, _ = w.Write(decoded)
	}
}

func invokeHandler(ctx context.Context, handlerPath string, env envelope) (response, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "python3", handlerPath)
	cmd.Env = append(os.Environ(), "FAAS_RUNTIME=python313")

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
