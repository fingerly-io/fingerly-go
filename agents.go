package fingerly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// AgentsService reports the requests your server served, as raw input, and
// answers what sent each one. Reach it as client.Agents.
//
// AI crawlers and fetchers that never run a page's JavaScript are seen only
// this way. Most servers do not call it directly: AgentSightings batches the
// reports, remembers the answers and never touches the request being served.
type AgentsService struct {
	client *Client
}

// AgentSighting is one request your server served, as it was received. The
// SDK decides nothing about it: the API does.
type AgentSighting struct {
	// UserAgent is the User-Agent header as received, or "" when absent. At
	// most 4096 bytes.
	UserAgent string `json:"user_agent"`
	// ClientIP is one IPv4 or IPv6 literal from your trusted client-IP source,
	// never a forwarded list.
	ClientIP string `json:"client_ip"`
	// Host is the Host the request was sent to, which a signing agent signs.
	// AgentSightings leaves out one over 4096 bytes.
	Host string `json:"host,omitempty"`
	// Method is the request method; the API reads an empty one as GET.
	Method string `json:"method,omitempty"`
	// Signature, SignatureInput and SignatureAgent are the Web Bot Auth
	// headers, when the request had them. Each at most 4096 bytes.
	Signature      string `json:"signature,omitempty"`
	SignatureInput string `json:"signature_input,omitempty"`
	SignatureAgent string `json:"signature_agent,omitempty"`
	// Count is how many times this exact request was served since it was last
	// reported, this one included. The API reads 0 as 1; at most 1000000.
	Count int64 `json:"count,omitempty"`
}

// AgentSightingsAnswer answers every reported request, in the order sent.
type AgentSightingsAnswer struct {
	// RulesVersion names the server's classification rules. When it changes,
	// every remembered answer is stale.
	RulesVersion string `json:"rules_version"`
	// Items holds one answer per reported request, in the same order.
	Items []AgentAnswer `json:"items"`
	// Raw is the response body exactly as it arrived.
	Raw json.RawMessage `json:"-"`
}

// AgentAnswer is what sent one request, as the API answered it.
//
// AgentSightings.Lookup returns AgentUnknown when it holds no answer for a
// request; treat that as "serve as usual".
type AgentAnswer struct {
	// TTL is how many seconds this answer may be reused for the exact same
	// user agent, address and Signature-Agent; 0 for a signed request, which
	// is checked every time.
	TTL int `json:"ttl"`
	// VisitorKind is VisitorKindHuman, VisitorKindAIBot, VisitorKindAutomation
	// or VisitorKindSearchCrawler, and "" in AgentUnknown.
	VisitorKind VisitorKind `json:"visitor_kind"`
	// Agent is the agent's name where the server knows it, such as GPTBot or
	// Googlebot. Empty for a person's browser.
	Agent string `json:"agent,omitempty"`
	// AI is who operates the agent, what its traffic is for and how far its
	// claim was checked. Present only when VisitorKind is VisitorKindAIBot.
	AI *AIAgent `json:"ai,omitempty"`
}

// AgentUnknown is the answer AgentSightings.Lookup gives for a request it
// holds no answer for: no kind, no agent, no AI. Serve the request as usual.
//
//	if answer := sightings.Lookup(r); answer == fingerly.AgentUnknown {
//		// Not answered yet: serve as usual.
//	}
var AgentUnknown = AgentAnswer{}

// Known reports whether the answer came from the API rather than being
// AgentUnknown.
func (a AgentAnswer) Known() bool { return a.VisitorKind != "" }

// maxSightingsItems is the most requests one report may carry.
const maxSightingsItems = 200

// Sightings reports up to 200 served requests in one call and returns the
// API's answer for each, in the same order. It never retries: a 429 is an
// *APIError whose RetryAfter and RateLimitScope say when to report again.
//
// The call needs a secret key.
func (s *AgentsService) Sightings(ctx context.Context, items []AgentSighting) (*AgentSightingsAnswer, error) {
	if items == nil {
		items = []AgentSighting{}
	}
	payload, err := json.Marshal(struct {
		Items []AgentSighting `json:"items"`
	}{items})
	if err != nil {
		return nil, fmt.Errorf("fingerly: encode sightings: %w", err)
	}
	body, err := s.client.send(ctx, http.MethodPost, "/agents/sightings", payload)
	if err != nil {
		return nil, err
	}
	var answer AgentSightingsAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("fingerly: decode sightings answer: %w", err)
	}
	answer.Raw = body
	return &answer, nil
}
