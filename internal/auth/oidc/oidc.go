// Package oidc provides a verify-only external OIDC adapter with resilient
// discovery. It wraps github.com/coreos/go-oidc/v3/oidc to validate bearer
// tokens issued by an external Identity Provider and maps IdP groups/roles to
// Nova's internal role vocabulary.
//
// Design note: New attempts discovery once at start-up. On failure it does NOT
// return an error — it logs, starts a background retry, and returns a Verifier
// that yields auth.ErrAuthUnavailable until discovery succeeds. This ensures
// the coordinator can boot and serve content even when the external IdP is
// temporarily unreachable.
package oidc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/nova-archive/nova/internal/auth"
)

// Config holds the parameters for an external OIDC verifier.
type Config struct {
	// IssuerURL is the OIDC provider's issuer identifier (e.g.
	// "https://idp.example.com"). Discovery is performed at
	// IssuerURL + "/.well-known/openid-configuration".
	IssuerURL string

	// ClientID is the expected audience of incoming tokens.
	ClientID string

	// RoleClaim is the JWT claim whose value (string or []string) is mapped to
	// a Nova role. Defaults to "groups" when empty.
	RoleClaim string

	// RoleMapping maps IdP group/role strings to Nova roles
	// (viewer|uploader|moderator|operator). Unmapped values fall back to
	// "viewer".
	RoleMapping map[string]string
}

// Verifier validates external OIDC bearer tokens. It implements auth.Verifier.
type Verifier struct {
	cfg            Config
	mu             sync.RWMutex
	idv            *coreoidc.IDTokenVerifier
	initialBackoff time.Duration // first sleep in retryDiscovery; 0 → 1s default
	// ready is true once discovery has succeeded and idv is set.
	ready bool
}

// Ready reports whether OIDC discovery has succeeded and Verify can run
// against the IdP. Used by the /readyz handler via a type assertion so a
// coordinator with a pending external-IdP discovery is observably 503 on
// the readiness probe (separate from /health, which only checks
// process liveness). M6.2 D1.
func (v *Verifier) Ready() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.ready
}

// New constructs an external OIDC Verifier from cfg.
//
// It attempts OIDC discovery exactly once. On success the verifier is
// immediately usable. On failure it logs, starts a background retry goroutine,
// and returns a Verifier that returns auth.ErrAuthUnavailable until discovery
// eventually succeeds. (LOCAL-mode is fail-fast; this external package is
// resilient.)
//
// New returns a non-nil error only for invalid configuration (e.g. empty
// IssuerURL). Network/IdP failures during discovery are NOT errors — the
// verifier starts in the unavailable state and recovers in the background.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	return newWithBackoff(ctx, cfg, 0)
}

// newWithBackoff is the internal constructor used by New and tests. A zero
// backoff means "use the production default of 1s".
func newWithBackoff(ctx context.Context, cfg Config, initialBackoff time.Duration) (*Verifier, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("oidc: IssuerURL is required")
	}
	if cfg.RoleClaim == "" {
		cfg.RoleClaim = "groups"
	}

	v := &Verifier{cfg: cfg, initialBackoff: initialBackoff}

	provider, err := coreoidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		slog.Warn("oidc: discovery failed; retrying in background",
			"issuer", cfg.IssuerURL, "err", err)
		go v.retryDiscovery(context.Background())
		return v, nil
	}

	v.idv = provider.Verifier(&coreoidc.Config{ClientID: cfg.ClientID})
	v.ready = true
	return v, nil
}

// retryDiscovery loops with capped exponential back-off until OIDC discovery
// succeeds, then sets idv and ready under the write lock.
func (v *Verifier) retryDiscovery(ctx context.Context) {
	const maxBackoff = 60 * time.Second
	backoff := v.initialBackoff
	if backoff == 0 {
		backoff = time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		provider, err := coreoidc.NewProvider(ctx, v.cfg.IssuerURL)
		if err != nil {
			slog.Warn("oidc: discovery retry failed",
				"issuer", v.cfg.IssuerURL, "err", err,
				"next_retry", backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		v.mu.Lock()
		v.idv = provider.Verifier(&coreoidc.Config{ClientID: v.cfg.ClientID}) // Fix I1: was v.cfg.IssuerURL
		v.ready = true
		v.mu.Unlock()

		slog.Info("oidc: discovery succeeded (retry)", "issuer", v.cfg.IssuerURL)
		return
	}
}

// Verify implements auth.Verifier.
//
// It pre-checks the token's issuer without signature verification so it can
// return auth.ErrTokenNotForMe quickly (allowing the middleware to try other
// verifiers). It then delegates full verification to the coreos IDTokenVerifier.
func (v *Verifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	v.mu.RLock()
	idv := v.idv
	ready := v.ready
	v.mu.RUnlock()

	if !ready {
		return auth.Identity{}, auth.ErrAuthUnavailable
	}

	// Pre-check issuer without verifying the signature. This lets the bearer
	// middleware call ErrTokenNotForMe quickly so it can try other verifiers
	// (e.g. the local issuer) rather than failing hard with a 401.
	var issClaims struct {
		Issuer string `json:"iss"`
	}
	parsed, err := josejwt.ParseSigned(raw, []jose.SignatureAlgorithm{
		jose.EdDSA, jose.RS256, jose.RS384, jose.RS512,
		jose.ES256, jose.ES384, jose.ES512,
		jose.PS256, jose.PS384, jose.PS512,
	})
	if err == nil {
		_ = parsed.UnsafeClaimsWithoutVerification(&issClaims)
		if issClaims.Issuer != "" && issClaims.Issuer != v.cfg.IssuerURL {
			return auth.Identity{}, auth.ErrTokenNotForMe
		}
	}
	// If parsing fails entirely, fall through to idv.Verify which will produce
	// a descriptive error.

	idt, err := idv.Verify(ctx, raw)
	if err != nil {
		return auth.Identity{}, err
	}

	// Extract raw claims map for the role claim.
	var claims map[string]any
	if claimErr := idt.Claims(&claims); claimErr != nil {
		return auth.Identity{}, claimErr
	}

	role := v.mapRole(claims)

	return auth.Identity{
		UserID: idt.Subject,
		Role:   role,
		Issuer: v.cfg.IssuerURL,
	}, nil
}

// mapRole reads the configured role claim from claims and returns the mapped
// Nova role. It iterates the claim values in array order and returns the FIRST
// value that matches an entry in cfg.RoleMapping (first-match-wins, not
// highest-privilege). Values not present in the operator-supplied RoleMapping
// are ignored; if no value matches the fallback is "viewer".
func (v *Verifier) mapRole(claims map[string]any) string {
	val, ok := claims[v.cfg.RoleClaim]
	if !ok {
		return "viewer"
	}

	// Collect candidate values: the claim may be a string or an array.
	var candidates []string
	switch t := val.(type) {
	case string:
		candidates = []string{t}
	case []string:
		candidates = t
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				candidates = append(candidates, s)
			}
		}
	}

	for _, c := range candidates {
		if role, ok := v.cfg.RoleMapping[c]; ok {
			return role
		}
	}
	return "viewer"
}
