package fingerly_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	fingerly "github.com/fingerly-io/fingerly-go"
)

// Build one client and reuse it: it holds a pooled connection and is safe for
// concurrent use.
func ExampleNew() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

	event, err := client.Events.Get(context.Background(), "01a0a84b-e6a2-7c09-9f51-0b3d7a26c8e4")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(event.VisitorID, event.SuspectLevel)
}

// A zero field is not sent, so the server applies its own default.
func ExampleEventsService_List() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

	page, err := client.Events.List(context.Background(), &fingerly.EventListParams{
		Level: "high",
		Limit: 50,
		From:  time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, event := range page.Rows {
		fmt.Println(event.RequestID, event.IPAddress)
	}
}

// Decide on the server's verdict, never on a score the browser supplied.
func ExampleAPIError() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

	decide := func(ctx context.Context, orderID, requestID string) (string, error) {
		event, err := client.Events.Get(ctx, requestID)
		var apiErr *fingerly.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
			return "refuse", nil
		case err != nil:
			return "", err
		}

		if event.Tag != "checkout:"+orderID || time.Since(event.OccurredAt) > 2*time.Minute {
			return "refuse", nil
		}
		// An unscored event is not a clean one; treat it as unknown.
		if !event.Scored() {
			return "challenge", nil
		}
		switch event.SuspectLevel {
		case "high":
			return "review", nil
		case "medium":
			return "challenge", nil
		}
		return "allow", nil
	}

	verdict, err := decide(context.Background(), "8412", "01a0a84b-e6a2-7c09-9f51-0b3d7a26c8e4")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(verdict)
}

// Verify the raw bytes before decoding them: re-encoding changes the digest.
func ExampleVerifyWebhook() {
	http.HandleFunc("/webhooks/fingerly", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if !fingerly.VerifyWebhook(
			os.Getenv("FINGERLY_WEBHOOK_SECRET"),
			body,
			r.Header.Get("X-Fingerly-Timestamp"),
			r.Header.Get("X-Fingerly-Signature"),
		) {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}

		var event fingerly.WebhookEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "unreadable event", http.StatusBadRequest)
			return
		}
		// Deduplicate on the event ID: a delivery may arrive more than once.
		enqueue(event.ID, body)
		w.WriteHeader(http.StatusNoContent)
	})
}

// The proxy is an http.Handler, so every router takes it directly.
func ExampleNewProxy() {
	proxy, err := fingerly.NewProxy(fingerly.ProxyOptions{
		ProxyKey: os.Getenv("FINGERLY_PROXY_KEY"),
		Prefix:   "/metrics",
		ClientIP: func(r *http.Request) string {
			// Read this from infrastructure you trust, never from the
			// left-most X-Forwarded-For value: the visitor writes that.
			return r.Header.Get("CF-Connecting-IP")
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics/", proxy)
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func enqueue(id string, body []byte) {}
