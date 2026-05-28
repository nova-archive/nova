//go:build !nova_dev

// Package auth holds the coordinator's authentication surface. M3 ships only
// the anonymous-mode startup floor; bearer (M6) and signed URLs (M7) land
// later. Two build-tagged files implement EnforceAnonymousPolicy: production
// (this file) refuses anonymous mode; the nova_dev build permits it.
package auth

import "errors"

// EnforceAnonymousPolicy returns an error in production builds when the
// operator set auth.anonymous=true. Anonymous management bypass is a dev-only
// affordance; a production binary must refuse to start.
func EnforceAnonymousPolicy(anonymous bool) error {
	if anonymous {
		return errors.New("auth: anonymous mode is not permitted in production builds (rebuild with -tags nova_dev for local dev)")
	}
	return nil
}
