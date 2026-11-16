package fingerly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxErrorBody caps how much of a failed response is parsed for metadata. An
// error body is small; anything larger is not one worth reading.
const maxErrorBody = 1 << 20

// maxResponseBody caps a successful answer. An event is a few kilobytes and a
// page of them well under a megabyte.
const maxResponseBody = 4 << 20

// userAgent identifies the SDK build that made the request.
var userAgent = "fingerly-go/" + Version

// errorEnvelope is the API's error body. Every field is treated as untrusted.
type errorEnvelope struct {
	Error struct {
		Code       string          `json:"code"`
		Details    json.RawMessage `json:"details"`
		ConsumedAt json.RawMessage `json:"consumed_at"`
	} `json:"error"`
	// Code and ConsumedAt at the top level are read only for an
	// event_already_consumed answer, whose body may carry them there. They
	// stay raw so an unexpected shape never spoils the rest of the envelope.
	Code       json.RawMessage `json:"code"`
	ConsumedAt json.RawMessage `json:"consumed_at"`
	RequestID  string          `json:"request_id"`
}

// rawString decodes a raw JSON value as a string, or "" when it is not one.
func rawString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// get performs one authenticated GET and returns the raw response body.
//
// It never retries. The deadline covers the request and reading the body,
// because a Go request context governs the whole exchange.
func (c *Client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.send(ctx, http.MethodGet, path, nil)
}

// send performs one authenticated request with an optional JSON body and
// returns the raw response body. It never retries.
func (c *Client) send(ctx context.Context, method, path string, payload []byte) (json.RawMessage, error) {
	if c.configErr != nil {
		return nil, c.configErr
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var requestBody io.Reader
	if payload != nil {
		requestBody = bytes.NewReader(payload)
	} else if method != http.MethodGet {
		requestBody = http.NoBody
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return nil, fmt.Errorf("fingerly: build request: %w", err)
	}
	request.Header.Set("X-API-Key", c.secretKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, c.connectionError(err)
	}
	defer func() {
		// Drain before closing so the connection returns to the pool.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))
		_ = response.Body.Close()
	}()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, c.apiError(response)
	}
	if response.ContentLength > maxResponseBody {
		return nil, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil {
		return nil, c.connectionError(err)
	}
	if len(body) > maxResponseBody {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

// connectionError wraps a transport failure in the SDK's own type, with the
// key redacted from its message.
func (c *Client) connectionError(err error) error {
	timeout := errors.Is(err, context.DeadlineExceeded)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		timeout = true
	}
	return &ConnectionError{Timeout: timeout, cause: c.redactError(err)}
}

// apiError maps a failed response to a redacted *APIError.
//
// Only the status and the structured fields the API documents are surfaced.
// The raw body is deliberately dropped rather than echoed.
func (c *Client) apiError(response *http.Response) *APIError {
	apiErr := &APIError{
		Status:     response.StatusCode,
		RequestID:  c.redact(response.Header.Get("X-Request-Id")),
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if err != nil {
		return apiErr
	}
	var envelope errorEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		// A non-JSON body still reports its HTTP status.
		return apiErr
	}
	apiErr.Code = c.redact(envelope.Error.Code)
	if apiErr.Code == "" && rawString(envelope.Code) == codeEventAlreadyConsumed {
		apiErr.Code = codeEventAlreadyConsumed
	}
	if envelope.RequestID != "" {
		apiErr.RequestID = c.redact(envelope.RequestID)
	}
	if len(envelope.Error.Details) > 0 {
		// Re-decoding the redacted text covers nested values without walking
		// the tree.
		var details any
		if json.Unmarshal([]byte(c.redact(string(envelope.Error.Details))), &details) == nil {
			apiErr.Details = details
		}
		if apiErr.Code == codeAppleRejected {
			apiErr.AppleStatus = appleStatus(envelope.Error.Details)
		}
	}
	if apiErr.Code == codeEventAlreadyConsumed {
		apiErr.consumedAt = c.redact(consumedAt(envelope))
	}
	return apiErr
}

// consumedAt reads when an event was first consumed from an
// event_already_consumed answer: a top-level consumed_at, error.consumed_at,
// or an error.details entry named consumed_at, whose issue is the time. It is
// "" when none is present.
func consumedAt(envelope errorEnvelope) string {
	if at := rawString(envelope.ConsumedAt); at != "" {
		return at
	}
	if at := rawString(envelope.Error.ConsumedAt); at != "" {
		return at
	}
	var details []struct {
		Field string `json:"field"`
		Issue string `json:"issue"`
	}
	if json.Unmarshal(envelope.Error.Details, &details) != nil {
		return ""
	}
	for _, detail := range details {
		if detail.Field == "consumed_at" {
			return strings.TrimSpace(detail.Issue)
		}
	}
	return ""
}

// appleStatus reads Apple's HTTP status from an apple_rejected answer's
// details entry named apple_status, or 0 when there is none.
func appleStatus(raw json.RawMessage) int {
	var details []struct {
		Field string `json:"field"`
		Issue string `json:"issue"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return 0
	}
	for _, detail := range details {
		if detail.Field != "apple_status" {
			continue
		}
		if status, err := strconv.Atoi(strings.TrimSpace(detail.Issue)); err == nil && status > 0 && status < 1000 {
			return status
		}
	}
	return 0
}

// redact strips the secret key from anything an error may carry to logs.
func (c *Client) redact(value string) string {
	if c.secretKey == "" {
		return value
	}
	return strings.ReplaceAll(value, c.secretKey, "[redacted]")
}

// redactError strips the secret key from a transport error, whose message can
// include the request URL.
func (c *Client) redactError(err error) error {
	if err == nil || c.secretKey == "" || !strings.Contains(err.Error(), c.secretKey) {
		return err
	}
	return errors.New(c.redact(err.Error()))
}
