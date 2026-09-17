package fingerly

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const webhookTimestamp = "1789646400"

var webhookPayload = []byte(`{"id":"evt_1","type":"identification.completed"}`)

// signWith produces a signature exactly as the Fingerly webhook worker does.
func signWith(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(webhookTimestamp + "."))
	mac.Write(payload)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// at returns an instant offset from the fixture timestamp.
func at(offset time.Duration) time.Time {
	return time.Unix(1789646400, 0).Add(offset)
}

func TestVerifyWebhookAcceptsAGenuineDelivery(t *testing.T) {
	if !VerifyWebhook("secret", webhookPayload, webhookTimestamp,
		signWith("secret", webhookPayload), WithWebhookTime(at(0))) {
		t.Error("a genuine delivery was refused")
	}
}

func TestVerifyWebhookRefusesTampering(t *testing.T) {
	signature := signWith("secret", webhookPayload)
	for name, call := range map[string]bool{
		"body":      VerifyWebhook("secret", []byte(`{"id":"evt_2"}`), webhookTimestamp, signature, WithWebhookTime(at(0))),
		"secret":    VerifyWebhook("other", webhookPayload, webhookTimestamp, signature, WithWebhookTime(at(0))),
		"timestamp": VerifyWebhook("secret", webhookPayload, "1789646401", signature, WithWebhookTime(at(0))),
	} {
		if call {
			t.Errorf("tampered %s was accepted", name)
		}
	}
}

func TestVerifyWebhookAcceptsAnySignatureDuringRotation(t *testing.T) {
	header := strings.Join([]string{
		signWith("new", webhookPayload), signWith("old", webhookPayload),
	}, ",")
	for _, secret := range []string{"new", "old"} {
		if !VerifyWebhook(secret, webhookPayload, webhookTimestamp, header, WithWebhookTime(at(0))) {
			t.Errorf("secret %q was refused during rotation", secret)
		}
	}
	if VerifyWebhook("other", webhookPayload, webhookTimestamp, header, WithWebhookTime(at(0))) {
		t.Error("an unrelated secret was accepted during rotation")
	}
}

func TestVerifyWebhookEnforcesTheReplayWindow(t *testing.T) {
	signature := signWith("secret", webhookPayload)
	for _, offset := range []time.Duration{0, 299 * time.Second, -299 * time.Second} {
		if !VerifyWebhook("secret", webhookPayload, webhookTimestamp, signature, WithWebhookTime(at(offset))) {
			t.Errorf("offset %v was refused", offset)
		}
	}
	for _, offset := range []time.Duration{301 * time.Second, -301 * time.Second} {
		if VerifyWebhook("secret", webhookPayload, webhookTimestamp, signature, WithWebhookTime(at(offset))) {
			t.Errorf("offset %v was accepted", offset)
		}
	}
}

func TestVerifyWebhookHonoursACustomTolerance(t *testing.T) {
	signature := signWith("secret", webhookPayload)
	if !VerifyWebhook("secret", webhookPayload, webhookTimestamp, signature,
		WithWebhookTolerance(10*time.Minute), WithWebhookTime(at(400*time.Second))) {
		t.Error("a widened tolerance was not applied")
	}
	if VerifyWebhook("secret", webhookPayload, webhookTimestamp, signature,
		WithWebhookTolerance(-time.Second), WithWebhookTime(at(0))) {
		t.Error("a negative tolerance was accepted")
	}
}

func TestVerifyWebhookRefusesMalformedInput(t *testing.T) {
	signature := signWith("secret", webhookPayload)
	for name, timestamp := range map[string]string{
		"empty": "", "blank": " ", "fractional": "1789646400.5", "negative": "-1",
		"hex": "0x10", "word": "Infinity", "newline": "1789646400\n",
	} {
		if VerifyWebhook("secret", webhookPayload, timestamp, signature, WithWebhookTime(at(0))) {
			t.Errorf("timestamp %s was accepted", name)
		}
	}
	for name, candidate := range map[string]string{
		"empty": "", "prefix only": signaturePrefix, "short": signaturePrefix + strings.Repeat("0", 63),
		"uppercase": signaturePrefix + strings.Repeat("F", 64), "wrong algorithm": "md5=" + strings.Repeat("0", 64),
		"bare digest": strings.Repeat("0", 64),
	} {
		if VerifyWebhook("secret", webhookPayload, webhookTimestamp, candidate, WithWebhookTime(at(0))) {
			t.Errorf("signature %s was accepted", name)
		}
	}
	if VerifyWebhook("", webhookPayload, webhookTimestamp, signature, WithWebhookTime(at(0))) {
		t.Error("an empty secret was accepted")
	}
	if !VerifyWebhook("secret", nil, webhookTimestamp,
		signWith("secret", nil), WithWebhookTime(at(0))) {
		t.Error("an empty body is still signable and must verify")
	}
}

func TestWebhookEventDecodesTheEnvelope(t *testing.T) {
	body := []byte(`{
		"id": "01a0a851-0c4e-7f23-b8d1-6e2f94c0a7b5",
		"type": "identification.completed",
		"version": 1,
		"organization_id": "01a0a7f2-3c18-7b40-8d2e-5f6a9b1c0d37",
		"environment": "live",
		"created_at": "2026-09-16T09:41:12Z",
		"data": {"request_id": "01a0a84b-e6a2-7c09-9f51-0b3d7a26c8e4", "level": "high"}
	}`)

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if event.ID != "01a0a851-0c4e-7f23-b8d1-6e2f94c0a7b5" || event.Type != "identification.completed" {
		t.Errorf("id %q type %q", event.ID, event.Type)
	}
	if event.Version != 1 || event.Environment != "live" {
		t.Errorf("version %d environment %q", event.Version, event.Environment)
	}
	if !event.CreatedAt.Equal(time.Date(2026, 9, 16, 9, 41, 12, 0, time.UTC)) {
		t.Errorf("created at %v", event.CreatedAt)
	}

	// Data stays raw, so a new event type needs no SDK upgrade.
	var data struct {
		RequestID string `json:"request_id"`
		Level     string `json:"level"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("Unmarshal data: %v", err)
	}
	if data.RequestID == "" || data.Level != "high" {
		t.Errorf("data is %+v", data)
	}
}
