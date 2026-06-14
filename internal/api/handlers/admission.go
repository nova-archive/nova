package handlers

import "sync"

// admission bounds concurrent in-flight upload operations: a global ceiling and
// a per-credential ceiling. A zero/negative limit disables that dimension.
type admission struct {
	global chan struct{} // buffered to MaxConcurrentGlobal; nil ⇒ unbounded
	max    int           // per-credential cap; <=0 ⇒ unbounded
	mu     sync.Mutex
	inUse  map[string]int
}

func newAdmission(global, perCredential int) *admission {
	a := &admission{max: perCredential, inUse: make(map[string]int)}
	if global > 0 {
		a.global = make(chan struct{}, global)
	}
	return a
}

// TryAcquire reserves one global + one per-credential slot for cred. On success
// it returns a release func (call once) and true. On failure it acquires
// nothing and returns (nil, false).
func (a *admission) TryAcquire(cred string) (func(), bool) {
	// 1. global slot (non-blocking)
	if a.global != nil {
		select {
		case a.global <- struct{}{}:
		default:
			return nil, false
		}
	}
	// 2. per-credential slot
	if a.max > 0 {
		a.mu.Lock()
		if a.inUse[cred] >= a.max {
			a.mu.Unlock()
			if a.global != nil {
				<-a.global // roll back the global slot we took
			}
			return nil, false
		}
		a.inUse[cred]++
		a.mu.Unlock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if a.max > 0 {
				a.mu.Lock()
				if a.inUse[cred] > 0 {
					a.inUse[cred]--
					if a.inUse[cred] == 0 {
						delete(a.inUse, cred) // don't leak keys
					}
				}
				a.mu.Unlock()
			}
			if a.global != nil {
				<-a.global
			}
		})
	}, true
}
