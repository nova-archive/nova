package storage

// ScanAction is the moderation verdict a product returns from analysis.
type ScanAction string

const (
	ActionAllow      ScanAction = "allow"
	ActionQuarantine ScanAction = "quarantine"
	ActionTombstone  ScanAction = "tombstone"
)

// ScanResult is a product's synchronous moderation verdict on a plaintext
// upload. Phase 1 nova-image always returns ActionAllow (manual moderation).
type ScanResult struct {
	Action  ScanAction
	Rule    string // e.g. "pdq_match" (Phase 3/4)
	RuleRef string // blocklist entry id, etc.
	Notes   string
}
