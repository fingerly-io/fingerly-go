<p align="center">
  <a href="https://fingerly.io">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/fingerly-io/.github/main/assets/logo-dark.svg">
      <img src="https://raw.githubusercontent.com/fingerly-io/.github/main/assets/logo-light.svg" alt="Fingerly" height="44">
    </picture>
  </a>
</p>

<p align="center"><b>Fingerly for Go</b><br>Read events, verify webhooks and serve a first-party proxy from your Go backend, using only the standard library.</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/fingerly-io/fingerly-go"><img src="https://img.shields.io/github/v/tag/fingerly-io/fingerly-go?style=flat-square&sort=semver&label=version" alt="Latest version"></a>
  <a href="https://github.com/fingerly-io/fingerly-go/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/fingerly-io/fingerly-go/ci.yml?branch=main&style=flat-square&label=CI" alt="CI status"></a>
  <img src="https://img.shields.io/badge/go-1.22%2B-blue?style=flat-square" alt="Go 1.22 or newer">
  <a href="https://github.com/fingerly-io/fingerly-go/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue?style=flat-square" alt="Apache-2.0 licence"></a>
  <a href="https://docs.fingerly.io/docs/sdks/go"><img src="https://img.shields.io/badge/docs-docs.fingerly.io-blue?style=flat-square" alt="Documentation"></a>
</p>

<p align="center"><img src="https://raw.githubusercontent.com/fingerly-io/.github/main/assets/banner.webp" alt="Fingerly" width="100%"></p>

## What is this

`fingerly-go` is the server-side Go package for [Fingerly](https://fingerly.io), which identifies the device behind every web and app session and scores how suspicious it is. Use it to protect signups, logins and payments: read the event the browser SDK produced with a server-only secret key, verify webhook signatures, and relay the browser SDK's traffic through your own domain.

Go 1.22 or newer. No dependencies outside the standard library.

## Getting started

**1. Prerequisites.** A Fingerly account and a secret key (see [API keys and environments](https://docs.fingerly.io/docs/api-keys)). A webhook signing secret to verify webhooks, and a proxy key (see [Proxy integrations](https://docs.fingerly.io/docs/proxy-integrations)) for the proxy.

**2. Install.**

```sh
go get github.com/fingerly-io/fingerly-go@v0.3.0
```

**3. Read an event.**

```go
import fingerly "github.com/fingerly-io/fingerly-go"

client := fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

event, err := client.Events.Get(ctx, requestID)
page, err := client.Events.List(ctx, &fingerly.EventListParams{Level: fingerly.SuspectLevelHigh, Limit: 50})
```

**4. Verify a webhook.**

```go
body, _ := io.ReadAll(r.Body)
ok := fingerly.VerifyWebhook(
	os.Getenv("FINGERLY_WEBHOOK_SECRET"),
	body,
	r.Header.Get("X-Fingerly-Timestamp"),
	r.Header.Get("X-Fingerly-Signature"),
)
```

Full documentation: [docs.fingerly.io/docs/sdks/go](https://docs.fingerly.io/docs/sdks/go).

## Choose what you need

| I want to | Go to |
| --- | --- |
| Read an event, or verify and consume it for a backend action | [Read events](#read-events), [Verify a backend action](#verify-a-backend-action) |
| Handle errors, deadlines and unscored events | [Errors](#errors), [Deadlines and cancellation](#deadlines-and-cancellation), [Unscored events](#unscored-events) |
| Read or write an iOS device's DeviceCheck bits | [DeviceCheck bits on iOS](#devicecheck-bits-on-ios) |
| Check a webhook signature | [Verify webhooks](#verify-webhooks) |
| Keep identification working behind content blockers | [First-party proxy](#first-party-proxy) |

## Contents

- [Read events](#read-events)
- [Deadlines and cancellation](#deadlines-and-cancellation)
- [Errors](#errors)
- [Unscored events](#unscored-events)
- [Read an event you just identified](#read-an-event-you-just-identified)
- [Verify a backend action](#verify-a-backend-action)
- [DeviceCheck bits on iOS](#devicecheck-bits-on-ios)
- [Verify webhooks](#verify-webhooks)
- [First-party proxy](#first-party-proxy)
- [Development and release preparation](#development-and-release-preparation)
- [Requirements](#requirements)
- [Documentation](#documentation)
- [Help](#help)
- [Licence](#licence)

## Read events

Event reads require `fly_sk_{production|staging|development}_…` keys. Requests go to `https://api.fingerly.io`; `WithEndpoint` overrides that for a local API. The endpoint must be a bare `https://` origin: scheme, host and optional port, with no path, query, fragment, credentials, space or control byte. A plain `http://` endpoint is accepted only for `127.0.0.1`, `localhost` or `[::1]`, and only together with `WithAllowInsecureLoopback()`, which is meant for a local test harness. Configuration errors never repeat the endpoint or the key. Public, proxy, management, malformed and unsupported keys are rejected before network access. Requests do not follow redirects. `Get` and `List` each perform one request and never retry.

`New` returns only a client, so it composes in a package-level `var`. An invalid key or option is reported by the first call, before anything is sent.

Every `Client` and `Proxy` in the process shares one connection pool, which keeps 256 idle connections per host and is replaced every five minutes so a DNS change is picked up. There is nothing to tune for a busy server. Pass `WithHTTPClient` or `ProxyOptions.Transport` only for a custom HTTP stack; an `http.Client` with a nil `Transport` still gets the shared pool.

## Deadlines and cancellation

Every call takes a `context.Context`, and a Go request context governs the whole exchange, so the deadline covers reading the response body too. The client applies `WithTimeout` (10s by default) on top of whatever the caller's context already carries, whichever is sooner. Cancelling the context stops the request.

## Errors

Any response outside 2xx, redirects included, returns an `*APIError` with `Status`, and optional `Code`, `Details`, `RequestID` and `RetryAfter`. The secret key is stripped from all of them, and the response body is never echoed. `RetryAfter` is the number of seconds from the `Retry-After` header, or nil when there was none; the SDK never retries a read itself.

```go
var apiErr *fingerly.APIError
if errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests && apiErr.RetryAfter != nil {
	wait := time.Duration(*apiErr.RetryAfter) * time.Second
	_ = wait // back off for at least this long
}
```

A `404` from `Events.Get` carries one of two codes, each with a sentinel, so neither needs a status check:

| Sentinel | Meaning |
| --- | --- |
| `fingerly.ErrEventPending` | Accepted but not recorded yet: retry after `apiErr.RetryAfter` seconds. |
| `fingerly.ErrEventNotFound` | Never issued for this key's environment, or past the 30-day retention. |

```go
switch {
case errors.Is(err, fingerly.ErrEventPending):
	// Accepted but not recorded yet: retry after apiErr.RetryAfter seconds.
case errors.Is(err, fingerly.ErrEventNotFound):
	// Never issued for this key's environment, or past the 30-day retention.
}
```

A read that got no complete answer (refused, reset, timed out, or broken off mid-body) returns a `*fingerly.ConnectionError`, and `errors.Is(err, fingerly.ErrTimeout)` is true when a deadline passed. Its message names neither the key nor the endpoint. An answer over 4 MiB returns `fingerly.ErrResponseTooLarge`.

Printing a `Client`, `Proxy` or `ProxyOptions` with any `fmt` verb, `slog` or `encoding/json` shows the keys as `[redacted]`.

## Unscored events

`Event.SuspectScore` is a `*int` because nil means unscored, which must never be read as zero: a visitor about whom nothing fired scores 0, while a submission that was never enriched has no score at all. `Scored()` and `Score()` state that distinction at the call site.

```go
score, ok := event.Score()
if !ok {
	// Unscored. Decide on something else; do not treat it as clean.
}
```

`Event.Raw` and `EventPage.Raw` keep the response body exactly as it arrived, so a field the API adds later is reachable without an SDK upgrade.

## Read an event you just identified

```go
event, err := client.Events.Get(ctx, requestID)
```

If the event is not recorded yet, the error matches `ErrEventPending` and its `*APIError` carries `RetryAfter`. That is not a verdict: try again after that many seconds, or verify asynchronously, for example from a webhook. The SDK never retries on its own. `ErrEventNotFound` means the request id was never issued or the event is past retention.

The archived `Document` arrives moments after the event itself, so check `HasDocument` rather than assuming it is there, and call `event.ParseDocument()` to decode it into a typed `*EventDocument` (including `*Attestation` and `*EventRiskReport`). When a submission is unidentifiable (`!event.Identifiable()`, such as insufficient entropy, a genuine embedded WebView, or an unmatched high-risk Web payload), `event.VisitorID` is `""`; there are no `deferred_token`, `deferred_signals` or `supplements` fields in `v: 3`.

Every closed set the API reports is a named string type with a constant per value, so compare against the constants rather than string literals:

| Type | Fields | Constants |
| --- | --- | --- |
| `Signal` | `EventTrigger.Signal` (groups), `EventRiskTrigger.Signal` (raw detectors) | `SignalVPN`, `SignalBot`, `SignalFingerprintSuppressed`, `SignalAttestationFailed`, `SignalVPNPublic`, ... (every group and detector) |
| `SuspectLevel` | `Event.SuspectLevel`, `EventRiskReport.Level`, `EventListParams.Level` | `SuspectLevelLow`, `SuspectLevelMedium`, `SuspectLevelHigh` |
| `Confidence` | `EventTrigger.Confidence`, `EventRiskTrigger.Confidence` | `ConfidenceLow`, `ConfidenceMedium`, `ConfidenceHigh` |
| `AttestationState` | `Attestation.State` | `AttestationStateAttested`, `AttestationStateNotAttested`, `AttestationStateFailed` |
| `AttestationStatus` | `Attestation.Status` | `AttestationStatusVerified`, `AttestationStatusIndeterminate`, `AttestationStatusInvalid`, `AttestationStatusAbsent`, `AttestationStatusUnconfigured` |
| `AttestSkippedReason` | `Attestation.AttestSkipped` | `AttestSkippedNotOffered`, `AttestSkippedUnsupported`, `AttestSkippedChallengeUnavailable`, `AttestSkippedKeyError`, `AttestSkippedTimeout`, `AttestSkippedBackoff`, `AttestSkippedInstallUnavailable` |
| `EventState` | `Event.State` | `EventStatePending`, `EventStateEnriched`, `EventStateUnavailable` |
| `AnonymityNetwork` | `Event.AnonymityNetwork` | `AnonymityNetworkTor`, `AnonymityNetworkRelay`, `AnonymityNetworkVPN`, `AnonymityNetworkProxy`, `AnonymityNetworkHosting` |
| `Environment` | `Event.Environment` | `EnvironmentProduction`, `EnvironmentStaging`, `EnvironmentDevelopment` |
| `RiskPlatform` | `Event.Platform`, `EventRiskReport.Platform` | `RiskPlatformWeb`, `RiskPlatformIOS`, `RiskPlatformAndroid` |
| `RiskSource` | `EventRiskReport.Source` | `RiskSourceKey`, `RiskSourceOrganization`, `RiskSourceDefault` |
| `VerifyReason` | `VerifyResult.Reason` | `VerifyReasonOK` (`""`), `VerifyNotFound`, `VerifyTooOld`, `VerifyTagMismatch`, `VerifyUnidentifiable`, `VerifyUnscored`, `VerifyAlreadyConsumed` |
| `WebhookEventType` | `WebhookEvent.Type` | `WebhookIdentificationCompleted`, `WebhookIdentificationRefused`, `WebhookVisitorSuspect`, `WebhookBillingStatusChanged`, `WebhookUsageDailySettled`, `WebhookTest` |
| `WebhookEnvironment` | `WebhookEvent.Environment` | `WebhookEnvironmentLive`, `WebhookEnvironmentTest` |

A value the server adds after this release still decodes, as a value no constant names. A `string` variable needs a conversion to compare, such as `fingerly.Signal(name)`.

A native `Attestation` also carries `AttestSkipped` (the app's reason for sending no proof) and `Downgrade` (the report named an install an attested device owns).

## Verify a backend action

Verify the event with a server-only key, then apply your own policy to the server's verdict. Do not authorize an action using scores or visitor identifiers supplied by the browser. Tags correlate events; they do not replace your application's authentication or replay protection. Never put a personal identifier (user ID, email, install ID, advertising ID) in a tag: the server keeps tag values in long-lived rollups.

`client.Events.Verify` reads the event and runs these checks in order, stopping at the first that fails, whose `VerifyReason` it reports:

1. `VerifyNotFound` (`"not_found"`): the event does not exist. `VerifyResult.Event` is nil only for this reason.
2. `VerifyTooOld` (`"too_old"`): it occurred more than `MaxAgeSeconds` ago (300 by default, `DefaultVerifyMaxAge`), or its `occurred_at` is missing or unreadable.
3. `VerifyTagMismatch` (`"tag_mismatch"`): `Tag` was given and the event's tag is not exactly that. A nil `Tag` skips this check; an empty one is refused.
4. `VerifyUnidentifiable` (`"unidentifiable"`): the event has no `VisitorID`.
5. `VerifyUnscored` (`"unscored"`): the event has no suspect score.
6. `VerifyAlreadyConsumed` (`"already_consumed"`): every check above passed, but the event was already consumed.

Only when every other check passes does `Verify` consume the event, last, so one event backs one decision and a replayed request id fails. Set `SkipConsume: true` to check without consuming. When everything passes, `OK` is true and `Reason` is `VerifyReasonOK` (`""`). A negative `MaxAgeSeconds` or an empty `Tag` returns `ErrInvalidVerifyOptions` before any request.

A failed check is a result, not an error. Every other failure is returned unchanged as the error, including `ErrEventPending`, which is not a verdict. `Verify` never looks at the score's value, the level or the triggers: that policy stays with you.

`Event.ProjectID` is the Fingerly project the event belongs to, on both `Events.Get` and every row of `Events.List`. It is empty when an older server does not send it.

<details>
<summary>Complete verification server</summary>

```go
package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/fingerly-io/fingerly-go"
)

var client = fingerly.New(os.Getenv("FINGERLY_SECRET_KEY"))

// POST /checkout/{request_id}
func checkout(w http.ResponseWriter, r *http.Request) {
	tag := "checkout"
	result, err := client.Events.Verify(r.Context(), r.PathValue("request_id"),
		&fingerly.VerifyOptions{Tag: &tag})
	var apiErr *fingerly.APIError
	switch {
	case errors.Is(err, fingerly.ErrEventPending) && errors.As(err, &apiErr):
		// Not decided yet, not fraud: ask the caller to come back.
		wait := 2
		if apiErr.RetryAfter != nil {
			wait = *apiErr.RetryAfter
		}
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		http.Error(w, "verification pending", http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, "verification unavailable", http.StatusBadGateway)
		return
	case !result.OK:
		// not_found, too_old, tag_mismatch, unidentifiable, unscored or
		// already_consumed.
		http.Error(w, "verification failed: "+string(result.Reason), http.StatusForbidden)
		return
	}
	// The event is recent, tagged, identified, scored and now consumed. The
	// score policy is yours: result.Event.SuspectLevel, Score(), triggers and
	// ParseDocument().
	event := result.Event
	if event.SuspectLevel == fingerly.SuspectLevelHigh {
		http.Error(w, "checkout refused", http.StatusForbidden)
		return
	}
	for _, trigger := range event.Triggers {
		if trigger.Signal == fingerly.SignalAttestationFailed {
			http.Error(w, "attestation failed", http.StatusForbidden)
			return
		}
	}
	if doc, err := event.ParseDocument(); err == nil && doc != nil && doc.Attestation != nil &&
		doc.Attestation.State == fingerly.AttestationStateFailed {
		http.Error(w, "attestation failed", http.StatusForbidden)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func main() {
	http.HandleFunc("POST /checkout/{request_id}", checkout)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

</details>

To consume an event on its own, call `client.Events.Consume`. It is a POST with no body and is never retried. The first call returns a `*ConsumeResult` with `ConsumedAt` and `First`; any later call returns an `*EventAlreadyConsumedError`, which matches `ErrEventAlreadyConsumed` with `errors.Is`, reaches its `*APIError` (status `409`, code `event_already_consumed`) with `errors.As`, and carries `ConsumedAt` from the first call, or `""` when the answer did not say.

```go
_, err := client.Events.Consume(ctx, requestID)
var consumed *fingerly.EventAlreadyConsumedError
if errors.As(err, &consumed) {
	// Already used at consumed.ConsumedAt: refuse the replay.
}
```

## DeviceCheck bits on iOS

When DeviceCheck is turned on for the project's Apple key, Fingerly holds the Apple DeviceCheck token of an iOS identification for one hour. `client.DeviceCheck` spends it, once, to read or to write the two bits Apple keeps for the device. The bits survive a reinstall and mean what you decide.

```go
bits, err := client.DeviceCheck.Query(ctx, requestID)
// bits.Found, bits.Bit0, bits.Bit1, bits.LastUpdate ("YYYY-MM")

_, err = client.DeviceCheck.Write(ctx, requestID, fingerly.DeviceCheckWrite{Bit0: fingerly.Bool(true)})
```

Apple treats a DeviceCheck token as single-use, so a read and a write each need their own identification. A once-per-device free trial takes two:

1. On the identification that asks for the trial, call `Query`. If the bits say the device already had one, refuse it.
2. Otherwise grant the trial and record the grant in your own data, with the device not yet marked.
3. On the next identification from that install (the app can identify again right after the grant), call `Write` with the bit, then mark the record. Until the write lands, your own record is what stops a second trial.

Each answer has a sentinel for `errors.Is`; the concrete error is an `*APIError`:

| Sentinel | Status and code | What to do |
| --- | --- | --- |
| `ErrDeviceCheckNotHeld` | `404 device_check_not_held` | No token for that event. Decide without the bits. |
| `ErrDeviceCheckSpent` | `409 device_check_spent` | Already used, or not the request it was first used for. Use another identification. |
| `ErrDeviceCheckBusy` | `409 device_check_busy` | Another call is using it. Retry in a moment. |
| `ErrCredentialUnavailable` | `409 credential_unavailable` | Fix the Apple key, then retry within the hour. |
| `ErrBitsRequired` | `422 bits_required` | Name `Bit0`, `Bit1` or both. Also returned, without a request, by a `Write` naming neither. |
| `ErrAppleRejected` | `422 apple_rejected` | Apple rejected the token; `APIError.AppleStatus` holds Apple's HTTP status. |
| `ErrDeviceCheckUnavailable` | `503 device_check_unavailable` | The token is kept. Retry the same call within the hour. |

A retry must be the same request: while a token is kept after an answer that was not definite, the API accepts only the operation and bits it was first used for, and answers anything else with `device_check_spent`. The SDK retries a `503` or a call that got no complete answer up to twice, with the identical request, and nothing else. When you retry later yourself, repeat the same call with the same bits.

## Verify webhooks

The example under [Getting started](#getting-started) calls `fingerly.VerifyWebhook`.

Verify the exact raw bytes before decoding them. Signatures cover the timestamp and body. The default replay tolerance is five minutes; persist processed `WebhookEvent.ID` values to deduplicate deliveries. During signing-secret rotation, any matching signature among the first 32 in the comma-separated header is accepted. Malformed or missing input is a false result, never a panic.

`WebhookEvent` decodes the envelope every delivery shares and leaves `Data` raw, so a new event type needs no SDK upgrade. Switch on `Type` with the `WebhookEventType` constants, such as `fingerly.WebhookIdentificationCompleted`. The `identification.completed` and `visitor.suspect` payloads carry `data.project_id`, the project the event belongs to; `identification.refused` carries it only when the refused request named a known key.

## First-party proxy

Content blockers refuse browser requests to third-party APIs, and a refused identification is a visitor never identified. Mount the proxy on a path of your own site and those requests become first-party. The proxy key is what lets that hop declare the visitor's real address, origin and user agent; a public key alone never can, so nobody can spoof them.

Create a proxy in the dashboard (**Integration > Proxy keys**) and mount the handler at the path it issued, here `/k7x2qm4a`:

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

Trust that header only when traffic can reach the server through that load balancer or CDN alone; otherwise a visitor can set it. `Proxy` implements `http.Handler`, so every router built on net/http takes it directly, and fiber through its adaptor:

| Router | Mount |
| --- | --- |
| net/http | `mux.Handle("/k7x2qm4a", proxy)` |
| gorilla/mux | `router.Handle("/k7x2qm4a", proxy)` |
| chi | `r.Handle("/k7x2qm4a", proxy)` |
| gin | `router.Any("/k7x2qm4a", gin.WrapH(proxy))` |
| echo | `e.Any("/k7x2qm4a", echo.WrapHandler(proxy))` |
| fiber | `app.All("/k7x2qm4a", adaptor.HTTPHandler(proxy))` |

Then point the client SDK at the same path. Browser and proxied native SDKs post a single `v: 3` identification envelope there, with nothing appended:

```ts
const fingerly = await load({ apiKey: 'fly_pk_…', endpoints: '/k7x2qm4a' })
```

Unlike `New`, `NewProxy` reports configuration errors immediately: a proxy is wired up once at start-up, where an error is natural to handle.

### What the proxy does

With `Path` set it answers only that path, matched exactly: a trailing or doubled slash, a dot segment, an encoded character, a different case or a path under it (including retired `/supplement` paths) all get `404`. `Path` is optional: leave it empty when the route already sends only that path to the handler, and behind `http.StripPrefix`, which changes the path the handler sees. It must be an absolute path such as `/k7x2qm4a`, or `NewProxy` returns an error. Only `POST` (plus `OPTIONS` when `AllowedOrigins` is configured) is accepted, compared case-sensitively; any other method, `post` included, gets `405`. Every answer the proxy writes itself, `404` and `405` included, carries `Cache-Control: no-store`.

Every forwarded request goes with its raw body bytes untouched (never JSON re-encoded) to one URL, `https://api.fingerly.io/api/v1/ingest` (`APIVersion` and `Upstream` change its version and origin). Event reads need a secret key and stay unreachable from a browser by construction.

`Upstream` must be a bare `https://` origin: scheme, host and optional port, with no path, query, fragment, credentials, space or control byte, or `NewProxy` returns an error. A plain `http://` upstream is accepted only for `127.0.0.1`, `localhost` or `[::1]`, and only with `AllowInsecureLoopback: true`, which is meant for a local test harness. Redirects are never followed, with the shared pool or your own `Transport`; an upstream `3xx`, or a status outside `200` to `599`, gets `502`.

| Situation | Answer |
| --- | --- |
| Path other than the configured one | `404` |
| Method other than `POST` (or `OPTIONS` with `AllowedOrigins` configured) | `405` |
| Neither an `Origin` nor a validated `X-Fingerly-SDK-Platform` | `400` (`missing_origin`) |
| Public key from a different environment than the proxy key | `401` |
| Body over 1 MiB | `413` |
| `Timeout` passed | `504` |
| Upstream `3xx` or a status outside `200` to `599` | `502` |
| Any other upstream failure: refused, reset, DNS, a truncated or malformed answer, an answer over 4 MiB | `502` |
| `ClientIP` returns no valid address | `500` with "The client IP callback returned no valid address" |

The caller must present a public key from the same environment as the proxy key, or the request gets `401`. Incoming headers follow the sanitize-and-restore sequence: all incoming `X-Fingerly-*`, `Cookie` and `Authorization` headers are stripped, restoring `X-Fingerly-SDK-Platform` only if it matched an exact lowercase native or bridge platform (`ios`, `android`, `react-native`, `react-native-ios`, `react-native-android`, `flutter`, `flutter-ios`, `flutter-android`, `lynx-ios`, `lynx-android`, `capacitor`, `kotlin-multiplatform`). A request carries one channel upstream: with an `Origin` it goes as a browser request (`X-Fingerly-Origin`) and the platform header is not forwarded; without one, the validated platform is. When both `Origin` and a validated `X-Fingerly-SDK-Platform` are absent, the proxy rejects locally with `400` (`missing_origin`), and it never synthesizes `X-Fingerly-Origin` from `Host`, `Referer` or the request URL. Bodies over 1 MiB get `413`. Alongside verified proxy metadata (`X-Fingerly-Host` included), only `Content-Type`, `X-Api-Key`, `User-Agent`, `Idempotency-Key`, `Sec-Fetch-*` and `Sec-CH-UA-*` are forwarded. The proxy reports itself upstream as `go/0.3.0`, which the dashboard shows per proxy.

One deadline, `Timeout` (5 seconds by default), covers the whole upstream exchange, body included, so a trickling answer cannot stretch it. When it passes the browser gets `504`; any other upstream failure (refused, reset, DNS, a truncated or malformed answer, an answer over 4 MiB) gets `502`. Both carry a short `text/plain` body and `Cache-Control: no-store`. The answer is read in full before anything is written, so a broken upstream never reaches the browser as a `200`. A visitor who disconnects cancels the upstream call, except under fiber, whose adaptor has no disconnect signal, so the call runs to `Timeout`.

`Timeout` bounds the upstream call, not the visitor's upload: a client that trickles its request body holds a handler until your `http.Server` gives up, so set `ReadHeaderTimeout` and `ReadTimeout` on it. A bare `http.ListenAndServe` has neither.

Each failure is reported once: to `ProxyOptions.OnError` if you set it, otherwise as one line through `slog.Default()`. `failure.Err` names the kind of failure only (a timeout, an unreachable upstream, a TLS failure, an unrelayable status or answer), never the upstream host, port or URL, and never a key.

```go
OnError: func(r *http.Request, failure *fingerly.ProxyError) {
	log.Printf("identification proxy answered %d: %v", failure.Status, failure.Err)
},
```

Responses are marked `no-store`, and the only upstream headers relayed back are `Content-Type`, `X-Request-Id`, `RateLimit-Limit`, `RateLimit-Remaining` and `Retry-After`, so nothing on the response names Fingerly. Use the path the dashboard issued: blockers look for words such as `fingerprint`, `tracking`, `analytics` and `fingerly` in the URL.

> **`ClientIP` is the one thing you must get right.** Resolve it from a header your own infrastructure sets, such as your load balancer's or CDN's, and fall back to `r.RemoteAddr`. Never read the left-most `X-Forwarded-For` value: the visitor writes it. It must return exactly one IPv4 or IPv6 address. Anything else (empty, a list, a port) is a configuration error: the proxy answers `500` with "The client IP callback returned no valid address", calls nothing upstream, and reports it once.

## Development and release preparation

```sh
gofmt -l .
go vet ./...
go test -race -cover ./...
go run tools/synccontract.go     # refresh the vendored contract snapshot
```

The vendored synthetic contract snapshot makes tests independent of sibling repositories. To update it from the integration workspace, set `FINGERLY_CONTRACT_DIR` to its `contract` directory. The snapshot's hashes are checked by `go test`, so there is no separate check step. Never put production secrets in fixtures.

<details>
<summary>Source structure</summary>

`fingerly.go` holds the `Client` and its options. `transport.go` performs authenticated requests and maps failures to `*APIError`. `events.go` owns the event reads and the list filters; `models.go` owns the types they decode into. `verify.go` verifies and consumes an event for a backend decision. `device_check.go` reads and writes an event's DeviceCheck bits. `webhooks.go` verifies signatures. `proxy.go` is the same-origin handler. `config.go` validates every caller-supplied option: keys, origins, API versions and durations, all before any network access. `errors.go` holds the error type and its sentinels.

</details>

## Requirements

- Go 1.22 or newer (`go 1.22` in `go.mod`; CI runs Go 1.22, 1.24 and 1.25).
- No dependencies outside the standard library.

## Documentation

- [Go SDK guide](https://docs.fingerly.io/docs/sdks/go)
- [Package reference on pkg.go.dev](https://pkg.go.dev/github.com/fingerly-io/fingerly-go)
- [Reading events](https://docs.fingerly.io/docs/reading-events) and [Webhooks](https://docs.fingerly.io/docs/webhooks)
- [Proxy integrations](https://docs.fingerly.io/docs/proxy-integrations)
- [API reference](https://docs.fingerly.io/reference/overview)

## Help

Open an issue on this repository, or write to [support@fingerly.io](mailto:support@fingerly.io). Security issues: [security@fingerly.io](mailto:security@fingerly.io).

## Licence

Apache-2.0. See [LICENSE](https://github.com/fingerly-io/fingerly-go/blob/main/LICENSE).
