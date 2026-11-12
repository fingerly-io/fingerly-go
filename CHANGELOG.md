# Changelog

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
