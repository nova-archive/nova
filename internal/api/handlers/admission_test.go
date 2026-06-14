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
	a := newAdmission(func() (int, int) { return 2, 0 }) // global=2, per-cred unbounded

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
	a := newAdmission(func() (int, int) { return 10, 1 }) // global=10 (plenty), per-cred=1

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
	a := newAdmission(func() (int, int) { return 1, 1 })

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
	a := newAdmission(func() (int, int) { return 0, 0 })

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

func TestAdmissionHonorsLiveLoweredLimit(t *testing.T) {
	limit := 2
	adm := newAdmission(func() (int, int) { return limit, 0 }) // global=limit, per-cred unbounded
	r1, ok := adm.TryAcquire("c")
	require.True(t, ok)
	_, ok = adm.TryAcquire("c")
	require.True(t, ok)
	_, ok = adm.TryAcquire("c")
	require.False(t, ok) // at global=2
	r1()
	_, ok = adm.TryAcquire("c")
	require.True(t, ok) // slot freed
}

func TestAdmissionPerCredAndGlobalRollback(t *testing.T) {
	adm := newAdmission(func() (int, int) { return 10, 1 }) // global=10, per-cred=1
	r1, ok := adm.TryAcquire("c")
	require.True(t, ok)
	_, ok = adm.TryAcquire("c")
	require.False(t, ok) // per-cred cap; global must NOT be leaked
	// a different credential still admits (global slot was not taken on the failed per-cred acquire)
	_, ok = adm.TryAcquire("d")
	require.True(t, ok)
	r1()
}

func TestAdmissionTightensWhenPerCredRaisedFromZero(t *testing.T) {
	perCred := 0 // start unbounded
	adm := newAdmission(func() (int, int) { return 100, perCred })
	r1, ok := adm.TryAcquire("x")
	require.True(t, ok) // admitted while unbounded
	perCred = 1         // tighten the live per-cred limit to 1
	// "x" already has 1 in flight, so a second concurrent "x" must be refused.
	_, ok = adm.TryAcquire("x")
	require.False(t, ok)
	r1()
	// after release, "x" is back to 0 in flight and admits again.
	r2, ok := adm.TryAcquire("x")
	require.True(t, ok)
	r2()
}
