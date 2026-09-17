package fingerly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The vendored snapshot makes these tests independent of sibling repositories.
// Refresh it with: go run tools/synccontract.go
const contractDir = "contract"

type contractFixtures struct {
	Responses []struct {
		Name   string          `json:"name"`
		Schema string          `json:"schema"`
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"responses"`
	Webhooks []struct {
		Name      string `json:"name"`
		Secret    string `json:"secret"`
		Payload   string `json:"payload"`
		Timestamp string `json:"timestamp"`
		Signature string `json:"signature"`
		Valid     bool   `json:"valid"`
	} `json:"webhooks"`
}

func loadFixtures(t *testing.T) contractFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(contractDir, "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixtures contractFixtures
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fixtures
}

// respondRaw answers with a fixture body exactly as the server recorded it.
func respondRaw(status int, body json.RawMessage) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

func TestContractSnapshotMatchesItsManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("the manifest records no files")
	}
	for name, want := range manifest.Files {
		content, err := os.ReadFile(filepath.Join(contractDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got := hex.EncodeToString(sha256Sum(content)); got != want {
			t.Errorf("%s is stale: hash %s, manifest records %s", name, got, want)
		}
	}
}

func TestReadsEveryEventFixture(t *testing.T) {
	for _, fixture := range loadFixtures(t).Responses {
		if fixture.Schema != "SDKEventOut" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			rec := newRecorder(t, respondRaw(fixture.Status, fixture.Body))
			event, err := testClient(rec).Events.Get(context.Background(), "id")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			var want map[string]any
			if err := json.Unmarshal(fixture.Body, &want); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if event.RequestID != want["request_id"] {
				t.Errorf("request id is %q", event.RequestID)
			}
			if event.VisitorID != want["visitor_id"] {
				t.Errorf("visitor id is %q", event.VisitorID)
			}
			// A fixture without a score must not decode as zero.
			score, scored := event.Score()
			if raw, present := want["suspect_score"]; !present || raw == nil {
				if scored {
					t.Errorf("an unscored fixture decoded as %d", score)
				}
			} else if !scored || float64(score) != raw.(float64) {
				t.Errorf("score is %d, want %v", score, raw)
			}
			if event.OccurredAt.IsZero() {
				t.Error("occurred_at did not decode")
			}
		})
	}
}

func TestReadsEveryPageFixture(t *testing.T) {
	for _, fixture := range loadFixtures(t).Responses {
		if fixture.Schema != "SDKEventsOut" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			rec := newRecorder(t, respondRaw(fixture.Status, fixture.Body))
			page, err := testClient(rec).Events.List(context.Background(), nil)
			if err != nil {
				t.Fatalf("List: %v", err)
			}

			var want struct {
				Rows     []map[string]any `json:"rows"`
				Page     int              `json:"page"`
				PageSize int              `json:"page_size"`
			}
			if err := json.Unmarshal(fixture.Body, &want); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if page.Page != want.Page || page.PageSize != want.PageSize {
				t.Errorf("page %d size %d, want %d and %d",
					page.Page, page.PageSize, want.Page, want.PageSize)
			}
			if len(page.Rows) != len(want.Rows) {
				t.Fatalf("decoded %d rows, want %d", len(page.Rows), len(want.Rows))
			}
			for i, row := range page.Rows {
				if row.RequestID != want.Rows[i]["request_id"] {
					t.Errorf("row %d request id is %q", i, row.RequestID)
				}
			}
		})
	}
}

func TestMapsEveryErrorFixture(t *testing.T) {
	for _, fixture := range loadFixtures(t).Responses {
		if fixture.Schema != "ErrorResponse" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			rec := newRecorder(t, respondRaw(fixture.Status, fixture.Body))
			_, err := testClient(rec).Events.Get(context.Background(), "id")

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is %T, want *APIError", err)
			}
			var want struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
				RequestID string `json:"request_id"`
			}
			if err := json.Unmarshal(fixture.Body, &want); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			if apiErr.Status != fixture.Status || apiErr.Code != want.Error.Code {
				t.Errorf("status %d code %q, want %d and %q",
					apiErr.Status, apiErr.Code, fixture.Status, want.Error.Code)
			}
			if apiErr.RequestID != want.RequestID {
				t.Errorf("request id is %q, want %q", apiErr.RequestID, want.RequestID)
			}
		})
	}
}

func TestVerifiesEveryWebhookFixture(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, fixture := range fixtures.Webhooks {
		t.Run(fixture.Name, func(t *testing.T) {
			seconds := parseUnix(t, fixture.Timestamp)
			got := VerifyWebhook(
				fixture.Secret, []byte(fixture.Payload), fixture.Timestamp, fixture.Signature,
				WithWebhookTime(time.Unix(seconds, 0)),
			)
			if got != fixture.Valid {
				t.Errorf("verified %v, want %v", got, fixture.Valid)
			}
		})
	}

	// The same fixture, past the replay window, must be refused.
	stale := fixtures.Webhooks[0]
	seconds := parseUnix(t, stale.Timestamp)
	if VerifyWebhook(stale.Secret, []byte(stale.Payload), stale.Timestamp, stale.Signature,
		WithWebhookTime(time.Unix(seconds+301, 0))) {
		t.Error("a stale delivery was accepted")
	}
}

// parseUnix reads a fixture's signing timestamp, so each one is verified at
// the instant it was signed.
func parseUnix(t *testing.T, timestamp string) int64 {
	t.Helper()
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", timestamp, err)
	}
	return seconds
}

func sha256Sum(content []byte) []byte {
	sum := sha256.Sum256(content)
	return sum[:]
}
