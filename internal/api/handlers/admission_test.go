package handlers

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAdmissionGlobal: acquiring 2 different creds against a global=2 admission
// succeeds; a 3rd (any cred) → global full → false. Release one → next succeeds.
func TestAdmissionGlobal(t *testing.T) {
	t.Parallel()
	a := newAdmission(2, 0) // global=2, per-cred unbounded

	rel1, ok := a.TryAcquire("cred-a")
	require.True(t, ok, "first acquire must succeed")

	rel2, ok := a.TryAcquire("cred-b")
	require.True(t, ok, "second acquire (different cred) must succeed")

	_, ok = a.TryAcquire("cred-c")
	require.False(t, ok, "third acquire must fail: global cap=2 exhausted")

	rel1() // free one global slot

	rel3, ok := a.TryAcquire("cred-c")
	require.True(t, ok, "fourth acquire must succeed after a slot is freed")
	rel2()
	rel3()
}

// TestAdmissionPerCredential: per-cred cap=1; second acquire for same cred fails
// but does NOT consume a global slot (a different cred can still acquire).
func TestAdmissionPerCredential(t *testing.T) {
	t.Parallel()
	a := newAdmission(10, 1) // global=10 (plenty), per-cred=1

	relA, ok := a.TryAcquire("a")
	require.True(t, ok, "first acquire for 'a' must succeed")

	_, ok = a.TryAcquire("a")
	require.False(t, ok, "second acquire for 'a' must fail (per-cred cap)")

	// Confirm global slot was NOT consumed by the failed per-cred attempt:
	// fill up global (10 total; 1 held by "a") with 9 different creds.
	var rels []func()
	for i := 0; i < 9; i++ {
		r, ok := a.TryAcquire(fmt.Sprintf("other-%d", i))
		require.True(t, ok, "different cred must still acquire (global not saturated)")
		rels = append(rels, r)
	}
	// Now global is full (10/10); one more must fail.
	_, ok = a.TryAcquire("b")
	require.False(t, ok, "global must now be full")

	// Release "a" → "a" can acquire again.
	relA()
	relA2, ok := a.TryAcquire("a")
	require.True(t, ok, "after releasing 'a', should acquire again")
	relA2()
	for _, r := range rels {
		r()
	}
}

// TestAdmissionReleaseIdempotent: calling the release func twice must not
// double-free. After a double-release, acquire+release until capacity confirms
// the slot counts are correct.
func TestAdmissionReleaseIdempotent(t *testing.T) {
	t.Parallel()
	a := newAdmission(1, 1)

	rel, ok := a.TryAcquire("x")
	require.True(t, ok)

	rel() // first release
	rel() // second release — must be a no-op

	// After double-release, the global slot must be available exactly once.
	rel2, ok := a.TryAcquire("x")
	require.True(t, ok, "slot must be available after idempotent release")

	_, ok = a.TryAcquire("y")
	require.False(t, ok, "global must be full (only 1 slot, held by x)")

	rel2()
}

// TestAdmissionUnbounded: newAdmission(0,0) — both dimensions disabled; many
// acquires all succeed.
func TestAdmissionUnbounded(t *testing.T) {
	t.Parallel()
	a := newAdmission(0, 0)

	var rels []func()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, ok := a.TryAcquire("anyone")
			require.True(t, ok, "unbounded admission must always succeed")
			mu.Lock()
			rels = append(rels, r)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, r := range rels {
		r()
	}
}
