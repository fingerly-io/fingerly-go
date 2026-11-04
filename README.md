# fingerly-go

Server-side event reads, webhook verification and a same-origin proxy for Go.
Go 1.22 or newer. No dependencies outside the standard library. Version 0.1.0
is prepared for release; the repository is private and the module has not been
published.

```sh
go get github.com/fingerly-io/fingerly-go
```

```go
import fingerly "github.com/fingerly-io/fingerly-go"

client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

event, err := client.Events.Get(ctx, requestID)
page, err := client.Events.List(ctx, &fingerly.EventListParams{Level: "high", Limit: 50})
```

Event reads require `fly_sk_{production|staging|development}_…` keys. Requests
go to `https://api.fingerly.io`; `WithEndpoint` overrides that for a local
API. Public, proxy, management, malformed and unsupported keys are rejected
before network access. Requests do not follow redirects. `Get` and `List` each
perform one request and never retry.

`New` returns only a client, so it composes in a package-level `var`. An
invalid key or option is reported by the first call, before anything is sent.

Every `Client` and `Proxy` in the process shares one connection pool, which
keeps 256 idle connections per host and is replaced every five minutes so a
DNS change is picked up. There is nothing to tune for a busy server. Pass
`WithHTTPClient` or `ProxyOptions.Transport` only for a custom HTTP stack; an
`http.Client` with a nil `Transport` still gets the shared pool.

## Deadlines and cancellation

Every call takes a `context.Context`, and a Go request context governs the
whole exchange, so the deadline covers reading the response body too. The
client applies `WithTimeout` (10s by default) on top of whatever the caller's
context already carries, whichever is sooner. Cancelling the context stops the
request.

## Errors

Any response outside 2xx, redirects included, returns an `*APIError` with
`Status`, and optional `Code`, `Details`, `RequestID` and `RetryAfter`. The
secret key is stripped from all of them, and the response body is never
echoed. `RetryAfter` is the number of seconds from the `Retry-After` header,
or nil when there was none; the SDK never retries a read itself.

```go
var apiErr *fingerly.APIError
if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests && apiErr.RetryAfter != nil {
	wait := time.Duration(*apiErr.RetryAfter) * time.Second
	_ = wait // back off for at least this long
}
```

A missing event has a sentinel, so it needs no status check:

```go
if errors.Is(err, fingerly.ErrEventNotFound) { /* no such request id */ }
```

A read that got no complete answer (refused, reset, timed out, or broken off
mid-body) returns a `*fingerly.ConnectionError`, and
`errors.Is(err, fingerly.ErrTimeout)` is true when a deadline passed. An
answer over 4 MiB returns `fingerly.ErrResponseTooLarge`.

Printing a `Client`, `Proxy` or `ProxyOptions` with any `fmt` verb, `slog` or
`encoding/json` shows the keys as `[redacted]`.

## Unscored events

`Event.SuspectScore` is a `*int` because nil means unscored, which must never
be read as zero: a visitor about whom nothing fired scores 0, while a
submission that was never enriched has no score at all. `Scored()` and
`Score()` state that distinction at the call site.

```go
score, ok := event.Score()
if !ok {
	// Unscored. Decide on something else; do not treat it as clean.
}
```

`Event.Raw` and `EventPage.Raw` keep the response body exactly as it arrived,
so a field the API adds later is reachable without an SDK upgrade.

## Read an event you just identified

An event is readable as soon as identification answers, so this needs nothing
special:

```go
event, err := client.Events.Get(ctx, requestID)
```

Earlier versions had a `WaitFor` that polled through the gap between
identification and the event becoming queryable. There is no gap, so
`ErrEventNotFound` now means a request id that was never issued.

The archived `Document` arrives moments after the event itself, so check
`HasDocument` rather than assuming it is there.

## Verify a backend action

Read the event with a server-only key. Check that its tag matches your action,
that it is recent, that its environment is correct, and that it was scored.
Apply your own policy to the server's verdict. Do not authorize an action using
scores or visitor identifiers supplied by the browser. Tags correlate events;
they do not replace your application's authentication or replay protection.

## Webhooks

```go
body, _ := io.ReadAll(r.Body)
ok := fingerly.VerifyWebhook(
	os.Getenv("FINGERLY_WEBHOOK_SECRET"),
	body,
	r.Header.Get("X-Fingerly-Timestamp"),
	r.Header.Get("X-Fingerly-Signature"),
)
```

Verify the exact raw bytes before decoding them. Signatures cover the timestamp
and body. The default replay tolerance is five minutes; persist processed
`WebhookEvent.ID` values to deduplicate deliveries. During signing-secret
rotation, any matching signature among the first 32 in the comma-separated
header is accepted.
Malformed or missing input is a false result, never a panic.

`WebhookEvent` decodes the envelope every delivery shares and leaves `Data`
raw, so a new event type needs no SDK upgrade.

## Same-origin proxy

Content blockers refuse browser requests to third-party APIs, and a refused
identification is a visitor never identified. Mount the proxy on a path of your
own site and those requests become first-party. The proxy key is what lets that
hop declare the visitor's real address, origin and user agent; a public key
alone never can, so nobody can spoof them.

Create a proxy in the dashboard (**Integration > Proxy keys**) and mount the
handler at the path it issued, here `/k7x2qm4a`:

```go
package main

import (
	"log"
	"net"
	"net/http"
	"os"

	fingerly "github.com/fingerly-io/fingerly-go"
)

func main() {
	proxy, err := fingerly.NewProxy(fingerly.ProxyOptions{
		ProxyKey: os.Getenv("FINGERLY_PROXY_KEY"),
		Path:     "/k7x2qm4a",
		ClientIP: func(r *http.Request) string {
			// The header your load balancer or CDN sets, else the TCP peer.
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
```

Trust that header only when traffic can reach the server through that load
balancer or CDN alone; otherwise a visitor can set it. `Proxy` implements
`http.Handler`, so every Go router takes it directly:

```go
router.Handle("/k7x2qm4a", proxy)         // gorilla/mux
r.Handle("/k7x2qm4a", proxy)              // chi
router.Any("/k7x2qm4a", gin.WrapH(proxy)) // gin
```

Then point the browser SDK at the same path. It posts both of its calls there,
exactly as given:

```ts
const fingerly = await load({ apiKey: 'fly_pk_…', endpoints: '/k7x2qm4a' })
```

Unlike `New`, `NewProxy` reports configuration errors immediately: a proxy is
wired up once at start-up, where an error is natural to handle.

### What the proxy does

With `Path` set it answers only that path, matched exactly: a trailing or
doubled slash, a dot segment, an encoded character, a different case or a path
under it all get `404`. `Path` is
optional: leave it empty when the route already sends only that path to the
handler, and behind `http.StripPrefix`, which changes the path the handler
sees. It must be an absolute path such as `/k7x2qm4a`, or `NewProxy` returns
an error. Only `POST` is forwarded; any other method gets `405`.

Every forwarded request goes to one URL, `https://api.fingerly.io/api/v1/ingest`
(`APIVersion` and `Upstream` change its version and origin). Event reads need a
secret key and stay unreachable from a browser by construction.

The browser must present a public key from the same environment as the proxy
key, or the request gets `401`. Bodies over 1 MiB get `413`. Only the content
type, the visitor metadata Fingerly needs, and an `Idempotency-Key` are
forwarded; customer cookies and authorization headers never are. The proxy
reports itself upstream as `go/0.1.0`, which the dashboard shows per proxy.

One deadline, `Timeout` (5 seconds by default), covers the whole upstream
exchange, body included, so a trickling answer cannot stretch it. When it
passes the browser gets `504`; any other upstream failure (refused, reset, DNS,
a truncated or malformed answer, an answer over 4 MiB) gets `502`. Both carry a
short `text/plain` body and `Cache-Control: no-store`. The answer is read in
full before anything is written, so a broken upstream never reaches the browser
as a `200`. A visitor who disconnects cancels the upstream call.

Each failure is reported once: to `ProxyOptions.OnError` if you set it,
otherwise as one line through `slog.Default()`.

```go
OnError: func(r *http.Request, failure *fingerly.ProxyError) {
	log.Printf("identification proxy answered %d: %v", failure.Status, failure.Err)
},
```

Responses are marked `no-store`, and the only upstream headers relayed back are
`Content-Type`, `X-Request-Id`, `RateLimit-Limit`, `RateLimit-Remaining` and
`Retry-After`, so nothing on the response names Fingerly. Use the path the
dashboard issued: blockers look for words such as `fingerprint`, `tracking`,
`analytics` and `fingerly` in the URL.

> **`ClientIP` is the one thing you must get right.** Resolve it from a header
> your own infrastructure sets, such as your load balancer's or CDN's, and fall
> back to `r.RemoteAddr`. Never read the left-most `X-Forwarded-For` value: the
> visitor writes it. It must return exactly one IPv4 or IPv6 address. Anything
> else (empty, a list, a port) is a configuration error: the proxy answers
> `500` with "The client IP callback returned no valid address", calls nothing
> upstream, and reports it once.

## Development and release preparation

```sh
gofmt -l .
go vet ./...
go test -race -cover ./...
go run tools/synccontract.go     # refresh the vendored contract snapshot
```

The vendored synthetic contract snapshot makes tests independent of sibling
repositories. To update it from the integration workspace, set
`FINGERLY_CONTRACT_DIR` to its `contract` directory. The snapshot's hashes are
checked by `go test`, so there is no separate check step. Never put production
secrets in fixtures.

## Source structure

`fingerly.go` holds the `Client` and its options. `transport.go` performs
authenticated requests and maps failures to `*APIError`. `events.go` owns the
event reads and the list filters; `models.go` owns the types they decode into.
`webhooks.go` verifies signatures. `proxy.go` is the same-origin handler.
`config.go` validates every caller-supplied option: keys, origins, API versions
and durations, all before any network access. `errors.go` holds the error type
and its sentinel.
