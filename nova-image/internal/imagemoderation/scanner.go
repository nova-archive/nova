// Package imagemoderation is the nova-image moderation scanner. Phase 1 is
// pass-through (manual moderation only): every upload is allowed. Phase 3 adds
// the Go-native pHash near-duplicate signal; Phase 4 adds PDQ matching against
// StopNCII/NCMEC. See the M5 spec.
package imagemoderation

import (
	"context"

	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Scanner runs synchronous moderation checks on upload plaintext.
type Scanner struct{}

func New() *Scanner { return &Scanner{} }

// Scan returns the moderation verdict. Phase 1: always allow.
func (Scanner) Scan(ctx context.Context, plaintext []byte) storage.ScanResult {
	return storage.ScanResult{Action: storage.ActionAllow}
}
