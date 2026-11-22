package fingerly

import (
	"bufio"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The bounds and timings every server SDK keeps for agent sightings.
const (
	// DefaultAgentCacheSize is how many answers AgentSightings remembers when
	// AgentSightingsOptions.CacheSize is 0: about 1.5 MB.
	DefaultAgentCacheSize = 4096
	// MaxAgentCacheSize is the most answers AgentSightings ever remembers,
	// about 25 MB. A larger CacheSize is lowered to it.
	MaxAgentCacheSize = 65536

	maxPendingSightings  = 1000
	maxSightingsBytes    = 64 << 10
	maxSightingHeader    = 4096
	maxSightingCount     = 1_000_000
	sightingsFlushAge    = 2 * time.Second
	sightingsDeadline    = 5 * time.Second
	sightingsFlushWait   = 6 * time.Second
	sightingsMaxTTL      = 86400
	sightingsMinWait     = time.Second
	sightingsTick        = 250 * time.Millisecond
	sightingsAuthPause   = 60 * time.Second
	sightingsDefaultWait = 60 * time.Second
	sightingsBackoffMin  = time.Second
	sightingsBackoffMax  = 60 * time.Second
	sightingsLogEvery    = time.Minute
)

// agentPaths are reported whatever their content type or status: the files
// crawlers and AI agents read to decide what to fetch.
var agentPaths = []string{"/robots.txt", "/llms.txt", "/llms-full.txt"}

// defaultAgentContentTypes are the responses reported by default: pages.
var defaultAgentContentTypes = []string{"text/html", "application/xhtml+xml"}

// AgentSightingsOptions configures AgentSightings. ClientIP is required.
type AgentSightingsOptions struct {
	// ClientIP resolves the visitor's address, exactly as ProxyOptions.ClientIP
	// does: one IPv4 or IPv6 literal read from a header your own
	// infrastructure sets, falling back to the TCP peer in r.RemoteAddr. Never
	// the left-most X-Forwarded-For value: the visitor writes it. A request
	// whose address does not resolve is not reported, and Lookup answers
	// AgentUnknown for it.
	ClientIP func(*http.Request) string

	// CacheSize is how many answers are remembered, least recently used
	// first out. 0 means DefaultAgentCacheSize; anything above
	// MaxAgentCacheSize is lowered to it.
	CacheSize int

	// ContentTypes are the response media types reported, compared without
	// parameters and ignoring case. Nil means text/html and
	// application/xhtml+xml; an empty, non-nil list reports by path only.
	ContentTypes []string
	// IncludePaths are path prefixes reported whatever their content type.
	// /robots.txt, /llms.txt and /llms-full.txt are always reported.
	IncludePaths []string
	// ExcludePaths are path prefixes never reported, even when another rule
	// matches, the three files above included.
	ExcludePaths []string

	// OnError is called for a report the API refused or could not take, at
	// most once a minute per kind of failure, and for a ClientIP that returned
	// no valid address or panicked. Leave it nil to log one line through
	// slog.Default(). The error never carries a key, an address, a user agent
	// or any header. It may be called from the background flusher and from
	// request goroutines at once; a panic in it is recovered and logged.
	OnError func(error)
}

// AgentSightings reports the requests your server served, so that AI crawlers
// and fetchers that never run a page's JavaScript are seen, and remembers what
// the API answered so Lookup can answer in memory.
//
// Reporting happens in the background, after each response, and never delays
// it. Lookup never waits on the network and never fails: a request it holds no
// answer for is AgentUnknown, which means "serve as usual".
//
// Build one per process, mount its Middleware, and call Shutdown (or Close)
// once your server has stopped serving. In a serverless function, call Flush
// before the invocation ends.
type AgentSightings struct {
	client       *Client
	clientIP     func(*http.Request) string
	onError      func(error)
	contentTypes map[string]struct{}
	includePaths []string
	excludePaths []string
	capacity     int
	// deadline bounds each flush: sightingsDeadline, shortened in tests.
	deadline time.Duration
	// flushWait bounds how long Flush waits for a flush already in flight:
	// sightingsFlushWait, shortened in tests.
	flushWait time.Duration

	// now is the clock, injectable for tests. time.Now carries a monotonic
	// reading, so ages and expiries ignore wall-clock jumps.
	now func() time.Time

	mu sync.Mutex
	// cache is the LRU of answers: front is most recently used.
	cache        *list.List
	cacheIndex   map[string]*list.Element
	rulesVersion string
	// pending holds unreported requests, oldest first; pendingKeys indexes
	// the unsigned ones by key so repeats fold into one count.
	pending      *list.List
	pendingKeys  map[string]*list.Element
	pendingBytes int
	inFlight     int
	pausedUntil  time.Time
	backoff      time.Duration
	closed       bool
	lastLog      map[string]time.Time

	dropped atomic.Uint64

	// flushing holds one token while a flush is in flight, so at most one
	// runs per process and Flush can stop waiting for it.
	flushing chan struct{}

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	// loopCtx governs the flusher's batches. Shutdown cancels it when its own
	// context ends first, abandoning the batch in flight.
	loopCtx    context.Context
	cancelLoop context.CancelFunc
	running    atomic.Bool
	closeOnce  sync.Once
	closeErr   error
}

// sightingEntry is one remembered answer.
type sightingEntry struct {
	key     string
	answer  AgentAnswer
	expires time.Time
}

// pendingSighting is one unreported request: the first sighting's raw fields
// and how many times it was seen.
type pendingSighting struct {
	key       string
	signed    bool
	item      AgentSighting
	firstSeen time.Time
	size      int
}

// NewAgentSightings builds the reporter around a client made with New, and
// starts its background flusher. It reports the client's configuration error,
// if any, immediately.
func NewAgentSightings(client *Client, options AgentSightingsOptions) (*AgentSightings, error) {
	sightings, err := newAgentSightings(client, options)
	if err != nil {
		return nil, err
	}
	sightings.start()
	return sightings, nil
}

// newAgentSightings builds the reporter without starting its flusher.
func newAgentSightings(client *Client, options AgentSightingsOptions) (*AgentSightings, error) {
	if client == nil {
		return nil, errors.New("fingerly: AgentSightings needs a client made with New")
	}
	if client.configErr != nil {
		return nil, client.configErr
	}
	if options.ClientIP == nil {
		return nil, errors.New("fingerly: ClientIP is required")
	}
	capacity := options.CacheSize
	switch {
	case capacity < 0:
		return nil, errors.New("fingerly: CacheSize must not be negative")
	case capacity == 0:
		capacity = DefaultAgentCacheSize
	case capacity > MaxAgentCacheSize:
		capacity = MaxAgentCacheSize
	}
	contentTypes := options.ContentTypes
	if contentTypes == nil {
		contentTypes = defaultAgentContentTypes
	}
	types := make(map[string]struct{}, len(contentTypes))
	for _, value := range contentTypes {
		media := mediaType(value)
		if media == "" {
			return nil, errors.New("fingerly: ContentTypes entries must be media types such as text/html")
		}
		types[media] = struct{}{}
	}
	include, err := pathPrefixes(options.IncludePaths, "IncludePaths")
	if err != nil {
		return nil, err
	}
	exclude, err := pathPrefixes(options.ExcludePaths, "ExcludePaths")
	if err != nil {
		return nil, err
	}
	loopCtx, cancelLoop := context.WithCancel(context.Background())
	return &AgentSightings{
		loopCtx:      loopCtx,
		cancelLoop:   cancelLoop,
		client:       client,
		clientIP:     options.ClientIP,
		onError:      options.OnError,
		contentTypes: types,
		includePaths: include,
		excludePaths: exclude,
		capacity:     capacity,
		deadline:     sightingsDeadline,
		flushWait:    sightingsFlushWait,
		now:          time.Now,
		cache:        list.New(),
		cacheIndex:   make(map[string]*list.Element),
		pending:      list.New(),
		pendingKeys:  make(map[string]*list.Element),
		lastLog:      make(map[string]time.Time),
		flushing:     make(chan struct{}, 1),
		wake:         make(chan struct{}, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}, nil
}

func pathPrefixes(values []string, name string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !strings.HasPrefix(value, "/") {
			return nil, errors.New("fingerly: " + name + " entries must start with /")
		}
		out = append(out, value)
	}
	return out, nil
}

// mediaType is a Content-Type without its parameters, trimmed and lowercased.
func mediaType(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// AgentSightingKey is the cache key every server SDK computes for a request:
// the lowercase hex of the first 16 bytes of SHA-256 over the user agent, the
// canonical address, the Signature-Agent header and whether the request was
// signed, separated by zero bytes. clientIP must already be one literal.
func AgentSightingKey(userAgent, clientIP, signatureAgent string, signed bool) (string, bool) {
	canonical, ok := canonicalIP(clientIP)
	if !ok {
		return "", false
	}
	return sightingKey(userAgent, canonical, signatureAgent, signed), true
}

func sightingKey(userAgent, canonical, signatureAgent string, signed bool) string {
	flag := "0"
	if signed {
		flag = "1"
	}
	hash := sha256.New()
	for i, part := range []string{userAgent, canonical, signatureAgent, flag} {
		if i > 0 {
			hash.Write([]byte{0})
		}
		hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

// canonicalIP parses one IP literal and prints it canonically: an
// IPv4-mapped IPv6 address as IPv4, IPv6 as RFC 5952. A list, a port, a zone
// or surrounding space is refused, as the proxy refuses them.
func canonicalIP(value string) (string, bool) {
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return "", false
	}
	return address.Unmap().String(), true
}

// sightingOf reads what is reported for r, or reports false when r is not
// reportable: a user agent over 4096 bytes or an address that does not
// resolve.
func (s *AgentSightings) sightingOf(r *http.Request) (pendingSighting, bool) {
	userAgent := r.Header.Get("User-Agent")
	if len(userAgent) > maxSightingHeader {
		return pendingSighting{}, false
	}
	resolved, panicked := s.resolveClientIP(r)
	address, ok := canonicalIP(resolved)
	if !ok {
		if panicked {
			s.logLimited("client_ip", errClientIPPanicked)
		} else {
			s.logLimited("client_ip", errNoClientIP)
		}
		return pendingSighting{}, false
	}
	// net/http takes a Host up to its 1 MB header limit, and waiting reports
	// must stay small: a Host over 4096 bytes is left out, as the signature
	// headers are.
	host := r.Host
	if len(host) > maxSightingHeader {
		host = ""
	}
	item := AgentSighting{
		UserAgent:      userAgent,
		ClientIP:       address,
		Host:           host,
		Method:         r.Method,
		Signature:      signatureHeader(r, "Signature"),
		SignatureInput: signatureHeader(r, "Signature-Input"),
		SignatureAgent: signatureHeader(r, "Signature-Agent"),
		Count:          1,
	}
	signed := item.Signature != "" && item.SignatureInput != ""
	return pendingSighting{
		key:    sightingKey(userAgent, address, item.SignatureAgent, signed),
		signed: signed,
		item:   item,
		size: len(item.UserAgent) + len(item.ClientIP) + len(item.Host) + len(item.Method) +
			len(item.Signature) + len(item.SignatureInput) + len(item.SignatureAgent),
	}, true
}

// errClientIPPanicked reports a ClientIP callback that panicked. The request
// it was resolving is served as usual and not reported.
var errClientIPPanicked = errors.New("fingerly: the client IP callback panicked; the request was not reported")

// resolveClientIP calls the ClientIP callback. A panic in it is the host's
// bug, but Report runs after the response was written and Lookup promises
// never to fail, so it must not turn a served page into a broken connection:
// it resolves as no address instead.
func (s *AgentSightings) resolveClientIP(r *http.Request) (address string, panicked bool) {
	defer func() {
		if recover() != nil {
			address, panicked = "", true
		}
	}()
	return s.clientIP(r), false
}

// signatureHeader reads one Web Bot Auth header as the proxy forwards it:
// repeated lines joined as one field list, and left out over 4096 bytes. An
// empty or whitespace-only header, repeated empty lines included, is absent.
func signatureHeader(r *http.Request, name string) string {
	value := strings.Join(r.Header.Values(name), ", ")
	if len(value) > maxSightingHeader || strings.Trim(value, " \t,") == "" {
		return ""
	}
	return value
}

// Lookup returns what the API last answered for a request like r, or
// AgentUnknown when it holds no unexpired answer. It never waits on the
// network and never fails; serve an AgentUnknown request as usual.
//
// A signed request is checked every time, so it is always AgentUnknown here.
func (s *AgentSightings) Lookup(r *http.Request) AgentAnswer {
	sighting, ok := s.sightingOf(r)
	if !ok {
		return AgentUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.cacheIndex[sighting.key]
	if !ok {
		return AgentUnknown
	}
	entry := element.Value.(*sightingEntry)
	if !s.now().Before(entry.expires) {
		s.cache.Remove(element)
		delete(s.cacheIndex, entry.key)
		return AgentUnknown
	}
	s.cache.MoveToFront(element)
	answer := entry.answer
	if answer.AI != nil {
		ai := *answer.AI
		answer.AI = &ai
	}
	return answer
}

// Report records that r was served with a response of the given Content-Type,
// when the reporting rules cover it. Middleware calls it after each response;
// call it yourself from a stack that does not use net/http handlers. It never
// blocks on the network.
func (s *AgentSightings) Report(r *http.Request, contentType string) {
	if !s.inScope(r.URL.Path, contentType) {
		return
	}
	s.record(r)
}

// inScope applies the reporting rules: never by user agent, address or any
// request header, only by the customer's own path and response type.
func (s *AgentSightings) inScope(path, contentType string) bool {
	for _, prefix := range s.excludePaths {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	for _, fixed := range agentPaths {
		if path == fixed {
			return true
		}
	}
	for _, prefix := range s.includePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	_, ok := s.contentTypes[mediaType(contentType)]
	return ok
}

// record adds r to the pending reports.
func (s *AgentSightings) record(r *http.Request) {
	sighting, ok := s.sightingOf(r)
	if !ok {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if !sighting.signed {
		if element, ok := s.pendingKeys[sighting.key]; ok {
			pending := element.Value.(*pendingSighting)
			pending.item.Count = addCounts(pending.item.Count, 1)
			s.mu.Unlock()
			return
		}
	}
	if s.pending.Len()+s.inFlight >= maxPendingSightings {
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	}
	sighting.firstSeen = s.now()
	element := s.pending.PushBack(&sighting)
	if !sighting.signed {
		s.pendingKeys[sighting.key] = element
	}
	s.pendingBytes += sighting.size
	full := s.pending.Len() >= maxSightingsItems || s.pendingBytes >= maxSightingsBytes
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func addCounts(a, b int64) int64 {
	if a+b > maxSightingCount {
		return maxSightingCount
	}
	return a + b
}

// Dropped is how many requests were not reported because 1000 were already
// waiting. Requests already waiting keep counting.
func (s *AgentSightings) Dropped() uint64 { return s.dropped.Load() }

// Flush sends every waiting report now, waiting first, at most 6 seconds, for
// a flush already in flight; when that one is still running it returns without
// sending. It stops early when the API asks the SDK to wait or refuses a
// batch. Call it before a serverless invocation ends; ctx bounds it on top of
// the 5 second deadline each batch has. It never fails: what was not sent is
// kept or dropped as the API's answer says.
func (s *AgentSightings) Flush(ctx context.Context) {
	wait := time.NewTimer(s.flushWait)
	defer wait.Stop()
	select {
	case s.flushing <- struct{}{}:
	case <-wait.C:
		return
	case <-ctx.Done():
		return
	}
	defer func() { <-s.flushing }()
	for ctx.Err() == nil && s.flushBatch(ctx, false) {
	}
}

// Shutdown stops the background flusher and sends what is waiting, giving up
// when ctx is done: a batch in flight is then abandoned and what still waits
// is not sent. Like Flush, it waits at most 6 seconds for the flusher's batch
// in flight, and sends nothing when that one is still running, so a call from
// OnError returns too. Pass it the context you give http.Server.Shutdown,
// after the server has stopped. Requests served afterwards are not reported.
//
// It returns ctx's error when it gave up, and nil otherwise; what the API
// refused is reported to OnError, as always. It is safe to call more than
// once and from several goroutines: later calls wait for the first and return
// its result.
func (s *AgentSightings) Shutdown(ctx context.Context) error {
	s.closeOnce.Do(func() { s.closeErr = s.shutdown(ctx) })
	return s.closeErr
}

func (s *AgentSightings) shutdown(ctx context.Context) error {
	close(s.stop)
	// Returning abandons the flusher's batch in flight, if any.
	defer s.cancelLoop()
	if s.started() {
		// The flusher starts no batch now, and the one in flight has its own
		// deadline. Wait for it as Flush waits for a flush in flight, at most
		// 6 seconds and never past ctx: the flusher may be inside OnError,
		// which may block, or call Shutdown or Close itself.
		wait := time.NewTimer(s.flushWait)
		defer wait.Stop()
		select {
		case <-s.done:
		case <-wait.C:
			s.markClosed()
			return ctx.Err()
		case <-ctx.Done():
			s.markClosed()
			return ctx.Err()
		}
	}
	s.markClosed()
	s.Flush(ctx)
	return ctx.Err()
}

// markClosed stops recording requests.
func (s *AgentSightings) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Close is Shutdown without a deadline of its own: it stops the background
// flusher and sends what is waiting, each batch within its 5 second deadline,
// stopping at the first batch the API does not take. It always returns nil.
func (s *AgentSightings) Close() error {
	_ = s.Shutdown(context.Background())
	return nil
}

// started reports whether start launched the flusher goroutine.
func (s *AgentSightings) started() bool { return s.running.Load() }

// start launches the background flusher.
func (s *AgentSightings) start() {
	s.running.Store(true)
	go s.loop()
}

// loop flushes when a batch is due: 200 waiting, 64 KiB waiting, or the oldest
// waiting for 2 seconds. A goroutine never keeps a Go program alive, so
// nothing more is needed for it to end with the process.
func (s *AgentSightings) loop() {
	ticker := time.NewTicker(sightingsTick)
	defer ticker.Stop()
	s.loopWith(ticker.C)
}

// loopWith is loop on the given ticks, which tests supply.
func (s *AgentSightings) loopWith(ticks <-chan time.Time) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-ticks:
		case <-s.wake:
		}
		// select picks at random among ready cases, so a tick or a wake
		// ready alongside stop must not start another batch: Shutdown is
		// waiting, and sends what is left itself.
		if s.stopping() {
			return
		}
		s.flushDue()
	}
}

// stopping reports whether Shutdown has begun.
func (s *AgentSightings) stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// flushDue sends batches while one is due, unless a flush is in flight or
// Shutdown has begun.
func (s *AgentSightings) flushDue() {
	select {
	case s.flushing <- struct{}{}:
	default:
		return
	}
	defer func() { <-s.flushing }()
	for !s.stopping() && s.flushBatch(s.loopCtx, true) {
	}
}

// due reports whether a batch should go now. The caller holds mu.
func (s *AgentSightings) due(now time.Time) bool {
	front := s.pending.Front()
	if front == nil {
		return false
	}
	return s.pending.Len() >= maxSightingsItems || s.pendingBytes >= maxSightingsBytes ||
		now.Sub(front.Value.(*pendingSighting).firstSeen) >= sightingsFlushAge
}

// flushBatch sends one batch and applies the API's answer. It reports whether
// another batch may follow straight away. The caller holds the flushing token.
func (s *AgentSightings) flushBatch(ctx context.Context, onlyWhenDue bool) bool {
	s.mu.Lock()
	now := s.now()
	if now.Before(s.pausedUntil) || s.pending.Len() == 0 || (onlyWhenDue && !s.due(now)) {
		s.mu.Unlock()
		return false
	}
	batch := s.takeBatch()
	s.mu.Unlock()

	items := make([]AgentSighting, len(batch))
	for i, pending := range batch {
		items[i] = pending.item
	}
	var exchange sightingsExchange
	callCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, exchange.trace()), s.deadline)
	answer, err := s.client.Agents.Sightings(callCtx, items)
	cancel()
	more, failure := s.settle(batch, answer, err, &exchange)
	s.emit(failure)
	return more
}

// takeBatch moves the oldest waiting reports in flight: up to 200, and no
// more than 64 KiB of fields unless the first alone is larger. The caller
// holds mu.
func (s *AgentSightings) takeBatch() []*pendingSighting {
	var batch []*pendingSighting
	bytes := 0
	for element := s.pending.Front(); element != nil && len(batch) < maxSightingsItems; {
		pending := element.Value.(*pendingSighting)
		if len(batch) > 0 && bytes+pending.size > maxSightingsBytes {
			break
		}
		next := element.Next()
		s.pending.Remove(element)
		if !pending.signed {
			delete(s.pendingKeys, pending.key)
		}
		s.pendingBytes -= pending.size
		bytes += pending.size
		batch = append(batch, pending)
		element = next
	}
	s.inFlight += len(batch)
	return batch
}

// sightingsExchange records how far a report got, so a failure can be told
// apart: before the request was fully written it is safe to retry, after it
// the API may have counted the batch.
type sightingsExchange struct {
	wrote     atomic.Bool
	responded atomic.Bool
}

func (e *sightingsExchange) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				e.wrote.Store(true)
			}
		},
		GotFirstResponseByte: func() { e.responded.Store(true) },
	}
}

// Why a batch was dropped, for OnError.
var (
	errSightingsMismatch = errors.New("fingerly: the sightings answer did not match the batch")
	errSightingsTimedOut = errors.New("fingerly: the sightings report timed out after it was sent; dropped so it is not counted twice")
)

// settle applies the API's answer to a batch, following the agent sightings
// contract's failure table. It reports whether another batch may follow, and
// the failure to report, if one is due.
func (s *AgentSightings) settle(
	batch []*pendingSighting, answer *AgentSightingsAnswer, err error, exchange *sightingsExchange,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight -= len(batch)
	now := s.now()

	if err == nil {
		if len(answer.Items) != len(batch) {
			return false, s.limited(now, "mismatch", errSightingsMismatch)
		}
		s.backoff = 0
		s.store(now, batch, answer)
		return true, nil
	}

	var apiErr *APIError
	var connErr *ConnectionError
	switch {
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusTooManyRequests:
			s.keep(batch)
			s.pausedUntil = now.Add(retryWait(now, apiErr))
			return false, nil
		case http.StatusServiceUnavailable:
			s.keep(batch)
			s.backOff(now)
			return false, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			s.pausedUntil = now.Add(sightingsAuthPause)
			return false, s.limited(now, "auth", err)
		case http.StatusUnprocessableEntity:
			return false, s.limited(now, "invalid", err)
		default:
			return false, s.limited(now, "status", err)
		}
	case errors.As(err, &connErr) && !exchange.responded.Load() && !exchange.wrote.Load():
		// The request was never fully written, so the API cannot have counted
		// the batch: a refused connection, a failed handshake, a connection
		// closed or a timeout before the request went out.
		s.keep(batch)
		s.backOff(now)
		return false, nil
	case errors.As(err, &connErr) && connErr.Timeout:
		return false, s.limited(now, "timeout", errSightingsTimedOut)
	default:
		// A connection closed or reset once the request was written, an
		// answer that broke off, or one that could not be decoded: the API
		// may have counted the batch.
		return false, s.limited(now, "answer", err)
	}
}

// retryWait is how long a 429 asks the SDK to wait: Retry-After, or without
// one, until the next UTC day for the daily allowance and a minute otherwise.
// A Retry-After of 0 or in the past still waits a second; a negative or
// unreadable one reaches here as none. One over a day waits a day: the daily
// allowance renews within one, and a larger value would overflow a Duration.
func retryWait(now time.Time, apiErr *APIError) time.Duration {
	if apiErr.RetryAfter != nil {
		seconds := min(*apiErr.RetryAfter, sightingsMaxTTL)
		return max(time.Duration(seconds)*time.Second, sightingsMinWait)
	}
	if apiErr.RateLimitScope == RateLimitScopeDaily {
		utc := now.UTC()
		midnight := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
		return midnight.Sub(utc)
	}
	return sightingsDefaultWait
}

// backOff pauses flushing for the next step of 1, 2, 4 ... 60 seconds. The
// caller holds mu.
func (s *AgentSightings) backOff(now time.Time) {
	switch {
	case s.backoff == 0:
		s.backoff = sightingsBackoffMin
	default:
		s.backoff *= 2
	}
	if s.backoff > sightingsBackoffMax {
		s.backoff = sightingsBackoffMax
	}
	s.pausedUntil = now.Add(s.backoff)
}

// keep puts a batch back as the oldest waiting reports. A key seen again
// while the batch was in flight folds into the kept report. The caller holds
// mu.
func (s *AgentSightings) keep(batch []*pendingSighting) {
	for i := len(batch) - 1; i >= 0; i-- {
		pending := batch[i]
		if !pending.signed {
			if element, ok := s.pendingKeys[pending.key]; ok {
				newer := element.Value.(*pendingSighting)
				pending.item.Count = addCounts(pending.item.Count, newer.item.Count)
				s.pending.Remove(element)
				s.pendingBytes -= newer.size
			}
		}
		element := s.pending.PushFront(pending)
		if !pending.signed {
			s.pendingKeys[pending.key] = element
		}
		s.pendingBytes += pending.size
	}
}

// store remembers every answer with a ttl, for at most a day, after dropping
// every remembered answer when the rules changed. The caller holds mu.
func (s *AgentSightings) store(now time.Time, batch []*pendingSighting, answer *AgentSightingsAnswer) {
	if answer.RulesVersion != s.rulesVersion {
		s.cache.Init()
		clear(s.cacheIndex)
		s.rulesVersion = answer.RulesVersion
	}
	for i, item := range answer.Items {
		if item.TTL <= 0 {
			continue
		}
		entry := &sightingEntry{
			key:     batch[i].key,
			answer:  item,
			expires: now.Add(time.Duration(min(item.TTL, sightingsMaxTTL)) * time.Second),
		}
		if element, ok := s.cacheIndex[entry.key]; ok {
			element.Value = entry
			s.cache.MoveToFront(element)
			continue
		}
		s.cacheIndex[entry.key] = s.cache.PushFront(entry)
		for s.cache.Len() > s.capacity {
			oldest := s.cache.Back()
			s.cache.Remove(oldest)
			delete(s.cacheIndex, oldest.Value.(*sightingEntry).key)
		}
	}
}

// logLimited reports err at most once a minute for its kind.
func (s *AgentSightings) logLimited(kind string, err error) {
	s.mu.Lock()
	err = s.limited(s.now(), kind, err)
	s.mu.Unlock()
	s.emit(err)
}

// limited returns err when its kind was not reported in the last minute, and
// nil otherwise. The caller holds mu, and emits the result after releasing it.
func (s *AgentSightings) limited(now time.Time, kind string, err error) error {
	if last, ok := s.lastLog[kind]; ok && now.Sub(last) < sightingsLogEvery {
		return nil
	}
	s.lastLog[kind] = now
	return err
}

// emit hands a failure to OnError, or logs one line about it.
func (s *AgentSightings) emit(err error) {
	if err == nil {
		return
	}
	if s.onError != nil {
		if s.callOnError(err) {
			return
		}
		slog.Default().Warn("fingerly agent sightings: OnError panicked", "error", err.Error())
		return
	}
	slog.Default().Warn("fingerly agent sightings: report failed", "error", err.Error())
}

// callOnError calls OnError and reports whether it returned. It runs on the
// background flusher too, where a panic would end the whole process, so a
// panic is recovered and the failure logged instead.
func (s *AgentSightings) callOnError(err error) (returned bool) {
	defer func() {
		if recover() != nil {
			returned = false
		}
	}()
	s.onError(err)
	return true
}

// Middleware reports every response its rules cover after next has written
// it, and never delays it. It works with any router built on net/http:
//
//	mux.Handle("/", sightings.Middleware(site))
func (s *AgentSightings) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &sightingsWriter{ResponseWriter: w, http1: r.ProtoMajor < 2}
		next.ServeHTTP(writer, r)
		s.Report(r, writer.contentType())
	})
}

// sniffLen is how much of a body net/http reads to sniff its type.
const sniffLen = 512

// sightingsWriter notes the Content-Type the response goes out with, sniffed
// from the body as net/http sniffs it when the handler set none. It unwraps
// for http.ResponseController and passes Flush, Hijack, ReadFrom, Push and
// CloseNotify through, so routers and frameworks that look for those
// interfaces, and wrap the writer again, keep finding them.
type sightingsWriter struct {
	http.ResponseWriter
	http1 bool
	// wroteHeader is set once the header is fixed: a final status, the first
	// byte of body, or a flush.
	wroteHeader bool
	// sent is the Content-Type the header carried.
	sent string
	// sniff is set when net/http picks the type from the body instead: no
	// Content-Type at all, no Content-Encoding (or Transfer-Encoding over
	// HTTP/1), and a status with a body.
	sniff bool
	// head is what net/http sniffs: the first 512 bytes of body written
	// before the first flush.
	head    []byte
	flushed bool
}

// fixHeader notes what the header goes out with, when net/http fixes it.
func (w *sightingsWriter) fixHeader(status int) {
	w.wroteHeader = true
	header := w.Header()
	_, typed := header["Content-Type"]
	w.sent = header.Get("Content-Type")
	w.sniff = !typed && header.Get("Content-Encoding") == "" &&
		!(w.http1 && header.Get("Transfer-Encoding") != "") &&
		status != http.StatusNoContent && status != http.StatusNotModified &&
		(status < 100 || status > 199)
}

func (w *sightingsWriter) WriteHeader(status int) {
	// An informational status other than 101 leaves the header open.
	if !w.wroteHeader && (status < 100 || status > 199 || status == http.StatusSwitchingProtocols) {
		w.fixHeader(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *sightingsWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.fixHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if w.sniff && !w.flushed && len(w.head) < sniffLen && n > 0 && n <= len(p) {
		w.head = append(w.head, p[:min(n, sniffLen-len(w.head))]...)
	}
	return n, err
}

// contentType is the type the response went out with.
func (w *sightingsWriter) contentType() string {
	if !w.wroteHeader {
		// Nothing written: net/http sends an empty 200 with these headers.
		return w.Header().Get("Content-Type")
	}
	if w.sniff && len(w.head) > 0 {
		return http.DetectContentType(w.head)
	}
	return w.sent
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *sightingsWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError passes through to the underlying writer, for streamed pages, and
// returns its error, as http.ResponseController.Flush does.
func (w *sightingsWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		if !w.wroteHeader {
			w.fixHeader(http.StatusOK)
		}
		w.flushed = true
	}
	return err
}

// Flush passes through to the underlying writer, for streamed pages.
func (w *sightingsWriter) Flush() { _ = w.FlushError() }

// Hijack passes through to the underlying writer, for upgraded connections.
func (w *sightingsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// ReadFrom passes through to the underlying writer, which sends a file with
// sendfile where it can. As net/http's own ReadFrom does, the body's first
// 512 bytes go through Write first, so the type sniffed from them is noted.
func (w *sightingsWriter) ReadFrom(src io.Reader) (int64, error) {
	var n int64
	if !w.wroteHeader || (w.sniff && !w.flushed && len(w.head) < sniffLen) {
		limit := int64(sniffLen - len(w.head))
		copied, err := io.Copy(writeOnly{w}, io.LimitReader(src, limit))
		n += copied
		if err != nil || copied < limit {
			return n, err
		}
	}
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		copied, err := from.ReadFrom(src)
		return n + copied, err
	}
	copied, err := io.Copy(writeOnly{w.ResponseWriter}, src)
	return n + copied, err
}

// writeOnly hides every method of a writer but Write, so io.Copy cannot call
// ReadFrom back.
type writeOnly struct{ io.Writer }

// Push passes through to the underlying writer, for HTTP/2 server push, and
// is http.ErrNotSupported where it has none.
func (w *sightingsWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// CloseNotify passes through to the underlying writer, for frameworks that
// still use http.CloseNotifier, some without checking that the writer has it.
// Where it has none, the channel never fires.
func (w *sightingsWriter) CloseNotify() <-chan bool {
	//lint:ignore SA1019 frameworks still call it, and assert that it is there.
	if notifier, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}
	return make(chan bool)
}
