package fingerly

// The closed string sets the API reports, as named types.
//
// Each type is a string, so a value decoded from the API is kept exactly as it
// arrived: one this build has no constant for, because the server added it
// later, decodes and compares like any other. Compare against the constants
// rather than against string literals:
//
//	if event.SuspectLevel == fingerly.SuspectLevelHigh { /* ... */ }
//
// An untyped literal still compiles against these types; a variable of type
// string needs a conversion, such as fingerly.Signal(name).

// Signal is a signal group or raw detector identifier. An event's Triggers
// carry signal groups, the names a console shows, such as SignalVPN; an
// archived risk report's Triggers carry the raw detectors behind them, such as
// SignalVPNPublic. Some names, such as SignalDeviceFarm, are both.
type Signal string

// Signal groups, reported on Event.Triggers.
const (
	SignalTor                   Signal = "tor"
	SignalDatacenterProxy       Signal = "datacenter_proxy"
	SignalResidentialProxy      Signal = "residential_proxy"
	SignalVPN                   Signal = "vpn"
	SignalIPBlocklist           Signal = "ip_blocklist"
	SignalIPReputation          Signal = "ip_reputation"
	SignalNetworkAnomaly        Signal = "network_anomaly"
	SignalLocationSpoofing      Signal = "location_spoofing"
	SignalIOSSimulator          Signal = "ios_simulator"
	SignalAndroidEmulator       Signal = "android_emulator"
	SignalVirtualMachine        Signal = "virtual_machine"
	SignalJailbrokenDevice      Signal = "jailbroken_device"
	SignalRootedDevice          Signal = "rooted_device"
	SignalClonedApp             Signal = "cloned_app"
	SignalDeviceFarm            Signal = "device_farm"
	SignalMITMAttack            Signal = "mitm_attack"
	SignalFridaDetected         Signal = "frida_detected"
	SignalBrowserTampering      Signal = "browser_tampering"
	SignalFingerprintSuppressed Signal = "fingerprint_suppressed"
	SignalIdentityObfuscated    Signal = "identity_obfuscated"
	SignalDeveloperTools        Signal = "developer_tools"
	SignalRemoteControl         Signal = "remote_control"
	SignalBot                   Signal = "bot"
	SignalHighActivity          Signal = "high_activity"
	SignalIncognitoMode         Signal = "incognito_mode"
	SignalPrivacySettings       Signal = "privacy_settings"
	SignalEmbeddedWebView       Signal = "embedded_web_view"
	SignalActiveCall            Signal = "active_call"
	SignalAttestationMissing    Signal = "attestation_missing"
	SignalAttestationFailed     Signal = "attestation_failed"
	SignalDeviceInconsistent    Signal = "device_inconsistent"
	SignalReportIncomplete      Signal = "report_incomplete"
	SignalCustomRootCA          Signal = "custom_root_ca"
	SignalSecondaryUserProfile  Signal = "secondary_user_profile"
)

// Raw detectors, reported on an archived EventRiskReport. A group above whose
// name is also a detector, such as SignalTor or SignalDeviceFarm, is not
// repeated here.
const (
	SignalProxy                            Signal = "proxy"
	SignalDatacenterIP                     Signal = "datacenter_ip"
	SignalResidentialProxyLowConfidence    Signal = "residential_proxy_low_confidence"
	SignalResidentialProxyMediumConfidence Signal = "residential_proxy_medium_confidence"
	SignalResidentialProxyHighConfidence   Signal = "residential_proxy_high_confidence"
	SignalVPNPublic                        Signal = "vpn_public"
	SignalVPNRelay                         Signal = "vpn_relay"
	SignalVPNMobile                        Signal = "vpn_mobile"
	SignalVPNOSMismatch                    Signal = "vpn_os_mismatch"
	SignalVPNLowConfidence                 Signal = "vpn_low_confidence"
	SignalVPNMediumConfidence              Signal = "vpn_medium_confidence"
	SignalVPNHighConfidence                Signal = "vpn_high_confidence"
	SignalIPBlocklistEmailSpam             Signal = "ip_blocklist_email_spam"
	SignalIPBlocklistAttackSource          Signal = "ip_blocklist_attack_source"
	SignalIPAbuser                         Signal = "ip_abuser"
	SignalBogonIP                          Signal = "bogon_ip"
	SignalAnycastIP                        Signal = "anycast_ip"
	SignalGeoMismatch                      Signal = "geo_mismatch"
	SignalSimulator                        Signal = "simulator"
	SignalEmulator                         Signal = "emulator"
	SignalJailbreak                        Signal = "jailbreak"
	SignalRootApps                         Signal = "root_apps"
	SignalPlayDeviceIntegrityFailed        Signal = "play_device_integrity_failed"
	SignalAppCloner                        Signal = "app_cloner"
	SignalPlayAppUnrecognized              Signal = "play_app_unrecognized"
	SignalRepeatedReinstall                Signal = "repeated_reinstall"
	SignalDeviceActivityHigh               Signal = "device_activity_high"
	SignalDeviceActivityVeryHigh           Signal = "device_activity_very_high"
	SignalFrida                            Signal = "frida"
	SignalTampering                        Signal = "tampering"
	SignalAntidetectBrowser                Signal = "antidetect_browser"
	SignalAutomation                       Signal = "automation"
	SignalBadBot                           Signal = "bad_bot"
	SignalAISpoofed                        Signal = "ai_spoofed"
	SignalFetchMetadataMissing             Signal = "fetch_metadata_missing"
	SignalWebTicketMissing                 Signal = "web_ticket_missing"
	SignalWebTicketInvalid                 Signal = "web_ticket_invalid"
	SignalIncognito                        Signal = "incognito"
	SignalAttestationDowngrade             Signal = "attestation_downgrade"
	SignalProbeErrors                      Signal = "probe_errors"
)

// SuspectLevel is the band a suspect score falls into.
type SuspectLevel string

const (
	SuspectLevelLow    SuspectLevel = "low"
	SuspectLevelMedium SuspectLevel = "medium"
	SuspectLevelHigh   SuspectLevel = "high"
)

// Confidence is how sure one detection is.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// AttestationState is the server's single verdict on a native event's
// hardware evidence.
type AttestationState string

const (
	AttestationStateAttested    AttestationState = "attested"
	AttestationStateNotAttested AttestationState = "not_attested"
	AttestationStateFailed      AttestationState = "failed"
)

// AttestationStatus is the detail behind an AttestationState.
type AttestationStatus string

const (
	AttestationStatusVerified      AttestationStatus = "verified"
	AttestationStatusIndeterminate AttestationStatus = "indeterminate"
	AttestationStatusInvalid       AttestationStatus = "invalid"
	AttestationStatusAbsent        AttestationStatus = "absent"
	AttestationStatusUnconfigured  AttestationStatus = "unconfigured"
)

// AttestSkippedReason is the app's own reason for sending no hardware
// attestation. Diagnostic only: it never lowers the score.
type AttestSkippedReason string

const (
	// AttestSkippedNotOffered means the challenge offered no attestation: on iOS
	// the SDK key has no Apple app ID for App Attest, on Android the app's
	// transport returned no challenge offer.
	AttestSkippedNotOffered AttestSkippedReason = "not_offered"
	// AttestSkippedUnsupported means the device cannot make an attested key: App
	// Attest is not supported on the iOS device, or Android is older than 7.0
	// (API 24).
	AttestSkippedUnsupported AttestSkippedReason = "unsupported"
	// AttestSkippedChallengeUnavailable means there was no usable challenge
	// ticket: the challenge request failed, or on Android the ticket it
	// returned was malformed.
	AttestSkippedChallengeUnavailable AttestSkippedReason = "challenge_unavailable"
	// AttestSkippedKeyError means the platform key store failed to create, attest
	// or sign with the key, or on Android the stored key record could not be
	// read.
	AttestSkippedKeyError AttestSkippedReason = "key_error"
	// AttestSkippedTimeout means attestation did not finish in time: a key store
	// call ran past its budget, or another identification held the
	// attestation step too long.
	AttestSkippedTimeout AttestSkippedReason = "timeout"
	// AttestSkippedBackoff means the server rejected a new key from this install
	// (on iOS, also when it asked for a replacement), and the SDK is waiting
	// out a backoff before it registers another.
	AttestSkippedBackoff AttestSkippedReason = "backoff"
	// AttestSkippedInstallUnavailable means the report has no usable install ID,
	// or on iOS the stored key record could not be read or saved.
	AttestSkippedInstallUnavailable AttestSkippedReason = "install_unavailable"
)

// EventState says whether an event was enriched.
type EventState string

const (
	// EventStatePending is accepted and not written yet. A read answers it
	// with ErrEventPending rather than an event, so a read Event is never in
	// it.
	EventStatePending EventState = "pending"
	// EventStateEnriched is an event whose network lookup ran.
	EventStateEnriched EventState = "enriched"
	// EventStateUnavailable is an event whose network lookup could not run;
	// Event.Reason says why.
	EventStateUnavailable EventState = "unavailable"
)

// AnonymityNetwork is the kind of network that hid the visitor's address. An
// ordinary consumer connection has none, which Event reports as "".
type AnonymityNetwork string

const (
	AnonymityNetworkTor     AnonymityNetwork = "tor"
	AnonymityNetworkRelay   AnonymityNetwork = "relay"
	AnonymityNetworkVPN     AnonymityNetwork = "vpn"
	AnonymityNetworkProxy   AnonymityNetwork = "proxy"
	AnonymityNetworkHosting AnonymityNetwork = "hosting"
)

// VisitorKind is what sort of client made a request.
type VisitorKind string

const (
	VisitorKindHuman         VisitorKind = "human"
	VisitorKindAIBot         VisitorKind = "ai_bot"
	VisitorKindAutomation    VisitorKind = "automation"
	VisitorKindSearchCrawler VisitorKind = "search_crawler"
	// VisitorKindUnknown is only in an agent sightings answer: a request the
	// server could not use; TTL 0, never remembered. An event never carries
	// it.
	VisitorKindUnknown VisitorKind = "unknown"
)

// AIPurpose is what an AI agent's operator says its traffic is for.
type AIPurpose string

const (
	// AIPurposeTraining is collecting content to train models.
	AIPurposeTraining AIPurpose = "training"
	// AIPurposeSearch is indexing for an AI answer or search product.
	AIPurposeSearch AIPurpose = "search"
	// AIPurposeUserFetch is reading a page a person asked an assistant to read.
	AIPurposeUserFetch AIPurpose = "user_fetch"
	// AIPurposeAgent is driving a browser on a person's behalf.
	AIPurposeAgent AIPurpose = "agent"
)

// AIRuntime is where an AI agent runs.
type AIRuntime string

const (
	// AIRuntimeProviderCloud is the operator's own infrastructure.
	AIRuntimeProviderCloud AIRuntime = "provider_cloud"
	// AIRuntimeHostedBrowser is a third party's cloud browser.
	AIRuntimeHostedBrowser AIRuntime = "hosted_browser"
	// AIRuntimeUserDevice is a person's own browser.
	AIRuntimeUserDevice AIRuntime = "user_device"
)

// AIVerificationMethod is how an AI agent's claimed identity was checked.
type AIVerificationMethod string

const (
	// AIVerificationSignature is a signed request.
	AIVerificationSignature AIVerificationMethod = "signature"
	// AIVerificationIPList is the operator's published address ranges.
	AIVerificationIPList AIVerificationMethod = "ip_list"
	// AIVerificationReverseDNS is a reverse lookup confirmed forward.
	AIVerificationReverseDNS AIVerificationMethod = "reverse_dns"
	// AIVerificationNone is no check at all.
	AIVerificationNone AIVerificationMethod = "none"
)

// AIVerificationResult is what checking an AI agent's claimed identity found.
type AIVerificationResult string

const (
	// AIVerificationPass means the claim held.
	AIVerificationPass AIVerificationResult = "pass"
	// AIVerificationFail means it was checked and did not hold.
	AIVerificationFail AIVerificationResult = "fail"
	// AIVerificationNotAttempted means no check ran.
	AIVerificationNotAttempted AIVerificationResult = "not_attempted"
)

// Environment is the deployment an event was recorded in, set by the key
// that submitted it.
type Environment string

const (
	EnvironmentProduction  Environment = "production"
	EnvironmentStaging     Environment = "staging"
	EnvironmentDevelopment Environment = "development"
)

// RiskPlatform is the scoring profile an event was scored under. It is
// coarser than the SDK platform: a React Native app on iOS is scored as
// RiskPlatformIOS.
type RiskPlatform string

const (
	RiskPlatformWeb     RiskPlatform = "web"
	RiskPlatformIOS     RiskPlatform = "ios"
	RiskPlatformAndroid RiskPlatform = "android"
)

// RiskSource says which risk profile scored an event.
type RiskSource string

const (
	// RiskSourceKey is the SDK key's own profile.
	RiskSourceKey RiskSource = "key"
	// RiskSourceOrganization is the organization's profile.
	RiskSourceOrganization RiskSource = "organization"
	// RiskSourceDefault is the weights the platform ships with.
	RiskSourceDefault RiskSource = "default"
)

// WebhookEventType is the Type of a webhook delivery.
type WebhookEventType string

const (
	WebhookIdentificationCompleted WebhookEventType = "identification.completed"
	WebhookIdentificationRefused   WebhookEventType = "identification.refused"
	WebhookVisitorSuspect          WebhookEventType = "visitor.suspect"
	WebhookBillingStatusChanged    WebhookEventType = "billing.status_changed"
	WebhookUsageDailySettled       WebhookEventType = "usage.daily_settled"
	// WebhookTest is sent when a test delivery is requested for one endpoint.
	// Nobody subscribes to it.
	WebhookTest WebhookEventType = "webhook.test"
)

// WebhookEnvironment is the Environment of a webhook delivery: live for
// production keys, test for every other environment.
type WebhookEnvironment string

const (
	WebhookEnvironmentLive WebhookEnvironment = "live"
	WebhookEnvironmentTest WebhookEnvironment = "test"
)
