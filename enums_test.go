package fingerly

import (
	"encoding/json"
	"testing"
)

// The wire values are the server's own and permanent: a stored weight map and
// every customer's comparisons are keyed on them.
func TestEnumWireValues(t *testing.T) {
	cases := map[string]string{
		// Signal groups.
		string(SignalTor):                   "tor",
		string(SignalDatacenterProxy):       "datacenter_proxy",
		string(SignalResidentialProxy):      "residential_proxy",
		string(SignalVPN):                   "vpn",
		string(SignalIPBlocklist):           "ip_blocklist",
		string(SignalIPReputation):          "ip_reputation",
		string(SignalNetworkAnomaly):        "network_anomaly",
		string(SignalLocationSpoofing):      "location_spoofing",
		string(SignalIOSSimulator):          "ios_simulator",
		string(SignalAndroidEmulator):       "android_emulator",
		string(SignalVirtualMachine):        "virtual_machine",
		string(SignalJailbrokenDevice):      "jailbroken_device",
		string(SignalRootedDevice):          "rooted_device",
		string(SignalClonedApp):             "cloned_app",
		string(SignalDeviceFarm):            "device_farm",
		string(SignalMITMAttack):            "mitm_attack",
		string(SignalFridaDetected):         "frida_detected",
		string(SignalBrowserTampering):      "browser_tampering",
		string(SignalFingerprintSuppressed): "fingerprint_suppressed",
		string(SignalDeveloperTools):        "developer_tools",
		string(SignalRemoteControl):         "remote_control",
		string(SignalBot):                   "bot",
		string(SignalHighActivity):          "high_activity",
		string(SignalIncognitoMode):         "incognito_mode",
		string(SignalPrivacySettings):       "privacy_settings",
		string(SignalEmbeddedWebView):       "embedded_web_view",
		string(SignalActiveCall):            "active_call",
		string(SignalAttestationMissing):    "attestation_missing",
		string(SignalAttestationFailed):     "attestation_failed",
		string(SignalDeviceInconsistent):    "device_inconsistent",
		string(SignalReportIncomplete):      "report_incomplete",
		string(SignalCustomRootCA):          "custom_root_ca",
		string(SignalSecondaryUserProfile):  "secondary_user_profile",
		// Raw detectors.
		string(SignalProxy):                            "proxy",
		string(SignalDatacenterIP):                     "datacenter_ip",
		string(SignalResidentialProxyLowConfidence):    "residential_proxy_low_confidence",
		string(SignalResidentialProxyMediumConfidence): "residential_proxy_medium_confidence",
		string(SignalResidentialProxyHighConfidence):   "residential_proxy_high_confidence",
		string(SignalVPNPublic):                        "vpn_public",
		string(SignalVPNTimezone):                      "vpn_timezone",
		string(SignalVPNRelay):                         "vpn_relay",
		string(SignalVPNMobile):                        "vpn_mobile",
		string(SignalVPNOSMismatch):                    "vpn_os_mismatch",
		string(SignalVPNLowConfidence):                 "vpn_low_confidence",
		string(SignalVPNMediumConfidence):              "vpn_medium_confidence",
		string(SignalVPNHighConfidence):                "vpn_high_confidence",
		string(SignalIPBlocklistEmailSpam):             "ip_blocklist_email_spam",
		string(SignalIPBlocklistAttackSource):          "ip_blocklist_attack_source",
		string(SignalIPAbuser):                         "ip_abuser",
		string(SignalBogonIP):                          "bogon_ip",
		string(SignalAnycastIP):                        "anycast_ip",
		string(SignalGeoMismatch):                      "geo_mismatch",
		string(SignalSimulator):                        "simulator",
		string(SignalEmulator):                         "emulator",
		string(SignalJailbreak):                        "jailbreak",
		string(SignalRootApps):                         "root_apps",
		string(SignalPlayDeviceIntegrityFailed):        "play_device_integrity_failed",
		string(SignalAppCloner):                        "app_cloner",
		string(SignalPlayAppUnrecognized):              "play_app_unrecognized",
		string(SignalRepeatedReinstall):                "repeated_reinstall",
		string(SignalDeviceActivityHigh):               "device_activity_high",
		string(SignalDeviceActivityVeryHigh):           "device_activity_very_high",
		string(SignalFrida):                            "frida",
		string(SignalTampering):                        "tampering",
		string(SignalAntidetectBrowser):                "antidetect_browser",
		string(SignalAutomation):                       "automation",
		string(SignalBadBot):                           "bad_bot",
		string(SignalFetchMetadataMissing):             "fetch_metadata_missing",
		string(SignalWebTicketMissing):                 "web_ticket_missing",
		string(SignalWebTicketInvalid):                 "web_ticket_invalid",
		string(SignalIncognito):                        "incognito",
		string(SignalAttestationDowngrade):             "attestation_downgrade",
		string(SignalProbeErrors):                      "probe_errors",
	}
	if len(cases) != 73 {
		t.Errorf("%d distinct signal values, want 73: a constant duplicates another", len(cases))
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("signal constant %q, want %q", got, want)
		}
	}

	others := []struct{ got, want string }{
		{string(SuspectLevelLow), "low"},
		{string(SuspectLevelMedium), "medium"},
		{string(SuspectLevelHigh), "high"},
		{string(ConfidenceLow), "low"},
		{string(ConfidenceMedium), "medium"},
		{string(ConfidenceHigh), "high"},
		{string(AttestationStateAttested), "attested"},
		{string(AttestationStateNotAttested), "not_attested"},
		{string(AttestationStateFailed), "failed"},
		{string(AttestationStatusVerified), "verified"},
		{string(AttestationStatusIndeterminate), "indeterminate"},
		{string(AttestationStatusInvalid), "invalid"},
		{string(AttestationStatusAbsent), "absent"},
		{string(AttestationStatusUnconfigured), "unconfigured"},
		{string(AttestSkippedNotOffered), "not_offered"},
		{string(AttestSkippedUnsupported), "unsupported"},
		{string(AttestSkippedChallengeUnavailable), "challenge_unavailable"},
		{string(AttestSkippedKeyError), "key_error"},
		{string(AttestSkippedTimeout), "timeout"},
		{string(AttestSkippedBackoff), "backoff"},
		{string(AttestSkippedInstallUnavailable), "install_unavailable"},
		{string(EventStatePending), "pending"},
		{string(EventStateEnriched), "enriched"},
		{string(EventStateUnavailable), "unavailable"},
		{string(AnonymityNetworkTor), "tor"},
		{string(AnonymityNetworkRelay), "relay"},
		{string(AnonymityNetworkVPN), "vpn"},
		{string(AnonymityNetworkProxy), "proxy"},
		{string(AnonymityNetworkHosting), "hosting"},
		{string(EnvironmentProduction), "production"},
		{string(EnvironmentStaging), "staging"},
		{string(EnvironmentDevelopment), "development"},
		{string(RiskPlatformWeb), "web"},
		{string(RiskPlatformIOS), "ios"},
		{string(RiskPlatformAndroid), "android"},
		{string(RiskSourceKey), "key"},
		{string(RiskSourceOrganization), "organization"},
		{string(RiskSourceDefault), "default"},
		{string(VerifyReasonOK), ""},
		{string(VerifyNotFound), "not_found"},
		{string(VerifyTooOld), "too_old"},
		{string(VerifyTagMismatch), "tag_mismatch"},
		{string(VerifyUnidentifiable), "unidentifiable"},
		{string(VerifyUnscored), "unscored"},
		{string(VerifyAlreadyConsumed), "already_consumed"},
		{string(WebhookIdentificationCompleted), "identification.completed"},
		{string(WebhookIdentificationRefused), "identification.refused"},
		{string(WebhookVisitorSuspect), "visitor.suspect"},
		{string(WebhookBillingStatusChanged), "billing.status_changed"},
		{string(WebhookUsageDailySettled), "usage.daily_settled"},
		{string(WebhookTest), "webhook.test"},
		{string(WebhookEnvironmentLive), "live"},
		{string(WebhookEnvironmentTest), "test"},
	}
	for _, c := range others {
		if c.got != c.want {
			t.Errorf("constant %q, want %q", c.got, c.want)
		}
	}
}

// The typed fields decode the wire values into the constants, and keep a
// value this build has no constant for exactly as it arrived.
func TestEnumFieldsDecode(t *testing.T) {
	var event Event
	body := `{
		"request_id": "r", "visitor_id": "v", "occurred_at": "2026-09-17T12:00:00Z",
		"state": "enriched", "environment": "production", "platform": "ios",
		"sdk_platform": "react-native-ios", "sdk_version": "0.3.0",
		"anonymity_network": "relay", "suspect_score": 9, "suspect_level": "high",
		"triggers": [
			{"signal": "fingerprint_suppressed", "weight": 5, "confidence": "medium"},
			{"signal": "signal_from_the_future", "weight": 1, "confidence": "certain"}
		],
		"has_document": false
	}`
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if event.State != EventStateEnriched || event.Environment != EnvironmentProduction ||
		event.Platform != RiskPlatformIOS || event.AnonymityNetwork != AnonymityNetworkRelay ||
		event.SuspectLevel != SuspectLevelHigh {
		t.Errorf("unexpected event fields: %+v", event)
	}
	if event.Triggers[0].Signal != SignalFingerprintSuppressed || event.Triggers[0].Confidence != ConfidenceMedium {
		t.Errorf("trigger 0 = %+v", event.Triggers[0])
	}
	if event.Triggers[1].Signal != "signal_from_the_future" || event.Triggers[1].Confidence != "certain" {
		t.Errorf("unknown values were not kept: %+v", event.Triggers[1])
	}

	var envelope WebhookEvent
	if err := json.Unmarshal([]byte(`{"type":"visitor.suspect","environment":"test"}`), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Type != WebhookVisitorSuspect || envelope.Environment != WebhookEnvironmentTest {
		t.Errorf("envelope = %+v", envelope)
	}

	var report EventRiskReport
	if err := json.Unmarshal([]byte(`{"platform":"web","level":"low","source":"organization","triggers":[{"signal":"vpn_public","weight":6}]}`), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Platform != RiskPlatformWeb || report.Level != SuspectLevelLow || report.Source != RiskSourceOrganization ||
		report.Triggers[0].Signal != SignalVPNPublic {
		t.Errorf("report = %+v", report)
	}
}
