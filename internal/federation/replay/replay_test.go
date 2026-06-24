package replay_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/replay"
)

func TestReplayUseOnce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	exp := now.Add(5 * time.Minute)

	cases := []struct {
		name      string
		setup     func(c *replay.Cache) // pre-seed calls before the probe
		jti       string
		exp       time.Time
		now       time.Time
		wantFirst bool // expected result of the FIRST (or only) UseOnce call
	}{
		{
			name:      "fresh_jti_accepted",
			jti:       "j-fresh",
			exp:       exp,
			now:       now,
			wantFirst: true,
		},
		{
			name: "same_jti_rejected",
			setup: func(c *replay.Cache) {
				c.UseOnce("j-dup", exp, now)
			},
			jti:       "j-dup",
			exp:       exp,
			now:       now,
			wantFirst: false, // already used in setup
		},
		{
			name: "expired_entry_swept_then_reused",
			// Insert a token whose expiry is BEFORE now, so it is swept
			// and a second token with the same jti is admitted.
			setup: func(c *replay.Cache) {
				pastExp := now.Add(-1 * time.Second)
				c.UseOnce("j-swept", pastExp, now.Add(-2*time.Second))
			},
			jti:       "j-swept",
			exp:       exp,
			now:       now,   // now is AFTER pastExp → entry was swept
			wantFirst: true,  // re-admitted after sweep
		},
		{
			name: "different_jtis_independent",
			setup: func(c *replay.Cache) {
				c.UseOnce("j-a", exp, now)
			},
			jti:       "j-b",
			exp:       exp,
			now:       now,
			wantFirst: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := replay.New()
			if tc.setup != nil {
				tc.setup(c)
			}
			got := c.UseOnce(tc.jti, tc.exp, tc.now)
			if got != tc.wantFirst {
				t.Errorf("UseOnce(%q) = %v, want %v", tc.jti, got, tc.wantFirst)
			}
		})
	}

	t.Run("second_call_always_false", func(t *testing.T) {
		c := replay.New()
		if !c.UseOnce("j-once", exp, now) {
			t.Fatal("first call should return true")
		}
		if c.UseOnce("j-once", exp, now) {
			t.Fatal("second call should return false (replay)")
		}
	})
}

func TestBootFloorOK(t *testing.T) {
	boot := time.Unix(1_000_000, 0)

	cases := []struct {
		name      string
		notBefore int64
		bootTime  time.Time
		want      bool
	}{
		{name: "exactly_at_floor", notBefore: boot.Unix(), bootTime: boot, want: true},
		{name: "after_floor", notBefore: boot.Unix() + 1, bootTime: boot, want: true},
		{name: "before_floor", notBefore: boot.Unix() - 1, bootTime: boot, want: false},
		{name: "far_in_future", notBefore: boot.Unix() + 3600, bootTime: boot, want: true},
		{name: "far_in_past", notBefore: boot.Unix() - 3600, bootTime: boot, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := replay.BootFloorOK(tc.notBefore, tc.bootTime)
			if got != tc.want {
				t.Errorf("BootFloorOK(%d, %v) = %v, want %v", tc.notBefore, tc.bootTime, got, tc.want)
			}
		})
	}
}
