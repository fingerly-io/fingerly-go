package fingerly_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
		Level: fingerly.SuspectLevelHigh,
		Limit: 50,
		From:  time.Now().Add(-24 * time.Hour),
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, event := range page.Rows {
		fmt.Println(event.RequestID, event.IPNetwork)
	}
}

// Decide on the server's verdict, never on a score the browser supplied.
func ExampleAPIError() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

	decide := func(ctx context.Context, orderID, requestID string) (string, error) {
		event, err := client.Events.Get(ctx, requestID)
		var apiErr *fingerly.APIError
		switch {
		case errors.Is(err, fingerly.ErrEventPending):
			// Not decided yet, not fraud: retry after apiErr.RetryAfter
			// seconds or verify asynchronously.
			return "pending", nil
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
		case fingerly.SuspectLevelHigh:
			return "review", nil
		case fingerly.SuspectLevelMedium:
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

// A once-per-device free trial on iOS spends two identifications, because
// Apple treats each DeviceCheck token as single-use: read the bits on the
// identification that asks for the trial, record the grant in your own data,
// then write the bit on the device's next identification.
func ExampleDeviceCheckService_Query() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))
	ctx := context.Background()

	// 1. The identification that asks for the trial.
	bits, err := client.DeviceCheck.Query(ctx, "01a0a84b-e6a2-7c09-9f51-0b3d7a26c8e4")
	switch {
	case errors.Is(err, fingerly.ErrDeviceCheckNotHeld), errors.Is(err, fingerly.ErrDeviceCheckSpent):
		// No answer, not a verdict: decide on your own records alone.
	case err != nil:
		log.Fatal(err)
	case bits.Found && bits.Bit0:
		fmt.Println("this device already had a trial")
		return
	}
	// 2. Grant the trial and record it in your own data, with the device
	// not yet marked.

	// 3. The next identification from that install marks the device.
	if _, err := client.DeviceCheck.Write(ctx, "01a0a84b-f1c3-7d10-8a62-1c4e8b37d9f5",
		fingerly.DeviceCheckWrite{Bit0: fingerly.Bool(true)}); err != nil {
		// Keep the device unmarked and write again on a later identification.
		log.Print(err)
	}
}

// Verify checks the event and consumes it last, so a replayed request id
// fails with already_consumed. The score policy stays with the caller.
func ExampleEventsService_Verify() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

	tag := "checkout"
	result, err := client.Events.Verify(context.Background(), "01a0a84b-e6a2-7c09-9f51-0b3d7a26c8e4",
		&fingerly.VerifyOptions{Tag: &tag})
	switch {
	case errors.Is(err, fingerly.ErrEventPending):
		// Not a verdict: ask the caller to try again shortly.
	case err != nil:
		log.Fatal(err)
	case !result.OK:
		fmt.Println("refused:", result.Reason)
	case result.Event.SuspectLevel == fingerly.SuspectLevelHigh:
		fmt.Println("refused: high risk")
	default:
		fmt.Println("allowed")
	}
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
		Path:     "/k7x2qm4a",
		ClientIP: func(r *http.Request) string {
			// The header your load balancer or CDN sets. Trust it only when
			// traffic can reach this server through that hop alone; never
			// trust the left-most X-Forwarded-For value, which the visitor
			// writes.
			if address := r.Header.Get("X-Real-IP"); address != "" {
				return address
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			return host
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/k7x2qm4a", proxy)
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// AgentSightings reports what the site served after each response, and
// answers from memory what sent a request.
func ExampleNewAgentSightings() {
	client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))
	sightings, err := fingerly.NewAgentSightings(client, fingerly.AgentSightingsOptions{
		// The same trusted source as the proxy's ClientIP.
		ClientIP: func(r *http.Request) string {
			if address := r.Header.Get("X-Real-IP"); address != "" {
				return address
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			return host
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := sightings.Lookup(r)
		if answer.AI != nil && answer.AI.Verification.Result == fingerly.AIVerificationFail {
			// It names an AI agent, and checking that claim failed.
		}
		// AgentUnknown, or anything else: serve as usual.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<!doctype html><title>Shop</title>")
	})
	server := &http.Server{Addr: ":8080", Handler: sightings.Middleware(site)}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// On SIGTERM, stop serving, then send what is still waiting.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	_ = sightings.Shutdown(shutdown)
}

func enqueue(id string, body []byte) {}
