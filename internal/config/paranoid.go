package config

import "fmt"

// ApplyPrivacyPreset resolves the privacy preset selected by auth.paranoid and
// returns any consequence warnings.
//
// paranoid is a *preset*, not a hard override: when true it sets protective
// DEFAULTS for the privacy-relevant settings, but any value the operator set
// explicitly WINS. Relaxing a protective default only produces a warning — never
// a silent override, and never a refusal to start. (Legal/safety floors such as
// T1.20 public_uploads→tos_url are enforced in validate(), not here, and DO
// refuse to start; they are not privacy preferences.) See
// docs/PRIVACY_AUDIT.md § "paranoid: true mode".
//
// Called automatically from the loader after validate(); operators don't invoke
// it directly. The returned warnings are stashed on the Config (PrivacyWarnings)
// for the coordinator to log at startup and the admin UI to surface inline.
func ApplyPrivacyPreset(cfg *Config) []string {
	if !cfg.Auth.Paranoid {
		// Default posture (paranoid off): every privacy setting keeps its
		// configured / default value. Nothing to warn about.
		return nil
	}

	var warnings []string

	// Outbound webhooks — preset default is "none". Do not nuke an explicit
	// configuration; warn that egress remains active so the choice is informed.
	if len(cfg.Webhooks) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"paranoid mode is on but %d webhook destination(s) are configured; "+
				"outbound webhook egress remains ACTIVE — clear `webhooks` to suppress it",
			len(cfg.Webhooks)))
	}

	// Source-IP retention — preset default is 1 day. Fill when unset; keep an
	// explicit longer window but warn that it exceeds the hardened default.
	switch {
	case cfg.SourceIPRetentionDays == 0:
		cfg.SourceIPRetentionDays = 1
	case cfg.SourceIPRetentionDays > 1:
		warnings = append(warnings, fmt.Sprintf(
			"paranoid mode is on but source_ip_retention_days=%d exceeds the hardened "+
				"1-day default; keeping your value",
			cfg.SourceIPRetentionDays))
	}

	// Source-IP recording — preset default is off. Fill the effective value when
	// unset; keep an explicit `true` but warn that IPs will be recorded.
	if cfg.Coordinator.RecordSourceIP == nil {
		off := false
		cfg.Coordinator.RecordSourceIP = &off
	} else if *cfg.Coordinator.RecordSourceIP {
		warnings = append(warnings,
			"paranoid mode is on but coordinator.record_source_ip=true; "+
				"client source IPs WILL be recorded with each blob")
	}

	// Public IPFS DHT — preset expects private participation. Warn if the
	// operator opted into the public DHT (it advertises pinned CIDs publicly).
	if cfg.Coordinator.PublicIpfsDht {
		warnings = append(warnings,
			"paranoid mode is on but coordinator.public_ipfs_dht=true; "+
				"pinned CIDs will be advertised to the public IPFS DHT")
	}

	return warnings
}

// PrivacyWarnings returns the consequence warnings produced by
// ApplyPrivacyPreset at load time. Empty in the default posture (paranoid off)
// and whenever paranoid is on without any relaxed protective default.
func (c *Config) PrivacyWarnings() []string { return c.privacyWarnings }
