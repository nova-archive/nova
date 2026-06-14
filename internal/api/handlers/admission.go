package handlers

import "sync"

// limitsFn returns the current (global, perCredential) caps; <=0 ⇒ unbounded.
type limitsFn func() (global, perCredential int)

// admission bounds concurrent in-flight upload operations using live limits.
type admission struct {
	limits    limitsFn
	mu        sync.Mutex
	globalIn  int
	perCredIn map[string]int
}

func newAdmission(limits limitsFn) *admission {
	return &admission{limits: limits, perCredIn: make(map[string]int)}
}

// TryAcquire reserves one global + one per-credential slot for cred. On success
// returns a release func (call once) and true; on failure acquires nothing.
func (a *admission) TryAcquire(cred string) (func(), bool) {
	global, perCred := a.limits()
	a.mu.Lock()
	if global > 0 && a.globalIn >= global {
		a.mu.Unlock()
		return nil, false
	}
	if perCred > 0 && a.perCredIn[cred] >= perCred {
		a.mu.Unlock()
		return nil, false // global not yet taken ⇒ nothing to roll back
	}
	a.globalIn++
	if perCred > 0 || a.perCredIn[cred] > 0 {
		a.perCredIn[cred]++
	}
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.globalIn--
			if a.perCredIn[cred] > 0 {
				a.perCredIn[cred]--
				if a.perCredIn[cred] == 0 {
					delete(a.perCredIn, cred)
				}
			}
			a.mu.Unlock()
		})
	}, true
}
