package main

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/pkg/gateway"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestSynthAdapterForwardInvocationStampsPlatformHeaders(t *testing.T) {
	a := &synthAdapter{
		forward: func(target gateway.Target) http.Handler {
			if target.InstanceID != "instance-1" || target.NodeID != "node-1" {
				t.Fatalf("target = %#v, want instance/node target", target)
			}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				for key, want := range map[string]string{
					"x-faas-invocation-id":     "inv-1",
					"x-faas-app-id":            "app-1",
					"x-faas-invocation-source": "async_invoke",
					"x-faas-instance":          "instance-1",
					"x-faas-node":              "node-1",
				} {
					if got := r.Header.Get(key); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
				if string(body) != `{"hello":"world"}` {
					t.Errorf("body = %q, want request payload", body)
				}
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
		},
	}
	out, err := a.forwardInvocation(context.Background(), gateway.Target{
		InstanceID: "instance-1",
		NodeID:     "node-1",
	}, state.Invocation{
		ID:      "inv-1",
		AppID:   "app-1",
		Source:  state.InvocationAsyncInvoke,
		Method:  http.MethodPost,
		Path:    "/e2e",
		Payload: []byte(`{"hello":"world"}`),
	})
	if err != nil {
		t.Fatalf("forwardInvocation: %v", err)
	}
	if string(out.Result) != `{"ok":true}` {
		t.Fatalf("result = %s, want response body", out.Result)
	}
	if out.State != state.InvocationDispatching {
		t.Fatalf("state = %q, want dispatching", out.State)
	}
}
