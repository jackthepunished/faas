package polar

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/billing"
)

// VerifyWebhook verifies Polar's Standard Webhooks headers and normalizes the
// supported subscription/order events. The signature covers the exact body
// bytes, so callers must not re-marshal the payload before invoking it.
func (p *Provider) VerifyWebhook(payload []byte, headers map[string]string, tolerance time.Duration) (billing.Event, error) {
	if p == nil || strings.TrimSpace(p.webhookSecret) == "" {
		return billing.Event{}, fmt.Errorf("polar: %w: webhook secret is not configured", billing.ErrBadSignature)
	}
	id := headerValue(headers, "webhook-id")
	timestamp := headerValue(headers, "webhook-timestamp")
	signature := headerValue(headers, "webhook-signature")
	if id == "" || timestamp == "" || signature == "" {
		return billing.Event{}, fmt.Errorf("polar: %w: missing Standard Webhooks header", billing.ErrBadSignature)
	}
	if err := verifyStandardWebhookSignature(payload, id, timestamp, signature, p.webhookSecret, tolerance); err != nil {
		return billing.Event{}, err
	}
	event, err := parsePolarEvent(payload, id, p)
	if err != nil {
		return billing.Event{}, err
	}
	return event, nil
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func verifyStandardWebhookSignature(payload []byte, id, timestamp, signature, secret string, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("polar: %w: malformed webhook timestamp", billing.ErrBadSignature)
	}
	age := time.Since(time.Unix(unix, 0))
	if age > tolerance || age < -tolerance {
		return fmt.Errorf("polar: %w: webhook timestamp outside tolerance", billing.ErrBadSignature)
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return fmt.Errorf("polar: %w: invalid webhook secret encoding", billing.ErrBadSignature)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	expected := mac.Sum(nil)
	for _, part := range strings.Fields(signature) {
		version, encoded, ok := strings.Cut(part, ",")
		if !ok || version != "v1" {
			continue
		}
		got, decodeErr := decodeBase64(encoded)
		if decodeErr == nil && hmac.Equal(expected, got) {
			return nil
		}
	}
	return fmt.Errorf("polar: %w: signature mismatch", billing.ErrBadSignature)
}

func decodeSecret(secret string) ([]byte, error) {
	secret = strings.TrimSpace(strings.TrimPrefix(secret, "whsec_"))
	if secret == "" {
		return nil, errors.New("empty secret")
	}
	return decodeBase64(secret)
}

func decodeBase64(value string) ([]byte, error) {
	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, decoder := range decoders {
		if decoded, err := decoder.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// SignForTest creates a Standard Webhooks v1 signature. rawSecret is the
// decoded HMAC key; VerifyWebhook expects the base64 encoding of that key.
func SignForTest(payload []byte, rawSecret, id string, when time.Time) string {
	timestamp := strconv.FormatInt(when.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(rawSecret))
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(payload)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type polarWebhook struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func parsePolarEvent(payload []byte, eventID string, p *Provider) (billing.Event, error) {
	var raw polarWebhook
	if err := json.Unmarshal(payload, &raw); err != nil {
		return billing.Event{}, fmt.Errorf("polar: parse webhook body: %w", err)
	}
	var data map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw.Data)))
	decoder.UseNumber()
	_ = decoder.Decode(&data)

	subscriptionID := stringValue(data["subscription_id"])
	if subscriptionID == "" && strings.HasPrefix(raw.Type, "subscription.") {
		subscriptionID = stringValue(data["id"])
	}
	event := billing.Event{
		EventID:        eventID,
		Type:           mapPolarEventType(raw.Type, data),
		CustomerID:     firstString(data, "customer_id", nestedString(data, "customer", "id")),
		SubscriptionID: subscriptionID,
		PlanID:         p.planForProduct(firstString(data, "product_id", nestedString(data, "product", "id"))),
		Raw:            clone(payload),
		Currency:       strings.ToUpper(firstString(data, "currency", "price_currency")),
	}
	if event.Type == billing.EventRefundProcessed {
		event.ProviderRefundID = stringValue(data["id"])
		event.ChargeID = firstString(data, "order_id", nestedString(data, "order", "id"))
		event.AmountCents = numberValue(data["amount"])
	} else if strings.HasPrefix(raw.Type, "order.") {
		event.ChargeID = stringValue(data["id"])
		event.AmountCents = firstNumber(data, "total_amount", "net_amount", "amount")
	}
	if event.SubscriptionID == event.CustomerID {
		event.SubscriptionID = stringValue(data["subscription_id"])
	}
	return event, nil
}

func mapPolarEventType(eventType string, data map[string]any) billing.EventType {
	switch eventType {
	case "subscription.created":
		return billing.EventSubscriptionCreated
	case "subscription.updated", "subscription.uncanceled":
		return billing.EventSubscriptionUpdated
	case "subscription.active":
		return billing.EventPaymentSucceeded
	case "subscription.past_due":
		return billing.EventSubscriptionPastDue
	case "subscription.revoked":
		return billing.EventSubscriptionCanceled
	case "subscription.canceled":
		// Polar sends this immediately for a scheduled cancellation while
		// the subscription is still active. Do not suspend the account until
		// the eventual revoked event.
		if boolValue(data["cancel_at_period_end"]) || stringValue(data["status"]) == "active" {
			return billing.EventSubscriptionUpdated
		}
		return billing.EventSubscriptionCanceled
	case "order.paid":
		return billing.EventPaymentSucceeded
	case "order.refunded", "refund.updated":
		return billing.EventRefundProcessed
	default:
		return billing.EventUnknown
	}
}

func (p *Provider) planForProduct(productID string) string {
	if p == nil || productID == "" {
		return ""
	}
	for plan, configured := range p.products {
		if configured == productID {
			return string(plan)
		}
	}
	return ""
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(data[key]); value != "" {
			return value
		}
	}
	return ""
}

func nestedString(data map[string]any, objectKey, valueKey string) string {
	nested, ok := data[objectKey].(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(nested[valueKey])
}

func stringValue(value any) string {
	switch value := value.(type) {
	case string:
		return value
	default:
		return ""
	}
}

func boolValue(value any) bool {
	b, _ := value.(bool)
	return b
}

func numberValue(value any) int64 {
	switch value := value.(type) {
	case json.Number:
		n, _ := value.Int64()
		return n
	case float64:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

func firstNumber(data map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if value := numberValue(data[key]); value != 0 {
			return value
		}
	}
	return 0
}

func clone(payload []byte) []byte {
	return append([]byte(nil), payload...)
}
