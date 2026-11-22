package fingerly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DeviceCheckService spends the Apple DeviceCheck tokens that Fingerly holds
// for an iOS event, for one hour after the identification, to read and to
// write the device's two bits. Reach it as client.DeviceCheck.
//
// Apple treats a token as single-use, so an event holds a read token and a
// write token, and each pays for one call: Query spends the read token, Write
// the write token. To read and then mark a device, read and write with the
// same request id.
type DeviceCheckService struct {
	client *Client
}

// DeviceCheckBits is what Apple holds for the device behind an event.
type DeviceCheckBits struct {
	// Found is false when Apple holds no bits for the device yet; both bits
	// are then false.
	Found bool `json:"found"`
	Bit0  bool `json:"bit0"`
	Bit1  bool `json:"bit1"`
	// LastUpdate is when the bits last changed, as YYYY-MM, or empty.
	LastUpdate string `json:"last_update,omitempty"`
}

// DeviceCheckWrite names the bits to set. A nil bit keeps its value; at
// least one must be set. fingerly.Bool makes the pointer.
type DeviceCheckWrite struct {
	Bit0 *bool `json:"bit0,omitempty"`
	Bit1 *bool `json:"bit1,omitempty"`
}

// DeviceCheckWriteResult repeats the bits Apple accepted; a bit left out of
// the write is nil.
type DeviceCheckWriteResult struct {
	Bit0 *bool `json:"bit0,omitempty"`
	Bit1 *bool `json:"bit1,omitempty"`
}

// Bool returns a pointer to v, for DeviceCheckWrite.
func Bool(v bool) *bool { return &v }

// deviceCheckRetryDelays are the waits before the second and third attempt
// of a DeviceCheck call. A test shortens them.
var deviceCheckRetryDelays = []time.Duration{250 * time.Millisecond, time.Second}

// maxDeviceCheckRetryAfter caps how long a Retry-After header can hold a
// retry.
const maxDeviceCheckRetryAfter = 5 * time.Second

// Query spends the event's held read token to read the device's two bits.
//
// Errors match ErrDeviceCheckNotHeld, ErrDeviceCheckSpent,
// ErrDeviceCheckBusy, ErrCredentialUnavailable, ErrAppleRejected or
// ErrDeviceCheckUnavailable with errors.Is. A 503 or a call that got no
// complete answer is retried, with the identical request, up to twice.
func (s *DeviceCheckService) Query(ctx context.Context, requestID string) (*DeviceCheckBits, error) {
	if !validRequestID(requestID) {
		return nil, ErrInvalidRequestID
	}
	body, err := s.client.spendDeviceCheck(ctx, http.MethodPost, deviceCheckPath(requestID)+"/query", nil)
	if err != nil {
		return nil, err
	}
	var bits DeviceCheckBits
	if err := json.Unmarshal(body, &bits); err != nil {
		return nil, fmt.Errorf("fingerly: decode device check bits: %w", err)
	}
	return &bits, nil
}

// Write spends the event's held write token to set bit0, bit1 or both. A
// write naming neither bit returns ErrBitsRequired without sending anything.
//
// Errors are the same as for Query, plus ErrBitsRequired. A 503 or a call
// that got no complete answer is retried, with the identical request, up to
// twice.
func (s *DeviceCheckService) Write(ctx context.Context, requestID string, bits DeviceCheckWrite) (*DeviceCheckWriteResult, error) {
	if !validRequestID(requestID) {
		return nil, ErrInvalidRequestID
	}
	if bits.Bit0 == nil && bits.Bit1 == nil {
		return nil, ErrBitsRequired
	}
	payload, err := json.Marshal(bits)
	if err != nil {
		return nil, fmt.Errorf("fingerly: encode device check bits: %w", err)
	}
	body, err := s.client.spendDeviceCheck(ctx, http.MethodPut, deviceCheckPath(requestID), payload)
	if err != nil {
		return nil, err
	}
	var result DeviceCheckWriteResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("fingerly: decode device check bits: %w", err)
	}
	return &result, nil
}

func deviceCheckPath(requestID string) string {
	return "/events/" + url.PathEscape(requestID) + "/device-check"
}

// spendDeviceCheck sends one DeviceCheck call and retries it, identically,
// on a 503 or a transport failure. The server keeps the token only for a
// retry of the request it was first used for, so the payload never changes
// between attempts. Nothing else is retried.
func (c *Client) spendDeviceCheck(ctx context.Context, method, path string, payload []byte) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		body, err := c.send(ctx, method, path, payload)
		if err == nil || attempt >= len(deviceCheckRetryDelays) || ctx.Err() != nil {
			return body, err
		}
		wait := deviceCheckRetryDelays[attempt]
		var apiErr *APIError
		var connErr *ConnectionError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable:
			if apiErr.RetryAfter != nil {
				if after := time.Duration(*apiErr.RetryAfter) * time.Second; after > wait {
					wait = min(after, maxDeviceCheckRetryAfter)
				}
			}
		case errors.As(err, &connErr):
		default:
			return body, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}
