//go:build nova_dev

package auth

// EnforceAnonymousPolicy is a no-op in nova_dev builds: anonymous management
// bypass is permitted for local development only. M6 drops nova_dev from
// production builds entirely.
func EnforceAnonymousPolicy(anonymous bool) error { return nil }
