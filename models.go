package fingerly

import (
	"encoding/json"
	"time"
)

// EventTrigger is one signal group that contributed to an event's score.
type EventTrigger struct {
	// Signal is the signal group, such as "vpn" or "bot".
	Signal string `json:"signal"`
	// Weight is the group's combined weight in this score.
	Weight int `json:"weight"`
	// Confidence is "low", "medium" or "high".
	Confidence string `json:"confidence,omitempty"`
}

// Event is a stored identification event.
//
// SuspectScore is a pointer because nil means unscored, which must not be read
// as zero: an ordinary visitor about whom nothing fired scores 0, while a
// submission that was never enriched has no score at all.
type Event struct {
	RequestID string `json:"request_id"`
	// ProjectID is the Fingerly project the event belongs to. Empty when the
	// server predates projects and did not send it.
	ProjectID  string    `json:"project_id,omitempty"`
	VisitorID  string    `json:"visitor_id"`
	OccurredAt time.Time `json:"occurred_at"`
	// State is "enriched", or "unavailable" when the network lookup could not run.
	State string `json:"state"`

	Environment string `json:"environment"`
	// Platform is the scoring profile applied; SDKPlatform is what the SDK
	// called itself. They differ: react-native-ios reports itself as such and
	// is scored as ios.
	Platform    string `json:"platform"`
	SDKPlatform string `json:"sdk_platform"`
	SDKVersion  string `json:"sdk_version"`

	Origin  string `json:"origin,omitempty"`
	URLHost string `json:"url_host,omitempty"`
	URLPath string `json:"url_path,omitempty"`
	// Tag is your own reference, as the client sent it.
	Tag string `json:"tag,omitempty"`

	// IPNetwork is the network the request came from: an IPv4 /24 or an IPv6
	// /40, such as 203.0.113.0/24. The API never returns the address itself.
	// Empty when unknown.
	IPNetwork   string `json:"ip_network,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	CountryName string `json:"country_name,omitempty"`
	ASN         string `json:"asn,omitempty"`
	ASNName     string `json:"asn_name,omitempty"`
	// AnonymityNetwork is "tor", "vpn", "proxy" or "hosting". Empty on an
	// ordinary consumer connection, which is most of them.
	AnonymityNetwork string `json:"anonymity_network,omitempty"`

	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	OS           string `json:"os,omitempty"`
	DeviceKind   string `json:"device_kind,omitempty"`
	VisitorKind  string `json:"visitor_kind,omitempty"`

	// SuspectScore is nil when the request was not scored. Never read nil as 0.
	SuspectScore *int `json:"suspect_score"`
	// SuspectLevel is "low", "medium" or "high". Empty when not scored.
	SuspectLevel string `json:"suspect_level,omitempty"`

	// Triggers is never nil from the API: an event about which nothing fired
	// carries an empty list.
	Triggers []EventTrigger `json:"triggers"`

	// HasDocument says whether the full submission was archived.
	HasDocument bool `json:"has_document"`

	// Document is an allow-listed view of the archived submission: schema,
	// request id, time, SDK platform and version, IP network, the classified
	// IP (network, city-level location, detections) or IP error, risk,
	// attestation and velocity flags. It carries no raw signals, no user agent
	// and no IP address. Present when HasDocument is true and the archive could
	// be read. Its contents are deliberately opaque, not a public contract.
	Document json.RawMessage `json:"document,omitempty"`

	// Raw is the response body exactly as it arrived, so a field the API adds
	// after this SDK was released is still reachable without an upgrade.
	Raw json.RawMessage `json:"-"`
}

// Scored reports whether the API assigned this event a suspect score.
//
// Prefer it to a nil check: it states the question being asked.
func (e *Event) Scored() bool { return e != nil && e.SuspectScore != nil }

// Score returns the suspect score and whether the event was scored at all.
//
//	score, ok := event.Score()
//	if !ok { /* unscored, decide on something else */ }
func (e *Event) Score() (int, bool) {
	if e == nil || e.SuspectScore == nil {
		return 0, false
	}
	return *e.SuspectScore, true
}

// EventPage is one page of events, newest first.
type EventPage struct {
	Rows     []Event `json:"rows"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`

	// Raw is the response body exactly as it arrived.
	Raw json.RawMessage `json:"-"`
}

// EventListParams filters a listing. A zero field is not sent, so the server
// applies its own default.
type EventListParams struct {
	// From and To bound the window. The default is the last 24 hours; it may
	// be at most 30 days wide and start at most 30 days ago.
	From time.Time
	To   time.Time
	// Page is one-based. Limit is capped at 200 by the server.
	Page  int
	Limit int
	// Visitor restricts the listing to one exact visitor id.
	Visitor string
	// Level restricts it to "low", "medium" or "high".
	Level string
}
