package fingerly

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultWebhookTolerance is how far a delivery's timestamp may be from now
// before it is refused as a replay.
const DefaultWebhookTolerance = 5 * time.Minute

const signaturePrefix = "sha256="

// signaturePattern matches a lowercase hex digest, exactly as Fingerly sends
// it. Anchored with \A and \z because Go's $ also matches before a newline.
var signaturePattern = regexp.MustCompile(`\Asha256=[a-f0-9]{64}\z`)

// timestampPattern matches whole unsigned seconds, and nothing else.
var timestampPattern = regexp.MustCompile(`\A[0-9]+\z`)

// WebhookEvent is the envelope every webhook delivery shares.
//
// Data holds the event-specific payload, left raw so a new event type needs no
// SDK upgrade. Unmarshal it into the shape the event documents.
type WebhookEvent struct {
	// ID identifies this delivery. Persist it to deduplicate: a webhook may
	// arrive more than once.
	ID string `json:"id"`
	// Type is the event name, such as "identification.completed".
	Type string `json:"type"`
	// Version is the envelope version.
	Version int `json:"version"`
	// OrganizationID is the organization the event belongs to.
	OrganizationID string `json:"organization_id"`
	// Environment is the deployment the event came from.
	Environment string `json:"environment"`
	// CreatedAt is when Fingerly recorded the event.
	CreatedAt time.Time `json:"created_at"`
	// Data is the event-specific payload.
	Data json.RawMessage `json:"data"`
}

// WebhookOption configures VerifyWebhook.
type WebhookOption func(*webhookConfig)

type webhookConfig struct {
	tolerance time.Duration
	now       time.Time
}

// WithWebhookTolerance sets the replay window. The default is five minutes.
func WithWebhookTolerance(tolerance time.Duration) WebhookOption {
	return func(c *webhookConfig) { c.tolerance = tolerance }
}

// WithWebhookTime fixes the current time. Override it only in tests.
func WithWebhookTime(now time.Time) WebhookOption {
	return func(c *webhookConfig) { c.now = now }
}

// VerifyWebhook reports whether a delivery came from Fingerly and is recent.
//
// Pass the exact bytes received, before any JSON decoding: re-encoding them
// changes the digest. During a signing-secret rotation Fingerly sends several
// comma-separated signatures, and any one matching is accepted.
//
//	body, _ := io.ReadAll(r.Body)
//	ok := fingerly.VerifyWebhook(secret, body,
//		r.Header.Get("X-Fingerly-Timestamp"),
//		r.Header.Get("X-Fingerly-Signature"))
//
// Verification proves authenticity and freshness. It does not deduplicate, so
// persist processed WebhookEvent.ID values as well.
//
// Malformed or missing input is a false result, never a panic.
func VerifyWebhook(secret string, payload []byte, timestamp, signature string, opts ...WebhookOption) bool {
	config := webhookConfig{tolerance: DefaultWebhookTolerance}
	for _, apply := range opts {
		apply(&config)
	}
	if secret == "" || timestamp == "" || signature == "" {
		return false
	}
	// Check freshness before spending an HMAC on an obvious replay.
	if !isFresh(timestamp, config.now, config.tolerance) {
		return false
	}

	expected := signPayload(secret, timestamp, payload)
	for _, candidate := range strings.Split(signature, ",") {
		if matchesSignature(candidate, expected) {
			return true
		}
	}
	return false
}

// isFresh reports whether the signed timestamp is within tolerance of now.
func isFresh(timestamp string, now time.Time, tolerance time.Duration) bool {
	if !timestampPattern.MatchString(timestamp) || tolerance < 0 {
		return false
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	drift := now.Sub(time.Unix(seconds, 0))
	return math.Abs(drift.Seconds()) <= tolerance.Seconds()
}

// signPayload computes the expected digest.
//
// Fingerly signs the timestamp, a literal dot, then the raw body bytes. The
// result is the hex digest as ASCII, ready for a constant-time compare.
func signPayload(secret, timestamp string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(payload)
	digest := make([]byte, hex.EncodedLen(sha256.Size))
	hex.Encode(digest, mac.Sum(nil))
	return digest
}

// matchesSignature compares one candidate signature in constant time.
func matchesSignature(candidate string, expected []byte) bool {
	value := strings.TrimSpace(candidate)
	if !signaturePattern.MatchString(value) {
		return false
	}
	return hmac.Equal([]byte(value[len(signaturePrefix):]), expected)
}
