package localissuer

// Exports for test-only access to unexported helpers.

// RetryUntil exposes the unexported retryUntil helper to the external test
// package so the retry policy can be unit-tested without spinning up a
// Postgres container.
var RetryUntil = retryUntil

// Test-visible refresh-rotation error sentinels. The handler maps
// ErrRefreshInternal to 503 (transient/operational) and ErrRefreshInvalid
// to 401 (token rejected).
var (
	ErrRefreshInternal = errRefreshInternal
	ErrRefreshInvalid  = errRefreshInvalid
)
