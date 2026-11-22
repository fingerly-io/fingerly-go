# Changelog

## 0.5.1

- Names the Android `bootloader_unlocked` signal and its group.

## 0.5.0

- Added `AgentSightings`, which reports the requests your server served so
  that AI crawlers and fetchers that never run a page's JavaScript are seen,
  and remembers the answers so `Lookup` can say in memory what sent a request
  (`AgentAnswer`, or `AgentUnknown` for "serve as usual"). Mount its
  `Middleware` on any net/http router, or call `Report` yourself. Reports go
  in batches from one background goroutine and never delay a response;
  waiting reports are capped at 1000 (`Dropped` counts the rest) and
  remembered answers at `CacheSize`. `Flush(ctx)` sends what is waiting
  before a serverless invocation ends, and `Shutdown(ctx)` or `Close()` stop
  it at shutdown, with `Shutdown` giving up when its context is done. A panic
  in the `ClientIP` or `OnError` callback is recovered and reported, and a
  `Retry-After` over a day waits a day.
- `AgentSightings.Middleware` reports a page by the `Content-Type` net/http
  sends: one written without a type is sniffed from its first 512 bytes, as
  net/http sniffs it, also after an explicit `WriteHeader`, and not at all
  under a `Content-Encoding`. The writer it hands your handler also keeps
  `io.ReaderFrom`, `http.Pusher` and `http.CloseNotifier` (frameworks that
  assert it, such as gin's `Stream`, no longer panic), and
  `http.ResponseController.Flush` returns the underlying writer's error.
- `AgentSightings.Shutdown` keeps its deadline, and `Close` returns, when
  `OnError` blocks or calls either of them itself.
- `AgentSightings` leaves out a `Host` over 4096 bytes, so waiting reports
  stay small, and does not report a request whose method is over 4096 bytes.
  Every length is measured as the API measures it, with each byte that is not
  UTF-8 counted as the three bytes of U+FFFD it is sent as, so no report holds
  an item the API would refuse the whole batch for.
- `Client.Agents.Sightings` answers an item that does not decode as
  `AgentUnknown`, in its place, instead of failing the whole answer.
- Added `VisitorKindUnknown`, the answer the API gives a reported request it
  could not use: TTL 0, so it is never remembered and `Lookup` answers it
  `AgentUnknown`, while the other requests of the batch are answered as
  usual. A `422` now means no request in the batch was usable.
- `APIError.RetryAfter` holds a `Retry-After` too large for an `int` at the
  largest `int` instead of none, so a sightings `429` with one waits a day.
- `Events.List` sends `From` and `To` with their fractions of a second, so the
  exclusive `To` no longer leaves out the events of its last second.
- Added `Client.Agents.Sightings(ctx, items)`, the call underneath, with
  `AgentSighting`, `AgentSightingsAnswer` and `AgentSightingKey`, and
  `APIError.RateLimitScope` with `RateLimitScopeDaily` for a `429` from the
  daily allowance.
- Added `Event.AI`, the AI agent behind an event whose `VisitorKind` is
  `VisitorKindAIBot`, as an `AIAgent` with its operator, `AIPurpose`,
  `AIRuntime` and `AIVerification` (`AIVerificationMethod` and
  `AIVerificationResult`), and the `Kind`, `AIProvider` and `AIPurpose`
  filters on `EventListParams`.
- The proxy forwards the Web Bot Auth `Signature`, `Signature-Input` and
  `Signature-Agent` headers, each up to 4096 bytes.
- Added `SignalAISpoofed` and `SignalIdentityObfuscated`.
- Breaking: `Event.VisitorKind` is a `VisitorKind`, with a constant per
  value; comparing with a constant or a string literal is unchanged. The
  `SignalVPNTimezone` constant is removed, since the server no longer reports
  it.
- The README, `DeviceCheckService` and its example describe one read and one
  write token per identification: a check-then-mark flow reads and writes on
  the same request ID.

## 0.4.1

- Each `AttestSkippedReason` constant says when the iOS and Android SDKs send
  it.
- The vendored OpenAPI contract declares the enum values for the fixed string
  fields, matching the server.

## 0.4.0

- Added named string types with a constant per value for every closed set the
  API reports: `Signal` (every signal group and raw detector, including the
  detectors `SignalVPNPublic`, `SignalProxy`, `SignalRepeatedReinstall`,
  `SignalDeviceActivityHigh` and the rest), `SuspectLevel`, `Confidence`,
  `AttestationState`, `AttestationStatus`, `AttestSkippedReason`,
  `EventState` (including `pending`), `AnonymityNetwork`
  (including `relay`), `Environment`, `RiskPlatform`, `RiskSource`,
  `WebhookEventType` and `WebhookEnvironment`, and `VerifyReasonOK` for the
  `""` reason of a passed verification.
- Breaking: the matching fields now have those types (`Event.SuspectLevel`,
  `Event.State`, `Event.Environment`, `Event.Platform`,
  `Event.AnonymityNetwork`, `EventTrigger.Signal` and `Confidence`,
  `EventRiskTrigger.Signal` and `Confidence`, `EventRiskReport.Platform`,
  `Level` and `Source`, `Attestation.State`, `Status` and `AttestSkipped`,
  `EventListParams.Level`, `WebhookEvent.Type` and `Environment`), and the
  `Signal*`, `AttestationState*` and `AttestationStatus*` constants are typed.
  Comparing with a constant or a string literal is unchanged; a `string`
  variable needs a conversion, such as `fingerly.Signal(name)`. Values the
  server adds later still decode.

## 0.3.0

- Added `Events.Verify(ctx, requestID, *VerifyOptions)`, which reads an event
  and checks it exists, is recent (`MaxAgeSeconds`, 300 by default), carries
  the expected `Tag`, is identifiable and scored, then consumes it once
  (`SkipConsume` to skip), reporting a `VerifyReason` in a `VerifyResult`, and
  `Events.Consume(ctx, requestID)` with a typed `EventAlreadyConsumedError`
  for `event_already_consumed`, matching `ErrEventAlreadyConsumed`.
- Added `Client.DeviceCheck` with `Query` and `Write`, which spend an iOS
  event's held DeviceCheck token to read or write the device's two bits,
  `DeviceCheckBits`, `DeviceCheckWrite`, `DeviceCheckWriteResult`, `Bool`,
  `APIError.AppleStatus`, and a sentinel per DeviceCheck error code. A `503`
  or a transport failure is retried with the identical request.
- Refreshed the vendored v3 OpenAPI and contract fixtures, removing retired
  deferred supplement schemas and routes, and added `Event.Reason`,
  `Event.Identifiable()`, `Event.ParseDocument()`, `EventDocument`,
  `Attestation` (with `AttestSkipped` and `Downgrade`), and v3 signal
  constants (`SignalAttestationFailed`, `SignalAttestationDowngrade`,
  `SignalDeviceInconsistent`, `SignalReportIncomplete`, `SignalProbeErrors`,
  `SignalFetchMetadataMissing`, `SignalWebTicketMissing`,
  `SignalWebTicketInvalid`, `SignalTampering`, `SignalCustomRootCA`,
  `SignalSecondaryUserProfile`).
- Updated `Proxy` to enforce the single-path `proxy.md` relay contract:
  sanitize-and-restore validation of `X-Fingerly-SDK-Platform`, rejection with
  HTTP `400` (`missing_origin`) when both `Origin` and a valid
  `X-Fingerly-SDK-Platform` are absent (never synthesising `X-Fingerly-Origin`),
  forwarding of `Sec-Fetch-*` and `Sec-CH-UA-*` client metadata headers,
  optional cross-origin `AllowedOrigins` CORS handling, and verbatim raw-byte
  forwarding.
- `NewProxy` requires `ProxyKey` and has no secret key option: every request
  must present a public key of the proxy key's environment or gets a `401`,
  and a secret key is never sent upstream.

## 0.2.0

- `Event.ProjectID` is the Fingerly project an event belongs to, on single
  reads and list rows; empty when the server does not send it. Webhook `data`
  for `identification.completed` and `visitor.suspect` (and
  `identification.refused` when the key was known) carries `project_id`.
- `Event.IPAddress` is replaced by `Event.IPNetwork`: the API now returns the
  network a request came from (an IPv4 /24 or an IPv6 /40), never the address.
  `Event.DeferredSignals` is removed, as the API no longer returns the deferred
  report. `Event.Document` is now an allow-listed view of the archived
  submission, without raw signals, user agent or address.
- The vendored contract snapshot is refreshed.

## 0.1.1

- Security: `Events.Get` refuses an empty request id, `.` and `..` with
  `ErrInvalidRequestID` before sending anything. Percent-escaping leaves dots
  alone and servers and proxies resolve dot segments, so `..` reached a route
  above `/events` with the secret key attached, and `""` or `.` read the event
  list, which a caller could take for one event. The request id usually comes
  from the browser, so a visitor could choose it. Ids that merely contain a
  dot, such as `a.b`, are sent as before.

## 0.1.0

First release: event reads, webhook verification and the same-origin proxy.
Changes made during pre-release testing:

- One shared connection pool for every `Client` and `Proxy`, keeping 256 idle
  connections per host and rotating every five minutes so DNS changes are
  picked up. `http.DefaultTransport` kept two, which under a few hundred
  requests in flight opened a connection per request and exhausted local ports.
- The proxy reads the upstream answer in full, capped at 4 MiB, before writing
  anything, so a stalled or broken upstream is a `502` rather than a `200`
  with a truncated body.
- The proxy answers `504` when its deadline passes and `502` for any other
  upstream failure, and reports each one to `ProxyOptions.OnError` or as one
  `slog` line. A visitor who disconnects cancels the upstream call quietly.
- A deadline a middleware put on the request is now answered `504` rather than
  an empty `200`, an upstream status outside 2xx to 5xx is a `502` rather than
  a panic, and the pool no longer panics when `http.DefaultTransport` has been
  replaced by a wrapper.
- `ClientIP` must return exactly one IPv4 or IPv6 address; anything else is
  answered `500` without calling upstream and reported once.
- The proxy path is matched exactly: no trailing slash, dot segment, encoded
  character or case difference.
- `Client`, `Proxy`, `ProxyOptions` and `EventsService` print keys as
  `[redacted]` with every `fmt` verb, in `slog` and in JSON.
- `APIError.RetryAfter` carries the `Retry-After` seconds; `RequestID` falls
  back to the `X-Request-Id` header. Transport failures are a
  `*ConnectionError` (`errors.Is(err, ErrTimeout)` for deadlines), and answers
  over 4 MiB return `ErrResponseTooLarge`.
- Webhook verification checks at most 32 signature candidates.
- `ErrEventPending` matches `404 event_pending` with `errors.Is`: the request
  id was accepted but the event is not recorded yet, and the `*APIError`
  carries `RetryAfter`. `ErrEventNotFound` still matches only
  `event_not_found` (never issued, or past retention). Neither is retried.
