package handlers

import (
	"testing"

	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// TestStatusMapping unit-tests mapBytesError directly (white-box, package
// handlers) for the P2-M4.1 read-path sentinels: a staging-hidden blob and a
// not-found blob both map to 404 (no existence leak), a committed-but-momentarily
// -unsourceable blob maps to 503, and anything else falls through to 500.
func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"staging not visible", storage.ErrStagingNotVisible, 404},
		{"no sourceable holder", storage.ErrNoSourceableHolder, 503},
		{"blob not found", storage.ErrBlobNotFound, 404},
		{"default", errUnmapped{}, 500},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got, _, _ := mapBytesError(c.err)
			if got != c.want {
				t.Fatalf("mapBytesError(%v) status = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

// errUnmapped is a sentinel-free error to exercise the default branch.
type errUnmapped struct{}

func (errUnmapped) Error() string { return "some unmapped storage error" }
