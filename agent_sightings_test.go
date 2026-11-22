package fingerly

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock for the cache and the flush timings.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sightingsAPI is a stand-in for POST /api/v1/agents/sightings that records
// every batch and answers with respond, which defaults to every item human
// with a 60 second ttl.
type sightingsAPI struct {
	mu      sync.Mutex
	batches [][]AgentSighting
	headers []http.Header
	paths   []string
	respond func(w http.ResponseWriter, batch []AgentSighting)
	server  *httptest.Server
}

func newSightingsAPI(t *testing.T) *sightingsAPI {
	t.Helper()
	api := &sightingsAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []AgentSighting `json:"items"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		api.mu.Lock()
		api.batches = append(api.batches, body.Items)
		api.headers = append(api.headers, r.Header.Clone())
		api.paths = append(api.paths, r.URL.Path)
		respond := api.respond
		api.mu.Unlock()
		if respond == nil {
			respond = answerAll("v1", 60)
		}
		respond(w, body.Items)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *sightingsAPI) setRespond(respond func(http.ResponseWriter, []AgentSighting)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.respond = respond
}

func (a *sightingsAPI) sent() [][]AgentSighting {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]AgentSighting(nil), a.batches...)
}

// answerAll answers every item as a person's browser.
func answerAll(rules string, ttl int) func(http.ResponseWriter, []AgentSighting) {
	return func(w http.ResponseWriter, batch []AgentSighting) {
		items := make([]AgentAnswer, len(batch))
		for i := range batch {
			items[i] = AgentAnswer{TTL: ttl, VisitorKind: VisitorKindHuman}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AgentSightingsAnswer{RulesVersion: rules, Items: items})
	}
}

// answerStatus answers every call with an error status and extra headers.
func answerStatus(status int, code string, headers map[string]string) func(http.ResponseWriter, []AgentSighting) {
	return func(w http.ResponseWriter, _ []AgentSighting) {
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(errorBody(code, status))
	}
}

// testIP is the ClientIP callback the tests use: a trusted header, else the
// TCP peer.
func testIP(r *http.Request) string {
	if ip := r.Header.Get("X-Test-IP"); ip != "" {
		return ip
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

// sightingsHarness is an AgentSightings without its background flusher, on a
// fake clock, against a sightingsAPI.
type sightingsHarness struct {
	*AgentSightings
	api    *sightingsAPI
	clock  *fakeClock
	errors *errorLog
}

type errorLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errorLog) add(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *errorLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errs)
}

func newHarness(t *testing.T, apply func(*AgentSightingsOptions)) *sightingsHarness {
	t.Helper()
	api := newSightingsAPI(t)
	log := &errorLog{}
	options := AgentSightingsOptions{ClientIP: testIP, OnError: log.add}
	if apply != nil {
		apply(&options)
	}
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := newAgentSightings(client, options)
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	sightings.now = clock.Now
	return &sightingsHarness{AgentSightings: sightings, api: api, clock: clock, errors: log}
}

// served builds a served request from a user agent and an address.
func served(userAgent, ip string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://shop.example/products/1?ref=x", nil)
	r.Header.Del("User-Agent")
	if userAgent != "" {
		r.Header.Set("User-Agent", userAgent)
	}
	r.Header.Set("X-Test-IP", ip)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

func (h *sightingsHarness) pendingItems() []AgentSighting {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []AgentSighting
	for element := h.pending.Front(); element != nil; element = element.Next() {
		out = append(out, element.Value.(*pendingSighting).item)
	}
	return out
}

func TestAgentSightingKeyReproducesEveryVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(contractDir, "agent-sightings-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name           string `json:"name"`
			UserAgent      string `json:"user_agent"`
			ClientIP       string `json:"client_ip"`
			CanonicalIP    string `json:"canonical_ip"`
			SignatureAgent string `json:"signature_agent"`
			Signed         bool   `json:"signed"`
			Key            string `json:"key"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	h := newHarness(t, nil)
	for _, vector := range file.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			canonical, ok := canonicalIP(vector.ClientIP)
			if !ok || canonical != vector.CanonicalIP {
				t.Fatalf("canonical %q, want %q", canonical, vector.CanonicalIP)
			}
			key, ok := AgentSightingKey(vector.UserAgent, vector.ClientIP, vector.SignatureAgent, vector.Signed)
			if !ok || key != vector.Key {
				t.Fatalf("key %s, want %s", key, vector.Key)
			}
			// The same key from a request carrying those headers.
			headers := map[string]string{}
			if vector.SignatureAgent != "" {
				headers["Signature-Agent"] = vector.SignatureAgent
			}
			if vector.Signed {
				headers["Signature"] = "sig1=:AAAA:"
				headers["Signature-Input"] = `sig1=("@authority");tag="web-bot-auth"`
			}
			sighting, ok := h.sightingOf(served(vector.UserAgent, vector.ClientIP, headers))
			if !ok || sighting.key != vector.Key || sighting.signed != vector.Signed {
				t.Fatalf("request key %s signed %v, want %s", sighting.key, sighting.signed, vector.Key)
			}
			if sighting.item.ClientIP != vector.CanonicalIP {
				t.Fatalf("sent address %q, want %q", sighting.item.ClientIP, vector.CanonicalIP)
			}
		})
	}
}

func TestAgentSightingKeyRefusesWhatIsNotOneAddress(t *testing.T) {
	for _, value := range []string{"", "10.0.0.1, 10.0.0.2", "10.0.0.1:80", " 10.0.0.1", "fe80::1%eth0"} {
		if _, ok := AgentSightingKey("ua", value, "", false); ok {
			t.Errorf("%q accepted", value)
		}
	}
}

func TestLookupIsUnknownUntilTheAPIAnswers(t *testing.T) {
	h := newHarness(t, nil)
	r := served("GPTBot/1.4", "20.171.206.9", nil)
	if got := h.Lookup(r); got != AgentUnknown || got.Known() {
		t.Fatalf("got %+v", got)
	}
	h.Report(withPath(r, "/"), "text/html")
	if got := h.Lookup(r); got != AgentUnknown {
		t.Fatal("answered before any flush")
	}
	h.api.setRespond(respondRawFixture(http.StatusOK, json.RawMessage(
		`{"rules_version":"r1","items":[{"ttl":900,"visitor_kind":"ai_bot","agent":"GPTBot",`+
			`"ai":{"name":"GPTBot","provider":"OpenAI","purpose":"training","runtime":"provider_cloud",`+
			`"declared":true,"verification":{"method":"ip_list","result":"pass"}}}]}`)).toRespond())
	h.Flush(context.Background())
	got := h.Lookup(r)
	if !got.Known() || got.VisitorKind != VisitorKindAIBot || got.AI == nil ||
		got.AI.Verification.Result != AIVerificationPass || got.AI.Purpose != AIPurposeTraining {
		t.Fatalf("got %+v", got)
	}
	// A different address is a different request.
	if h.Lookup(served("GPTBot/1.4", "20.171.206.10", nil)) != AgentUnknown {
		t.Fatal("answered for another address")
	}
	// The answer handed out is a copy.
	got.AI.Name = "changed"
	if h.Lookup(r).AI.Name != "GPTBot" {
		t.Fatal("the cache shares its answer")
	}
}

// toRespond adapts a handler to the sightingsAPI responder signature.
type handlerFunc http.HandlerFunc

func (f handlerFunc) toRespond() func(http.ResponseWriter, []AgentSighting) {
	return func(w http.ResponseWriter, _ []AgentSighting) { f(w, nil) }
}

func respondRawFixture(status int, body json.RawMessage) handlerFunc {
	return handlerFunc(respondRaw(status, body))
}

func withPath(r *http.Request, path string) *http.Request {
	r.URL.Path = path
	return r
}

func TestAnswersExpireAfterTheirTTLAndZeroIsNeverCached(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		items := make([]AgentAnswer, len(batch))
		for i, item := range batch {
			ttl, _ := strconv.Atoi(strings.TrimPrefix(item.UserAgent, "ttl"))
			items[i] = AgentAnswer{TTL: ttl, VisitorKind: VisitorKindHuman}
		}
		_ = json.NewEncoder(w).Encode(AgentSightingsAnswer{RulesVersion: "r", Items: items})
	})
	short := served("ttl10", "198.51.100.1", nil)
	never := served("ttl0", "198.51.100.1", nil)
	h.Report(short, "text/html")
	h.Report(never, "text/html")
	h.Flush(context.Background())
	if !h.Lookup(short).Known() {
		t.Fatal("ttl 10 not cached")
	}
	if h.Lookup(never).Known() {
		t.Fatal("ttl 0 cached")
	}
	h.clock.Advance(9 * time.Second)
	if !h.Lookup(short).Known() {
		t.Fatal("expired early")
	}
	h.clock.Advance(time.Second)
	if h.Lookup(short).Known() {
		t.Fatal("served past its ttl")
	}
	if h.cache.Len() != 0 {
		t.Fatalf("expired entry kept: %d", h.cache.Len())
	}
}

func TestANewRulesVersionDropsEveryAnswer(t *testing.T) {
	h := newHarness(t, nil)
	first := served("first", "198.51.100.1", nil)
	second := served("second", "198.51.100.1", nil)
	h.Report(first, "text/html")
	h.Flush(context.Background())
	if !h.Lookup(first).Known() {
		t.Fatal("not cached")
	}
	h.api.setRespond(answerAll("v2", 60))
	h.Report(second, "text/html")
	h.Flush(context.Background())
	if h.Lookup(first).Known() {
		t.Fatal("answer from the old rules kept")
	}
	if !h.Lookup(second).Known() {
		t.Fatal("answer under the new rules not stored")
	}
}

func TestTheCacheIsBoundedAndEvictsTheLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t, func(o *AgentSightingsOptions) { o.CacheSize = 2 })
	a, b, c := served("a", "198.51.100.1", nil), served("b", "198.51.100.1", nil), served("c", "198.51.100.1", nil)
	h.Report(a, "text/html")
	h.Report(b, "text/html")
	h.Flush(context.Background())
	h.Lookup(a) // a is now the most recently used.
	h.Report(c, "text/html")
	h.Flush(context.Background())
	if h.cache.Len() != 2 {
		t.Fatalf("cache holds %d", h.cache.Len())
	}
	if !h.Lookup(a).Known() || !h.Lookup(c).Known() || h.Lookup(b).Known() {
		t.Fatal("evicted the wrong entry")
	}
}

func TestCacheSizeBounds(t *testing.T) {
	client := New(testSecretKey)
	for size, want := range map[int]int{0: DefaultAgentCacheSize, 10: 10, MaxAgentCacheSize + 1: MaxAgentCacheSize} {
		sightings, err := newAgentSightings(client, AgentSightingsOptions{ClientIP: testIP, CacheSize: size})
		if err != nil || sightings.capacity != want {
			t.Errorf("CacheSize %d: capacity %v, %v", size, sightings, err)
		}
	}
	if _, err := newAgentSightings(client, AgentSightingsOptions{ClientIP: testIP, CacheSize: -1}); err == nil {
		t.Error("a negative CacheSize was accepted")
	}
	if _, err := newAgentSightings(client, AgentSightingsOptions{}); err == nil {
		t.Error("a missing ClientIP was accepted")
	}
	if _, err := NewAgentSightings(New("fly_pk_development_x"), AgentSightingsOptions{ClientIP: testIP}); err == nil {
		t.Error("a client with a bad key was accepted")
	}
	if _, err := newAgentSightings(client, AgentSightingsOptions{ClientIP: testIP, IncludePaths: []string{"api"}}); err == nil {
		t.Error("a relative include path was accepted")
	}
}

func TestRepeatsFoldIntoOneCountIncludingCacheHits(t *testing.T) {
	h := newHarness(t, nil)
	r := served("GPTBot/1.4", "20.171.206.9", nil)
	for range 3 {
		h.Report(r, "text/html")
	}
	h.Flush(context.Background())
	batches := h.api.sent()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].Count != 3 {
		t.Fatalf("sent %+v", batches)
	}
	if !h.Lookup(r).Known() {
		t.Fatal("not cached")
	}
	// Answered from the cache, and still counted.
	h.Report(r, "text/html")
	h.Report(r, "text/html")
	h.Flush(context.Background())
	batches = h.api.sent()
	if len(batches) != 2 || len(batches[1]) != 1 || batches[1][0].Count != 2 {
		t.Fatalf("cache hits not counted: %+v", batches)
	}
}

func TestCountsSaturateAtTheAPIMaximum(t *testing.T) {
	if addCounts(maxSightingCount, 1) != maxSightingCount || addCounts(2, 3) != 5 {
		t.Fatal("addCounts")
	}
}

func TestSignedRequestsAreNeverFolded(t *testing.T) {
	h := newHarness(t, nil)
	signed := map[string]string{
		"Signature":       "sig1=:AAAA:",
		"Signature-Input": `sig1=("@authority");tag="web-bot-auth"`,
		"Signature-Agent": `"https://chatgpt.com"`,
	}
	h.api.setRespond(answerAll("r", 0))
	r := served("Mozilla/5.0", "34.31.25.14", signed)
	h.Report(r, "text/html")
	h.Report(r, "text/html")
	h.Flush(context.Background())
	batches := h.api.sent()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("sent %+v", batches)
	}
	for _, item := range batches[0] {
		if item.Count != 1 || item.Signature == "" || item.SignatureInput == "" || item.SignatureAgent == "" {
			t.Fatalf("item %+v", item)
		}
	}
	if h.Lookup(r).Known() {
		t.Fatal("a signed request was answered from memory")
	}
}

func TestOnlyRawFieldsAreSent(t *testing.T) {
	h := newHarness(t, nil)
	r := served("GPTBot/1.4", "::ffff:20.171.206.9", map[string]string{
		"Cookie":        "session=secret",
		"Authorization": "Bearer secret",
	})
	r.Host = "shop.example"
	h.Report(withPath(r, "/products/1"), "text/html; charset=utf-8")
	h.Flush(context.Background())
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if h.api.paths[0] != "/api/v1/agents/sightings" || h.api.headers[0].Get("X-API-Key") != testSecretKey {
		t.Fatalf("sent to %s", h.api.paths[0])
	}
	want := AgentSighting{UserAgent: "GPTBot/1.4", ClientIP: "20.171.206.9", Host: "shop.example", Method: "GET", Count: 1}
	if got := h.api.batches[0][0]; got != want {
		t.Fatalf("sent %+v", got)
	}
}

func TestOversizedHeadersAreLeftOut(t *testing.T) {
	h := newHarness(t, nil)
	long := strings.Repeat("a", maxSightingHeader+1)
	tooLong := served(long, "198.51.100.1", nil)
	h.Report(tooLong, "text/html")
	if len(h.pendingItems()) != 0 || h.Lookup(tooLong) != AgentUnknown {
		t.Fatal("a user agent over 4096 bytes was reported")
	}
	edge := served(strings.Repeat("a", maxSightingHeader), "198.51.100.1", nil)
	h.Report(edge, "text/html")
	if len(h.pendingItems()) != 1 {
		t.Fatal("a 4096 byte user agent was not reported")
	}

	signedLong := served("ua", "198.51.100.1", map[string]string{
		"Signature":       long,
		"Signature-Input": "sig1=()",
		"Signature-Agent": long,
	})
	sighting, ok := h.sightingOf(signedLong)
	if !ok || sighting.item.Signature != "" || sighting.item.SignatureAgent != "" ||
		sighting.item.SignatureInput != "sig1=()" || sighting.signed {
		t.Fatalf("sighting %+v", sighting.item)
	}
	if want, _ := AgentSightingKey("ua", "198.51.100.1", "", false); sighting.key != want {
		t.Fatal("the key counts a header that was left out")
	}

	// net/http takes a Host up to its 1 MB header limit: a thousand waiting
	// would hold a gigabyte.
	hugeHost := served("ua", "198.51.100.1", nil)
	hugeHost.Host = strings.Repeat("a", 1<<20)
	sighting, ok = h.sightingOf(hugeHost)
	if !ok || sighting.item.Host != "" || sighting.size > 64 {
		t.Fatalf("a 1 MB Host was kept: %d bytes", sighting.size)
	}
}

func TestBlankSignatureHeadersAreAbsent(t *testing.T) {
	h := newHarness(t, nil)
	blank := served("ua", "198.51.100.1", map[string]string{
		"Signature":       "   ",
		"Signature-Input": "",
		"Signature-Agent": " \t ",
	})
	blank.Header.Add("Signature-Input", "")
	sighting, ok := h.sightingOf(blank)
	if !ok || sighting.item.Signature != "" || sighting.item.SignatureInput != "" ||
		sighting.item.SignatureAgent != "" || sighting.signed {
		t.Fatalf("sighting %+v", sighting.item)
	}
	if want, _ := AgentSightingKey("ua", "198.51.100.1", "", false); sighting.key != want {
		t.Fatal("the key counts a blank header")
	}
	h.Report(blank, "text/html")
	h.Report(blank, "text/html")
	if items := h.pendingItems(); len(items) != 1 || items[0].Count != 2 {
		t.Fatalf("pending %+v", items)
	}
}

func TestAnUnresolvedAddressIsNotReported(t *testing.T) {
	h := newHarness(t, nil)
	r := served("ua", "10.0.0.1, 10.0.0.2", nil)
	h.Report(r, "text/html")
	h.Report(r, "text/html")
	if len(h.pendingItems()) != 0 || h.Lookup(r) != AgentUnknown {
		t.Fatal("reported a forwarded list")
	}
	if h.errors.count() != 1 || !errors.Is(h.errors.errs[0], errNoClientIP) {
		t.Fatalf("logged %v", h.errors.errs)
	}
}

func TestTheBufferIsBoundedAndCountsDrops(t *testing.T) {
	h := newHarness(t, nil)
	for i := range maxPendingSightings {
		h.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	h.Report(served("one too many", "198.51.100.1", nil), "text/html")
	if h.Dropped() != 1 || len(h.pendingItems()) != maxPendingSightings {
		t.Fatalf("dropped %d, pending %d", h.Dropped(), len(h.pendingItems()))
	}
	// A key already waiting keeps counting.
	h.Report(served("ua0", "198.51.100.1", nil), "text/html")
	if h.Dropped() != 1 || h.pendingItems()[0].Count != 2 {
		t.Fatal("an existing key stopped counting")
	}
}

func TestABatchIsDueAt200Items(t *testing.T) {
	h := newHarness(t, nil)
	for i := range maxSightingsItems - 1 {
		h.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	h.flushDue()
	if len(h.api.sent()) != 0 {
		t.Fatal("flushed under every threshold")
	}
	h.Report(served("last", "198.51.100.1", nil), "text/html")
	select {
	case <-h.wake:
	default:
		t.Fatal("reaching 200 did not wake the flusher")
	}
	h.flushDue()
	if batches := h.api.sent(); len(batches) != 1 || len(batches[0]) != maxSightingsItems {
		t.Fatalf("sent %d batches", len(batches))
	}
}

func TestABatchIsDueAt64KiBAndStaysUnderIt(t *testing.T) {
	h := newHarness(t, nil)
	ua := strings.Repeat("x", 4000)
	for i := range 16 {
		h.Report(served(ua+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	h.flushDue()
	if len(h.api.sent()) != 0 {
		t.Fatal("flushed under 64 KiB")
	}
	h.Report(served(ua+"last", "198.51.100.1", nil), "text/html")
	h.flushDue()
	// The batch stops before 64 KiB; the rest is not due yet.
	if batches := h.api.sent(); len(batches) != 1 || len(batches[0]) != 16 {
		t.Fatalf("sent %d batches", len(batches))
	}
	h.clock.Advance(sightingsFlushAge)
	h.flushDue()
	batches := h.api.sent()
	if len(batches) != 2 {
		t.Fatalf("sent %d batches", len(batches))
	}
	total := 0
	for _, batch := range batches {
		size := 0
		for _, item := range batch {
			size += len(item.UserAgent) + len(item.ClientIP) + len(item.Host) + len(item.Method)
		}
		if size > maxSightingsBytes {
			t.Fatalf("a batch of %d bytes", size)
		}
		total += len(batch)
	}
	if total != 17 {
		t.Fatalf("sent %d items", total)
	}
}

func TestABatchIsDueTwoSecondsAfterItsOldestItem(t *testing.T) {
	h := newHarness(t, nil)
	h.Report(served("ua", "198.51.100.1", nil), "text/html")
	h.clock.Advance(1999 * time.Millisecond)
	h.flushDue()
	if len(h.api.sent()) != 0 {
		t.Fatal("flushed early")
	}
	h.clock.Advance(time.Millisecond)
	h.flushDue()
	if len(h.api.sent()) != 1 {
		t.Fatal("not flushed after 2 seconds")
	}
}

func TestTheBackgroundFlusherSendsOnItsOwnAndCloseFlushes(t *testing.T) {
	api := newSightingsAPI(t)
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := NewAgentSightings(client, AgentSightingsOptions{ClientIP: testIP})
	if err != nil {
		t.Fatal(err)
	}
	sightings.Report(served("ua", "198.51.100.1", nil), "text/html")
	deadline := time.Now().Add(5 * time.Second)
	for len(api.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(api.sent()) != 1 {
		t.Fatal("the background flusher never sent")
	}

	sightings.Report(served("before close", "198.51.100.1", nil), "text/html")
	if err := sightings.Close(); err != nil {
		t.Fatal(err)
	}
	if len(api.sent()) != 2 {
		t.Fatal("Close did not flush")
	}
	sightings.Report(served("after close", "198.51.100.1", nil), "text/html")
	sightings.Flush(context.Background())
	if len(api.sent()) != 2 || sightings.Close() != nil {
		t.Fatal("reported after Close")
	}
}

func TestOneFlushIsInFlightAtATime(t *testing.T) {
	h := newHarness(t, nil)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h.api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		entered <- struct{}{}
		<-release
		answerAll("r", 60)(w, batch)
	})
	h.Report(served("first", "198.51.100.1", nil), "text/html")
	done := make(chan struct{})
	go func() {
		h.Flush(context.Background())
		close(done)
	}()
	<-entered
	h.Report(served("second", "198.51.100.1", nil), "text/html")
	h.clock.Advance(time.Hour)
	h.flushDue() // A flush is in flight, so this returns at once.
	if len(h.api.sent()) != 1 {
		t.Fatal("a second flush ran alongside the first")
	}
	close(release)
	<-done
	if len(h.api.sent()) != 2 {
		t.Fatal("Flush did not send what arrived while it waited")
	}
}

func TestFlushWaitsABoundedTimeForAFlushInFlight(t *testing.T) {
	h := newHarness(t, nil)
	if h.flushWait != 6*time.Second {
		t.Fatalf("waits %s", h.flushWait)
	}
	h.flushWait = 100 * time.Millisecond
	h.Report(served("ua", "198.51.100.1", nil), "text/html")
	h.flushing <- struct{}{} // A flush that never finishes is in flight.
	started := time.Now()
	h.Flush(context.Background())
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("waited %s", elapsed)
	}
	if len(h.api.sent()) != 0 || len(h.pendingItems()) != 1 {
		t.Fatal("sent alongside the flush in flight")
	}
	<-h.flushing
	h.Flush(context.Background())
	if len(h.api.sent()) != 1 {
		t.Fatal("did not send once the flush in flight ended")
	}
}

func TestA200StoresAnswersAndDropsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.Report(served("ua", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.pendingItems()) != 0 || h.inFlight != 0 || h.cache.Len() != 1 {
		t.Fatal("the batch was not settled")
	}
}

func TestA422DropsTheBatchAndLogsOnceAMinute(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(answerStatus(http.StatusUnprocessableEntity, "invalid_request", nil))
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	h.Report(served("b", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.api.sent()) != 2 || len(h.pendingItems()) != 0 {
		t.Fatal("a 422 batch was kept or flushing paused")
	}
	if h.errors.count() != 1 {
		t.Fatalf("logged %d times", h.errors.count())
	}
	h.clock.Advance(time.Minute)
	h.Report(served("c", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if h.errors.count() != 2 {
		t.Fatal("not logged again after a minute")
	}
}

func TestA429KeepsTheBatchAndWaitsForRetryAfter(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(answerStatus(http.StatusTooManyRequests, "rate_limited",
		map[string]string{"Retry-After": "30", "Fingerly-RateLimit-Scope": "key"}))
	r := served("ua", "198.51.100.1", nil)
	h.Report(r, "text/html")
	h.Report(r, "text/html")
	h.Flush(context.Background())
	h.Report(r, "text/html")
	if items := h.pendingItems(); len(items) != 1 || items[0].Count != 3 {
		t.Fatalf("pending %+v", items)
	}
	h.api.setRespond(answerAll("r", 60))
	h.clock.Advance(29 * time.Second)
	h.Flush(context.Background())
	h.flushDue()
	if len(h.api.sent()) != 1 {
		t.Fatal("flushed before Retry-After passed")
	}
	h.clock.Advance(time.Second)
	h.Flush(context.Background())
	batches := h.api.sent()
	if len(batches) != 2 || batches[1][0].Count != 3 {
		t.Fatalf("sent %+v", batches)
	}
	if h.errors.count() != 0 {
		t.Fatal("a 429 was logged")
	}
}

func TestADaily429WithoutRetryAfterWaitsForTheNextUTCDay(t *testing.T) {
	now := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	daily := &APIError{Status: http.StatusTooManyRequests, RateLimitScope: RateLimitScopeDaily}
	if got := retryWait(now, daily); got != time.Hour {
		t.Fatalf("waits %s", got)
	}
	if got := retryWait(now, &APIError{Status: http.StatusTooManyRequests}); got != time.Minute {
		t.Fatalf("waits %s", got)
	}
	seconds := 7
	if got := retryWait(now, &APIError{RetryAfter: &seconds, RateLimitScope: RateLimitScopeDaily}); got != 7*time.Second {
		t.Fatalf("waits %s", got)
	}
}

func TestA401Or403DropsTheBatchAndPausesAMinute(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t, nil)
			h.api.setRespond(answerStatus(status, "unauthorized", nil))
			h.Report(served("a", "198.51.100.1", nil), "text/html")
			h.Flush(context.Background())
			if len(h.pendingItems()) != 0 || h.errors.count() != 1 {
				t.Fatal("the batch was kept or not logged")
			}
			h.Report(served("b", "198.51.100.1", nil), "text/html")
			h.clock.Advance(59 * time.Second)
			h.Flush(context.Background())
			if len(h.api.sent()) != 1 {
				t.Fatal("flushed during the pause")
			}
			h.api.setRespond(answerAll("r", 60))
			h.clock.Advance(time.Second)
			h.Flush(context.Background())
			if len(h.api.sent()) != 2 {
				t.Fatal("did not resume after a minute")
			}
		})
	}
}

func TestA503KeepsTheBatchAndBacksOff(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(answerStatus(http.StatusServiceUnavailable, "unavailable", nil))
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	for i, wait := range []time.Duration{1, 2, 4, 8, 16, 32, 60, 60} {
		h.Flush(context.Background())
		if len(h.api.sent()) != i+1 {
			t.Fatalf("attempt %d not sent", i+1)
		}
		h.clock.Advance(wait*time.Second - time.Millisecond)
		h.Flush(context.Background())
		if len(h.api.sent()) != i+1 {
			t.Fatalf("attempt %d retried before %ds", i+1, wait)
		}
		h.clock.Advance(time.Millisecond)
	}
	h.api.setRespond(answerAll("r", 60))
	h.Flush(context.Background())
	if len(h.pendingItems()) != 0 || h.backoff != 0 {
		t.Fatal("a 200 did not settle the kept batch and reset the backoff")
	}
}

func TestAKeptBatchFoldsWithRepeatsSeenWhileInFlight(t *testing.T) {
	h := newHarness(t, nil)
	r := served("ua", "198.51.100.1", nil)
	h.api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		h.Report(r, "text/html") // Served while the batch is in flight.
		answerStatus(http.StatusServiceUnavailable, "unavailable", nil)(w, batch)
	})
	h.Report(r, "text/html")
	h.Report(served("other", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	items := h.pendingItems()
	if len(items) != 2 || items[0].UserAgent != "ua" || items[0].Count != 2 || items[1].UserAgent != "other" {
		t.Fatalf("pending %+v", items)
	}
	if h.pendingBytes != 2+len("other")+2*(len("198.51.100.1")+len("shop.example")+len("GET")) {
		t.Fatalf("pending bytes %d", h.pendingBytes)
	}
}

func TestATransportErrorBeforeAnAnswerKeepsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.api.server.Close() // Connections are refused.
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.pendingItems()) != 1 || h.backoff != time.Second {
		t.Fatal("the batch was not kept with a backoff")
	}
	if h.errors.count() != 0 {
		t.Fatal("logged a retried failure")
	}
}

func TestAConnectionClosedAfterTheRequestWasWrittenDropsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(func(w http.ResponseWriter, _ []AgentSighting) {
		// The request body was read in full, then the connection is closed
		// without an answer.
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.pendingItems()) != 0 || h.backoff != 0 || !h.pausedUntil.IsZero() {
		t.Fatal("a batch the API may have counted was kept")
	}
	if h.errors.count() != 1 {
		t.Fatalf("logged %d times", h.errors.count())
	}
}

func TestABodyCutShortDropsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(func(w http.ResponseWriter, _ []AgentSighting) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rules_version":"r","items":[`))
		http.NewResponseController(w).Flush()
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
	})
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.pendingItems()) != 0 || h.backoff != 0 || !h.pausedUntil.IsZero() {
		t.Fatal("a batch the API may have counted was kept")
	}
}

func TestAWriteThatFailedIsNotWritten(t *testing.T) {
	var exchange sightingsExchange
	trace := exchange.trace()
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("broken pipe")})
	if exchange.wrote.Load() {
		t.Fatal("a failed write counted as written")
	}
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	if !exchange.wrote.Load() {
		t.Fatal("a full write not counted")
	}
}

func TestATTLIsClampedToADay(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(answerAll("r", 10*86400))
	r := served("ua", "198.51.100.1", nil)
	h.Report(r, "text/html")
	h.Flush(context.Background())
	h.clock.Advance(86400*time.Second - time.Millisecond)
	if !h.Lookup(r).Known() {
		t.Fatal("expired before a day")
	}
	h.clock.Advance(time.Millisecond)
	if h.Lookup(r).Known() {
		t.Fatal("served for more than a day")
	}
}

func TestARetryAfterOfZeroOrInThePastWaitsASecond(t *testing.T) {
	now := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	for _, header := range []string{"0", "Sat, 21 Nov 2026 22:00:00 GMT"} {
		apiErr := &APIError{RetryAfter: parseRetryAfter(header, now), RateLimitScope: RateLimitScopeDaily}
		if got := retryWait(now, apiErr); got != time.Second {
			t.Fatalf("Retry-After %q waits %s", header, got)
		}
	}
	// Negative or unreadable is treated as missing.
	for _, header := range []string{"-5", "soon"} {
		if got := retryWait(now, &APIError{RetryAfter: parseRetryAfter(header, now)}); got != time.Minute {
			t.Fatalf("Retry-After %q waits %s", header, got)
		}
		daily := &APIError{RetryAfter: parseRetryAfter(header, now), RateLimitScope: RateLimitScopeDaily}
		if got := retryWait(now, daily); got != time.Hour {
			t.Fatalf("daily Retry-After %q waits %s", header, got)
		}
	}

	h := newHarness(t, nil)
	h.api.setRespond(answerStatus(http.StatusTooManyRequests, "rate_limited",
		map[string]string{"Retry-After": "0", "Fingerly-RateLimit-Scope": "key"}))
	h.Report(served("ua", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	h.api.setRespond(answerAll("r", 60))
	h.Flush(context.Background())
	if len(h.api.sent()) != 1 {
		t.Fatal("flushed again without pausing")
	}
	h.clock.Advance(time.Second)
	h.Flush(context.Background())
	if len(h.api.sent()) != 2 {
		t.Fatal("did not resume after a second")
	}
}

func TestATimeoutAfterTheRequestWasSentDropsTheBatch(t *testing.T) {
	h := newHarness(t, nil)
	h.deadline = 100 * time.Millisecond
	h.api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		time.Sleep(400 * time.Millisecond)
		answerAll("r", 60)(w, batch)
	})
	h.Report(served("a", "198.51.100.1", nil), "text/html")
	h.Flush(context.Background())
	if len(h.pendingItems()) != 0 || h.backoff != 0 || !h.pausedUntil.IsZero() {
		t.Fatal("a batch the API may have counted was kept")
	}
	if h.errors.count() != 1 || !errors.Is(h.errors.errs[0], errSightingsTimedOut) {
		t.Fatalf("logged %v", h.errors.errs)
	}
}

func TestOtherFailuresDropTheBatch(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter, []AgentSighting){
		"500":      answerStatus(http.StatusInternalServerError, "internal", nil),
		"413":      answerStatus(http.StatusRequestEntityTooLarge, "too_large", nil),
		"mismatch": func(w http.ResponseWriter, _ []AgentSighting) { answerAll("r", 60)(w, nil) },
		"garbage":  respondRawFixture(http.StatusOK, json.RawMessage(`not json`)).toRespond(),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.api.setRespond(respond)
			h.Report(served("a", "198.51.100.1", nil), "text/html")
			h.Flush(context.Background())
			if len(h.pendingItems()) != 0 || h.errors.count() != 1 || !h.pausedUntil.IsZero() {
				t.Fatalf("pending %d, logged %d", len(h.pendingItems()), h.errors.count())
			}
		})
	}
}

func TestLogsNeverCarryAReportedField(t *testing.T) {
	h := newHarness(t, nil)
	h.api.setRespond(answerStatus(http.StatusUnprocessableEntity, "invalid_request", nil))
	h.Report(served("SecretAgent/1.0", "198.51.100.77", nil), "text/html")
	h.Flush(context.Background())
	for _, err := range h.errors.errs {
		if text := err.Error(); strings.Contains(text, "SecretAgent") || strings.Contains(text, "198.51.100.77") ||
			strings.Contains(text, testSecretKey) {
			t.Fatalf("logged %q", text)
		}
	}
}

func TestReportScope(t *testing.T) {
	cases := []struct {
		name        string
		apply       func(*AgentSightingsOptions)
		path        string
		contentType string
		want        bool
	}{
		{"html page", nil, "/products/1", "text/html; charset=utf-8", true},
		{"xhtml page", nil, "/a", "Application/XHTML+XML", true},
		{"json api", nil, "/api/items", "application/json", false},
		{"stylesheet", nil, "/app.css", "text/css", false},
		{"robots", nil, "/robots.txt", "text/plain", true},
		{"llms", nil, "/llms.txt", "", true},
		{"llms full", nil, "/llms-full.txt", "text/markdown", true},
		{"robots is exact", nil, "/robots.txt.bak", "text/plain", false},
		{"robots is case-sensitive", nil, "/ROBOTS.TXT", "text/plain", false},
		{"include prefix", func(o *AgentSightingsOptions) { o.IncludePaths = []string{"/api/"} }, "/api/items", "application/json", true},
		{"exclude wins", func(o *AgentSightingsOptions) { o.ExcludePaths = []string{"/admin"} }, "/admin/x", "text/html", false},
		{"exclude wins over robots", func(o *AgentSightingsOptions) { o.ExcludePaths = []string{"/robots"} }, "/robots.txt", "text/plain", false},
		{"content types replace", func(o *AgentSightingsOptions) { o.ContentTypes = []string{"application/json"} }, "/x", "application/json", true},
		{"replaced html", func(o *AgentSightingsOptions) { o.ContentTypes = []string{"application/json"} }, "/x", "text/html", false},
		{"paths only", func(o *AgentSightingsOptions) { o.ContentTypes = []string{} }, "/x", "text/html", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.apply)
			if got := h.inScope(tc.path, tc.contentType); got != tc.want {
				t.Fatalf("inScope = %v", got)
			}
		})
	}
}

func TestMiddlewareReportsOnlyInScopeResponses(t *testing.T) {
	h := newHarness(t, nil)
	site := http.NewServeMux()
	site.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<p>hi</p>")
	})
	site.HandleFunc("/sniffed", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>hi</body></html>")
	})
	site.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
	})
	site.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	})
	site.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "User-agent: *")
	})
	handler := h.Middleware(site)
	for _, path := range []string{"/page", "/sniffed", "/missing", "/api", "/robots.txt"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("User-Agent", "ua "+path)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if path == "/page" && w.Body.String() != "<p>hi</p>" {
			t.Fatal("the response changed")
		}
	}
	var reported []string
	for _, item := range h.pendingItems() {
		reported = append(reported, strings.TrimPrefix(item.UserAgent, "ua "))
	}
	if strings.Join(reported, ",") != "/page,/sniffed,/missing,/robots.txt" {
		t.Fatalf("reported %v", reported)
	}
}

func TestMiddlewareNeverWaitsOnTheAPI(t *testing.T) {
	api := newSightingsAPI(t)
	release := make(chan struct{})
	api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		<-release
		answerAll("r", 60)(w, batch)
	})
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := NewAgentSightings(client, AgentSightingsOptions{ClientIP: testIP})
	if err != nil {
		t.Fatal(err)
	}
	site := sightings.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "ok")
	}))
	start := time.Now()
	for i := range 3 * maxSightingsItems {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("User-Agent", "ua"+strconv.Itoa(i))
		site.ServeHTTP(httptest.NewRecorder(), r)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("serving waited on the API: %s", elapsed)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(api.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(api.sent()) != 1 {
		t.Fatal("the flusher did not start a flush")
	}
	close(release)
	_ = sightings.Close()
	if total := len(api.sent()); total != 3 {
		t.Fatalf("Close left reports unsent: %d batches", total)
	}
}

func TestMiddlewareKeepsFlushAndUnwrap(t *testing.T) {
	h := newHarness(t, nil)
	var flushed, unwrapped bool
	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "part")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
		_, unwrapped = w.(interface{ Unwrap() http.ResponseWriter })
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !flushed || !w.Flushed || !unwrapped {
		t.Fatal("the writer lost Flush or Unwrap")
	}
	if len(h.pendingItems()) != 1 {
		t.Fatal("a streamed page was not reported")
	}
}

// TestMiddlewareReportsTheTypeTheVisitorSaw serves each page through a real
// net/http server and checks a page is reported exactly when the visitor got
// it as text/html, however the handler wrote it.
func TestMiddlewareReportsTheTypeTheVisitorSaw(t *testing.T) {
	const page = "<!DOCTYPE html><html><body>hi</body></html>"
	file := filepath.Join(t.TempDir(), "page")
	if err := os.WriteFile(file, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	handlers := map[string]http.HandlerFunc{
		"/sniffed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, page)
		},
		"/status-first": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, page)
		},
		"/not-found": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, page)
		},
		// A template writes its text in pieces; net/http sniffs what it
		// buffered, not the first piece alone.
		"/pieces": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "\n")
			_, _ = io.WriteString(w, page)
		},
		"/twice": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, page)
		},
		"/encoded": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "identity-ish")
			_, _ = io.WriteString(w, page)
		},
		"/flushed-first": func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, page)
		},
		"/no-content": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"/early-hints": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, page)
		},
		"/copied": func(w http.ResponseWriter, _ *http.Request) {
			f, err := os.Open(file)
			if err != nil {
				panic(err)
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
		},
		"/served-file": func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, file)
		},
	}
	h := newHarness(t, nil)
	site := http.NewServeMux()
	for path, handler := range handlers {
		site.Handle(path, handler)
	}
	server := httptest.NewServer(h.Middleware(site))
	defer server.Close()

	saw := map[string]bool{}
	for path := range handlers {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		request.Header.Set("User-Agent", path)
		request.Header.Set("X-Test-IP", "198.51.100.1")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		saw[path] = mediaType(response.Header.Get("Content-Type")) == "text/html"
	}
	reported := map[string]bool{}
	for _, item := range h.pendingItems() {
		reported[item.UserAgent] = true
	}
	for path, html := range saw {
		if reported[path] != html {
			t.Errorf("%s: the visitor got text/html %v, reported %v", path, html, reported[path])
		}
	}
	for _, path := range []string{"/sniffed", "/pieces", "/status-first", "/not-found", "/copied"} {
		if !saw[path] {
			t.Errorf("%s: net/http did not sniff text/html; the case tests nothing", path)
		}
	}
}

// fancyWriter is an underlying writer with every optional interface, each
// recording that it was reached.
type fancyWriter struct {
	*httptest.ResponseRecorder
	flushErr error
	reached  map[string]bool
}

func (w *fancyWriter) FlushError() error { w.reached["flush"] = true; return w.flushErr }
func (w *fancyWriter) Push(string, *http.PushOptions) error {
	w.reached["push"] = true
	return nil
}

func (w *fancyWriter) CloseNotify() <-chan bool { w.reached["closenotify"] = true; return nil }
func (w *fancyWriter) ReadFrom(src io.Reader) (int64, error) {
	w.reached["readfrom"] = true
	return io.Copy(w.ResponseRecorder, src)
}

func TestMiddlewareKeepsEveryWriterInterface(t *testing.T) {
	h := newHarness(t, nil)
	broken := errors.New("the client went away")
	underlying := &fancyWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: broken, reached: map[string]bool{}}
	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Frameworks assert these without checking, gin's Stream for one.
		_ = w.(http.CloseNotifier).CloseNotify()
		if err := w.(http.Pusher).Push("/app.css", nil); err != nil {
			t.Errorf("Push: %v", err)
		}
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, broken) {
			t.Errorf("ResponseController.Flush returned %v, not the writer's error", err)
		}
		// A reader without WriteTo, so io.Copy asks the writer to ReadFrom.
		_, _ = io.Copy(w, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 2*sniffLen))})
	}))
	handler.ServeHTTP(underlying, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, name := range []string{"closenotify", "push", "flush", "readfrom"} {
		if !underlying.reached[name] {
			t.Errorf("%s did not reach the underlying writer", name)
		}
	}
	if underlying.Body.Len() != 2*sniffLen {
		t.Fatalf("wrote %d bytes", underlying.Body.Len())
	}
	if len(h.pendingItems()) != 1 {
		t.Fatal("the page was not reported")
	}

	// A writer without them: Push says so, CloseNotify never fires.
	plain := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := w.(http.Pusher).Push("/app.css", nil); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Push on a writer without it returned %v", err)
		}
		select {
		case <-w.(http.CloseNotifier).CloseNotify():
			t.Error("CloseNotify fired")
		default:
		}
	}))
	plain.ServeHTTP(struct{ http.ResponseWriter }{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestMiddlewareKeepsWebSocketUpgrades(t *testing.T) {
	h := newHarness(t, nil)
	server := httptest.NewServer(h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nhello")
		_ = buffered.Flush()
	})))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/ws", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusSwitchingProtocols || string(body) != "hello" {
		t.Fatalf("got %d %q", response.StatusCode, body)
	}
}

func TestSightingsDecodesTheFixture(t *testing.T) {
	api := newSightingsAPI(t)
	api.setRespond(respondRawFixture(http.StatusOK, responseFixture(t, "agent_sightings")).toRespond())
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	answer, err := client.Agents.Sightings(context.Background(), []AgentSighting{{UserAgent: "ua", ClientIP: "198.51.100.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if answer.RulesVersion != "0123456789abcdef" || len(answer.Items) != 5 || len(answer.Raw) == 0 {
		t.Fatalf("answer %+v", answer)
	}
	human, crawler, gptbot, fetcher, agent := answer.Items[0], answer.Items[4], answer.Items[1], answer.Items[2], answer.Items[3]
	if human.VisitorKind != VisitorKindHuman || human.AI != nil || human.TTL != 3600 {
		t.Fatalf("human %+v", human)
	}
	if crawler.VisitorKind != VisitorKindSearchCrawler || crawler.Agent != "Googlebot" || crawler.AI != nil {
		t.Fatalf("crawler %+v", crawler)
	}
	if gptbot.AI == nil || gptbot.AI.Verification.Method != AIVerificationIPList ||
		gptbot.AI.Verification.Result != AIVerificationPass || gptbot.AI.Runtime != AIRuntimeProviderCloud {
		t.Fatalf("gptbot %+v", gptbot)
	}
	if fetcher.AI == nil || fetcher.AI.Purpose != AIPurposeUserFetch || fetcher.AI.Verification.Result != AIVerificationFail {
		t.Fatalf("fetcher %+v", fetcher)
	}
	if agent.TTL != 0 || agent.AI == nil || agent.AI.Verification.Method != AIVerificationSignature ||
		agent.AI.Purpose != AIPurposeAgent {
		t.Fatalf("agent %+v", agent)
	}
}

func TestSightingsPostsTheItemsWithTheSecretKey(t *testing.T) {
	api := newSightingsAPI(t)
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	items := []AgentSighting{{UserAgent: "ua", ClientIP: "198.51.100.1", Method: "GET", Count: 4}}
	if _, err := client.Agents.Sightings(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.paths[0] != "/api/v1/agents/sightings" || api.headers[0].Get("X-API-Key") != testSecretKey ||
		api.headers[0].Get("Content-Type") != "application/json" || api.batches[0][0] != items[0] {
		t.Fatalf("sent %s %+v", api.paths[0], api.batches[0])
	}
}

func TestSightingsMapsA429WithItsScope(t *testing.T) {
	api := newSightingsAPI(t)
	api.setRespond(answerStatus(http.StatusTooManyRequests, "rate_limited",
		map[string]string{"Retry-After": "120", "Fingerly-RateLimit-Scope": RateLimitScopeDaily}))
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	_, err := client.Agents.Sightings(context.Background(), []AgentSighting{{ClientIP: "198.51.100.1"}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Code != "rate_limited" ||
		apiErr.RetryAfter == nil || *apiErr.RetryAfter != 120 || apiErr.RateLimitScope != RateLimitScopeDaily {
		t.Fatalf("err %#v", err)
	}
}

func TestAPanickingClientIPNeverBreaksTheResponse(t *testing.T) {
	h := newHarness(t, func(options *AgentSightingsOptions) {
		options.ClientIP = func(*http.Request) string { panic("the host's bug") }
	})
	site := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer := h.Lookup(r); answer != AgentUnknown {
			t.Errorf("answer %+v", answer)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "page")
	}))
	w := httptest.NewRecorder()
	site.ServeHTTP(w, served("ua", "198.51.100.1", nil))
	if w.Code != http.StatusOK || w.Body.String() != "page" {
		t.Fatalf("served %d %q", w.Code, w.Body.String())
	}
	if len(h.pendingItems()) != 0 {
		t.Fatal("reported a request whose address did not resolve")
	}
	if h.errors.count() != 1 || !errors.Is(h.errors.errs[0], errClientIPPanicked) {
		t.Fatalf("errors %v", h.errors.errs)
	}
}

func TestAPanickingOnErrorNeverEndsTheProcess(t *testing.T) {
	api := newSightingsAPI(t)
	api.setRespond(answerStatus(http.StatusUnprocessableEntity, "invalid_request", nil))
	var buffer strings.Builder
	var bufferMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		bufferMu.Lock()
		defer bufferMu.Unlock()
		return buffer.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := NewAgentSightings(client, AgentSightingsOptions{
		ClientIP: testIP,
		OnError:  func(error) { panic("the host's bug") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sightings.Close()
	// 200 waiting wakes the background flusher at once; its 422 reaches OnError
	// on the flusher's goroutine, where an unrecovered panic ends the process.
	for i := range maxSightingsItems {
		sightings.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(api.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	api.setRespond(nil)
	for i := range maxSightingsItems {
		sightings.Report(served("again"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	for len(api.sent()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(api.sent()) != 2 {
		t.Fatalf("the flusher stopped after OnError panicked: %d batches", len(api.sent()))
	}
	bufferMu.Lock()
	defer bufferMu.Unlock()
	if !strings.Contains(buffer.String(), "OnError panicked") {
		t.Fatalf("logged %q", buffer.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestTheFlusherStartsNoBatchOnceShutdownBegan(t *testing.T) {
	h := newHarness(t, nil)
	for i := range 3 * maxSightingsItems {
		h.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	h.clock.Advance(time.Hour)
	close(h.stop)
	// A tick and a wake ready alongside stop: select would pick one of them
	// half the time, so try often enough that a lucky pick cannot pass.
	ticks := make(chan time.Time, 1)
	for range 32 {
		h.done = make(chan struct{})
		select {
		case h.wake <- struct{}{}:
		default:
		}
		select {
		case ticks <- time.Now():
		default:
		}
		h.loopWith(ticks)
		if sent := len(h.api.sent()); sent != 0 {
			t.Fatalf("the flusher sent %d batches after Shutdown began", sent)
		}
	}
}

func TestADueFlushStopsBetweenBatchesOnceShutdownBegan(t *testing.T) {
	h := newHarness(t, nil)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	h.api.setRespond(func(w http.ResponseWriter, batch []AgentSighting) {
		entered <- struct{}{}
		<-release
		answerAll("r", 60)(w, batch)
	})
	for i := range 3 * maxSightingsItems {
		h.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	h.clock.Advance(time.Hour)
	done := make(chan struct{})
	go func() {
		h.flushDue()
		close(done)
	}()
	<-entered
	close(h.stop)
	close(release)
	<-done
	if sent := len(h.api.sent()); sent != 1 {
		t.Fatalf("sent %d batches after Shutdown began", sent)
	}
	if len(h.pendingItems()) != 2*maxSightingsItems {
		t.Fatal("the reports left for Shutdown's own flush were lost")
	}
}

func TestShutdownGivesUpWhenItsContextEnds(t *testing.T) {
	api := newSightingsAPI(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	api.setRespond(func(http.ResponseWriter, []AgentSighting) { <-release })
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := NewAgentSightings(client, AgentSightingsOptions{ClientIP: testIP, OnError: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 * maxSightingsItems {
		sightings.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(api.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(api.sent()) != 1 {
		t.Fatal("the flusher did not start a batch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := sightings.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Shutdown took %s with a 200ms context", elapsed)
	}
	select {
	case <-sightings.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the flusher is still running: its batch was not abandoned")
	}
	if err := sightings.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second Shutdown returned %v, not the first one's result", err)
	}
	if sightings.Close() != nil {
		t.Fatal("Close returned an error")
	}
}

func TestShutdownAndCloseAreSafeConcurrentlyAndAfterwards(t *testing.T) {
	api := newSightingsAPI(t)
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := NewAgentSightings(client, AgentSightingsOptions{ClientIP: testIP})
	if err != nil {
		t.Fatal(err)
	}
	sightings.Report(served("before", "198.51.100.1", nil), "text/html")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_ = sightings.Close()
			} else {
				_ = sightings.Shutdown(context.Background())
			}
		}()
		go func() {
			defer wg.Done()
			r := served("during"+strconv.Itoa(i), "198.51.100.1", nil)
			sightings.Report(r, "text/html")
			_ = sightings.Lookup(r)
			sightings.Flush(context.Background())
		}()
	}
	wg.Wait()
	sent := len(api.sent())
	r := served("after", "198.51.100.1", nil)
	sightings.Report(r, "text/html")
	_ = sightings.Lookup(r)
	sightings.Flush(context.Background())
	if sightings.Shutdown(context.Background()) != nil || sightings.Close() != nil {
		t.Fatal("a late Shutdown or Close failed")
	}
	if len(api.sent()) != sent {
		t.Fatal("reported after Close")
	}
	var total int
	for _, batch := range api.sent() {
		total += len(batch)
	}
	if total == 0 {
		t.Fatal("Close sent nothing")
	}
}

// startedSightings is an AgentSightings with its background flusher, whose
// waits for a flush in flight are short.
func startedSightings(t *testing.T, api *sightingsAPI, onError func(error)) *AgentSightings {
	t.Helper()
	client := New(testSecretKey, WithEndpoint(api.server.URL), WithAllowInsecureLoopback())
	sightings, err := newAgentSightings(client, AgentSightingsOptions{ClientIP: testIP, OnError: onError})
	if err != nil {
		t.Fatal(err)
	}
	sightings.flushWait = 200 * time.Millisecond
	sightings.start()
	return sightings
}

func TestShutdownKeepsItsDeadlineWhileOnErrorBlocks(t *testing.T) {
	api := newSightingsAPI(t)
	api.setRespond(answerStatus(http.StatusUnprocessableEntity, "invalid_request", nil))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sightings := startedSightings(t, api, func(error) {
		entered <- struct{}{}
		<-release
	})
	for i := range maxSightingsItems {
		sightings.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the flusher never reported the 422")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- sightings.Shutdown(ctx) }()
	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown waited past its context for a blocked OnError")
	}
}

func TestCloseFromOnErrorReturns(t *testing.T) {
	api := newSightingsAPI(t)
	api.setRespond(answerStatus(http.StatusUnauthorized, "unauthorized", nil))
	returned := make(chan struct{})
	var sightings *AgentSightings
	var once sync.Once
	ready := make(chan struct{})
	sightings = startedSightings(t, api, func(error) {
		<-ready
		// A host that stops reporting once the key is refused.
		once.Do(func() {
			_ = sightings.Close()
			close(returned)
		})
	})
	close(ready)
	for i := range maxSightingsItems {
		sightings.Report(served("ua"+strconv.Itoa(i), "198.51.100.1", nil), "text/html")
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Close called from OnError never returned")
	}
	select {
	case <-sightings.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the flusher did not stop")
	}
}

func TestARetryAfterOverADayWaitsADay(t *testing.T) {
	now := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	// The larger of these overflowed a Duration: some paused reporting for
	// years, and the largest waited a second, asking again every second.
	for _, header := range []string{"86401", "99999999999", "99999999999999", "9223372036854775807", "99999999999999999999"} {
		apiErr := &APIError{RetryAfter: parseRetryAfter(header, now)}
		if got := retryWait(now, apiErr); got != 24*time.Hour {
			t.Fatalf("Retry-After %q waits %s", header, got)
		}
	}
}
