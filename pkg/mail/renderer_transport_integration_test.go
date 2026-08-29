// Renderer→transport integration tests (issue #246 acceptance
// item 5). The §14 milestone gates acceptance on "each template
// reaches the wire with the expected subject/body/headers"; this
// file proves that against a httptest stub of the Resend /
// Postmark APIs so a regression in either the template or the
// transport surfaces in unit rather than in production.
//
// Mirrors the shape of pkg/mail/transports_test.go so the same
// patterns are reusable for a future Postmark parity row.
package mail_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/mail"
)

// stubResend stands up a minimal Resend-shaped API: every POST to
// /emails captures the body + headers + the Idempotency-Key,
// then returns 200 + a fake id. Reusable across all
// renderer→transport tests.
type stubResend struct {
	t           *testing.T
	gotBody     mail.ResendRequest
	gotAuth     string
	gotIdemKey  string
	callCount   int
	respondFunc func(w http.ResponseWriter, r *http.Request)
}

func newStubResend(t *testing.T) (*stubResend, *httptest.Server) {
	t.Helper()
	stub := &stubResend{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.gotAuth = r.Header.Get("Authorization")
		stub.gotIdemKey = r.Header.Get("Idempotency-Key")
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&stub.gotBody)
		}
		stub.callCount++
		if stub.respondFunc != nil {
			stub.respondFunc(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"stub_msg_001"}`))
	}))
	t.Cleanup(srv.Close)
	return stub, srv
}

// newResender wires a ResendSender pointed at the stub.
func newResender(t *testing.T, baseURL string) mail.Sender {
	t.Helper()
	s, err := mail.NewResendSender(mail.ResendConfig{
		APIKey:  "re_test_dryrun",
		From:    "ops@example.test",
		BaseURL: baseURL,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewResendSender: %v", err)
	}
	return s
}

// TestRendererToTransport_QuotaReachesWireWithUnsubscribe pins the
// §14 milestone gate: the quota-warning template, rendered and
// sent through the Resend transport, reaches the wire with the
// correct subject, body, HTML alt, AND the List-Unsubscribe
// pair (RFC 8058). Pre-PR this test failed because ResendSender
// set the headers via req.Header.Set, which Resend's API
// silently drops — the JSON body field is the only channel.
func TestRendererToTransport_QuotaReachesWireWithUnsubscribe(t *testing.T) {
	stub, srv := newStubResend(t)
	sender := newResender(t, srv.URL)

	renders, err := mail.RenderAllTemplates("https://faas.example.test/u", time.Now())
	if err != nil {
		t.Fatalf("RenderAllTemplates: %v", err)
	}
	var quota *mail.RenderTemplate
	for i := range renders {
		if renders[i].Name == "quota_warning" {
			quota = &renders[i]
			break
		}
	}
	if quota == nil {
		t.Fatal("quota template not in renders")
	}

	// Reshape RenderTemplate → Message (the renderer is read-only,
	// the production sender expects a Message).
	msg := mail.Message{
		To:        []string{"alice@example.test"},
		Subject:   quota.Subject,
		TextBody:  quota.TextBody,
		HTMLBody:  quota.HTMLBody,
		Headers:   quota.Headers,
		MessageID: quota.MessageID,
	}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if stub.callCount != 1 {
		t.Fatalf("stub callCount = %d, want 1", stub.callCount)
	}
	if stub.gotAuth != "Bearer re_test_dryrun" {
		t.Errorf("Authorization = %q, want Bearer re_test_dryrun", stub.gotAuth)
	}
	if stub.gotIdemKey != quota.MessageID {
		t.Errorf("Idempotency-Key = %q, want %q", stub.gotIdemKey, quota.MessageID)
	}

	// Subject + body sanity. The exact strings are pinned by
	// pkg/mail/headers_test.go; here we just confirm the values
	// the renderer produced are what the transport ships.
	if stub.gotBody.Subject != quota.Subject {
		t.Errorf("Subject = %q, want %q", stub.gotBody.Subject, quota.Subject)
	}
	if stub.gotBody.Text != quota.TextBody {
		t.Errorf("Text body mismatch")
	}
	if stub.gotBody.HTML != quota.HTMLBody {
		t.Errorf("HTML body mismatch")
	}

	// The headline §14 assertion: List-Unsubscribe reaches the
	// wire via the JSON body's Headers map.
	if got := stub.gotBody.Headers["List-Unsubscribe"]; got != "<https://faas.example.test/u>" {
		t.Errorf("List-Unsubscribe = %q, want <https://faas.example.test/u>", got)
	}
	if got := stub.gotBody.Headers["List-Unsubscribe-Post"]; got != "List-Unsubscribe=One-Click" {
		t.Errorf("List-Unsubscribe-Post = %q, want List-Unsubscribe=One-Click", got)
	}
}

// TestRendererToTransport_DunningReachesWireWithoutUnsubscribe
// pins the policy table pkg/mail/headers.go documents:
// dunning / billing templates must NOT carry one-click
// unsubscribe headers, even when the operator has wired the URL.
// A regression here would let a customer one-click-unsubscribe
// from "your account was suspended" and get deleted silently.
func TestRendererToTransport_DunningReachesWireWithoutUnsubscribe(t *testing.T) {
	stub, srv := newStubResend(t)
	sender := newResender(t, srv.URL)

	renders, err := mail.RenderAllTemplates("https://faas.example.test/u", time.Now())
	if err != nil {
		t.Fatalf("RenderAllTemplates: %v", err)
	}
	for _, r := range renders {
		if r.Name == "quota_warning" {
			continue // covered by the sibling test
		}
		// Each non-marketing template must produce an empty
		// Headers map after rendering (no List-Unsubscribe
		// attached).
		if len(r.Headers) != 0 {
			t.Errorf("%s carries marketing headers: %v", r.Name, r.Headers)
		}
		msg := mail.Message{
			To:        []string{"alice@example.test"},
			Subject:   r.Subject,
			TextBody:  r.TextBody,
			HTMLBody:  r.HTMLBody,
			Headers:   r.Headers,
			MessageID: r.MessageID,
		}
		if err := sender.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send(%s): %v", r.Name, err)
		}
		// The transport round-trip must NOT have added
		// List-Unsubscribe even if msg.Headers was empty.
		if _, ok := stub.gotBody.Headers["List-Unsubscribe"]; ok {
			t.Errorf("%s wire payload unexpectedly carries List-Unsubscribe", r.Name)
		}
	}
}

// TestRendererToTransport_IdempotencyKeyStableAcrossRetries pins
// the redelivery dedupe contract: a Send retry with the same
// MessageID must carry the same Idempotency-Key so Resend's
// 24-hour dedupe window collapses the retry onto the original.
// The renderer derives MessageID from (account_id, template, day)
// so a single account's redelivery within the day is one
// upstream POST.
func TestRendererToTransport_IdempotencyKeyStableAcrossRetries(t *testing.T) {
	stub, srv := newStubResend(t)
	sender := newResender(t, srv.URL)

	renders, err := mail.RenderAllTemplates("", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	quota := renders[0]
	msg := mail.Message{
		To:        []string{"alice@example.test"},
		Subject:   quota.Subject,
		TextBody:  quota.TextBody,
		HTMLBody:  quota.HTMLBody,
		MessageID: quota.MessageID,
	}
	// Two sends with the same MessageID = one upstream message.
	for i := 0; i < 2; i++ {
		if err := sender.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send[%d]: %v", i, err)
		}
	}
	if stub.callCount != 2 {
		t.Errorf("callCount = %d, want 2 (transport must not dedupe locally)", stub.callCount)
	}
	if stub.gotIdemKey != quota.MessageID {
		t.Errorf("Idempotency-Key = %q, want %q", stub.gotIdemKey, quota.MessageID)
	}
}
