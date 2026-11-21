package fingerly

import (
	"encoding/json"
	"time"
)

// EventTrigger is one signal group that contributed to an event's score.
type EventTrigger struct {
	// Signal is the signal group, such as SignalVPN, SignalBot or
	// SignalAttestationFailed.
	Signal Signal `json:"signal"`
	// Weight is the group's combined weight in this score.
	Weight int `json:"weight"`
	// Confidence is ConfidenceLow, ConfidenceMedium or ConfidenceHigh.
	Confidence Confidence `json:"confidence,omitempty"`
}

// Attestation is the platform-neutral hardware attestation verdict inside an
// archived event document.
type Attestation struct {
	// State is AttestationStateAttested, AttestationStateNotAttested or
	// AttestationStateFailed.
	State    AttestationState `json:"state"`
	Platform string           `json:"platform,omitempty"`
	// Status is AttestationStatusVerified, AttestationStatusIndeterminate,
	// AttestationStatusInvalid, AttestationStatusAbsent or
	// AttestationStatusUnconfigured.
	Status AttestationStatus `json:"status"`
	Reason string            `json:"reason,omitempty"`

	ChainLength              int    `json:"chain_length,omitempty"`
	AttestationSecurityLevel string `json:"attestation_security_level,omitempty"`
	KeyMintSecurityLevel     string `json:"keymint_security_level,omitempty"`
	DeviceLocked             *bool  `json:"device_locked,omitempty"`
	VerifiedBootState        string `json:"verified_boot_state,omitempty"`
	Nonce                    string `json:"nonce,omitempty"`
	RevocationChecked        bool   `json:"revocation_checked"`
	PackageBound             bool   `json:"package_bound"`
	SignerBound              bool   `json:"signer_bound"`

	// AttestSkipped is the app's own reason for sending no attestation, such
	// as AttestSkippedNotOffered, AttestSkippedUnsupported or
	// AttestSkippedTimeout. Diagnostic only; it never lowers the score.
	AttestSkipped AttestSkippedReason `json:"attest_skipped,omitempty"`
	// Downgrade is true when an unproven or rejected report named an install
	// an attested device owns, so it was answered unidentifiable.
	Downgrade bool `json:"downgrade,omitempty"`

	Mode        string `json:"mode,omitempty"`
	AppID       string `json:"app_id,omitempty"`
	Environment string `json:"environment,omitempty"`

	Receipt        string          `json:"receipt,omitempty"`
	RiskMetric     *int            `json:"risk_metric,omitempty"`
	RiskMetricAgeH *int            `json:"risk_metric_age_h,omitempty"`
	DeviceCheck    string          `json:"device_check,omitempty"`
	PlayIntegrity  json.RawMessage `json:"play_integrity,omitempty"`
}

// EventDocumentSDK identifies the client SDK recorded in an archived document.
type EventDocumentSDK struct {
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// EventRiskTrigger is one raw detector recorded inside an archived risk report.
type EventRiskTrigger struct {
	// Signal is the raw detector, such as SignalVPNPublic.
	Signal     Signal     `json:"signal"`
	Weight     int        `json:"weight"`
	Confidence Confidence `json:"confidence,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

// EventRiskReport is the scoring assessment recorded inside an archived document.
type EventRiskReport struct {
	Platform  RiskPlatform       `json:"platform"`
	Score     int                `json:"score"`
	Level     SuspectLevel       `json:"level"`
	Threshold int                `json:"threshold"`
	Source    RiskSource         `json:"source"`
	Revision  int64              `json:"revision,omitempty"`
	Triggers  []EventRiskTrigger `json:"triggers"`
}

// EventDocument is the allow-listed view of an archived identification
// submission returned when Event.HasDocument is true.
type EventDocument struct {
	Schema      int              `json:"schema"`
	RequestID   string           `json:"request_id"`
	OccurredAt  time.Time        `json:"occurred_at"`
	SDK         EventDocumentSDK `json:"sdk"`
	IPNetwork   string           `json:"ip_network,omitempty"`
	IP          json.RawMessage  `json:"ip,omitempty"`
	IPError     string           `json:"ip_error,omitempty"`
	Risk        *EventRiskReport `json:"risk,omitempty"`
	Attestation *Attestation     `json:"attestation,omitempty"`
	Velocity    json.RawMessage  `json:"velocity,omitempty"`
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
	ProjectID string `json:"project_id,omitempty"`
	// VisitorID is the stable project-scoped visitor identifier, or "" when
	// the submission was unidentifiable (identifiable == false).
	VisitorID  string    `json:"visitor_id"`
	OccurredAt time.Time `json:"occurred_at"`
	// State is EventStateEnriched, or EventStateUnavailable when the network
	// lookup could not run.
	State EventState `json:"state"`
	// Reason explains why enrichment did not happen on an unavailable event.
	Reason string `json:"reason,omitempty"`

	// Environment is the deployment the event was recorded in.
	Environment Environment `json:"environment"`
	// Platform is the scoring profile applied; SDKPlatform is what the SDK
	// called itself. They differ: react-native-ios reports itself as such and
	// is scored as RiskPlatformIOS.
	Platform    RiskPlatform `json:"platform"`
	SDKPlatform string       `json:"sdk_platform"`
	SDKVersion  string       `json:"sdk_version"`

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
	// AnonymityNetwork is AnonymityNetworkTor, AnonymityNetworkRelay,
	// AnonymityNetworkVPN, AnonymityNetworkProxy or AnonymityNetworkHosting.
	// Empty on an ordinary consumer connection, which is most of them.
	AnonymityNetwork AnonymityNetwork `json:"anonymity_network,omitempty"`

	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	OS           string `json:"os,omitempty"`
	DeviceKind   string `json:"device_kind,omitempty"`
	// VisitorKind is VisitorKindHuman, VisitorKindAIBot, VisitorKindAutomation
	// or VisitorKindSearchCrawler.
	VisitorKind VisitorKind `json:"visitor_kind,omitempty"`
	// AI names the agent behind the event. Present only when VisitorKind is
	// VisitorKindAIBot, nil otherwise.
	AI *AIAgent `json:"ai,omitempty"`

	// SuspectScore is nil when the request was not scored. Never read nil as 0.
	SuspectScore *int `json:"suspect_score"`
	// SuspectLevel is SuspectLevelLow, SuspectLevelMedium or
	// SuspectLevelHigh. Empty when not scored.
	SuspectLevel SuspectLevel `json:"suspect_level,omitempty"`

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

// AIAgent is the AI agent behind an event whose VisitorKind is
// VisitorKindAIBot: who operates it, what its traffic is for and how far its
// claim was checked. The identity comes from the user agent, which any client
// can copy, and is not verified yet.
type AIAgent struct {
	// Name is the agent as its operator names it, such as ChatGPT-User.
	Name string `json:"name"`
	// Provider is the company that operates it, such as OpenAI. An open
	// string, not a closed set.
	Provider string `json:"provider"`
	// Purpose is AIPurposeTraining, AIPurposeSearch, AIPurposeUserFetch or
	// AIPurposeAgent.
	Purpose AIPurpose `json:"purpose"`
	// Runtime is AIRuntimeProviderCloud, AIRuntimeHostedBrowser or
	// AIRuntimeUserDevice.
	Runtime AIRuntime `json:"runtime"`
	// Declared is true when the agent named itself, in its user agent or a
	// signature.
	Declared bool `json:"declared"`
	// Verification is how the claimed identity was checked, and what that
	// found.
	Verification AIVerification `json:"verification"`
}

// AIVerification is how an AI agent's claimed identity was checked, and what
// that found.
type AIVerification struct {
	// Method is AIVerificationSignature, AIVerificationIPList,
	// AIVerificationReverseDNS or AIVerificationNone.
	Method AIVerificationMethod `json:"method"`
	// Result is AIVerificationPass, AIVerificationFail or
	// AIVerificationNotAttempted.
	Result AIVerificationResult `json:"result"`
}

// Identifiable reports whether the event resolved to a visitor identifier.
func (e *Event) Identifiable() bool { return e != nil && e.VisitorID != "" }

// ParseDocument decodes Event.Document into a typed EventDocument when present.
func (e *Event) ParseDocument() (*EventDocument, error) {
	if e == nil || len(e.Document) == 0 || string(e.Document) == "null" {
		return nil, nil
	}
	var doc EventDocument
	if err := json.Unmarshal(e.Document, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
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
	// Level restricts it to SuspectLevelLow, SuspectLevelMedium or
	// SuspectLevelHigh.
	Level SuspectLevel
	// Kind restricts it to one visitor kind, such as VisitorKindAIBot.
	Kind VisitorKind
	// AIProvider restricts it to one AI agent operator, exactly as events
	// name it (case sensitive), such as OpenAI.
	AIProvider string
	// AIPurpose restricts it to one AI agent purpose, such as
	// AIPurposeUserFetch.
	AIPurpose AIPurpose
}
