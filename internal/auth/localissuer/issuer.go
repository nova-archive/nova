package localissuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Config holds the dependencies and parameters for an Issuer.
type Config struct {
	Queries    *gen.Queries
	Signer     *token.Signer
	Gate       *password.Gate
	IssuerURL  string
	Audience   string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// Issuer implements the local auth issuer: login, refresh, logout, and JWKS.
type Issuer struct {
	cfg     Config
	refresh *refreshStore
}

// New constructs an Issuer from the given Config.
// It returns an error if any required field is missing, so that
// misconfiguration (e.g. empty Audience) is caught at startup rather than
// silently disabling audience confinement at runtime.
func New(cfg Config) (*Issuer, error) {
	switch {
	case cfg.Queries == nil:
		return nil, fmt.Errorf("localissuer: Queries must not be nil")
	case cfg.Signer == nil:
		return nil, fmt.Errorf("localissuer: Signer must not be nil")
	case cfg.IssuerURL == "":
		return nil, fmt.Errorf("localissuer: IssuerURL must not be empty")
	case cfg.Audience == "":
		return nil, fmt.Errorf("localissuer: Audience must not be empty")
	}
	return &Issuer{
		cfg:     cfg,
		refresh: newRefreshStore(cfg.Queries, cfg.RefreshTTL),
	}, nil
}

// tokenResponse is the JSON body returned by Login and Refresh.
// Fields are tagged with snake_case keys to match the OpenAPI TokenResponse
// schema and OAuth2 convention (access_token, refresh_token, token_type,
// expires_in, kid). Downstream consumers (novactl, SPA, integration tests)
// must decode using matching snake_case tags.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	KID          string `json:"kid"`
}

func writeTokenResponse(w http.ResponseWriter, accessToken, refreshToken string, cfg Config) {
	resp := tokenResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "bearer",
		ExpiresIn:    int(cfg.AccessTTL.Seconds()),
		KID:          cfg.Signer.KID(),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Login handles POST /api/v1/auth/login.
// It validates credentials and returns an access + refresh token pair.
func (iss *Issuer) Login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := middleware.RequestIDFromContext(ctx)

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "invalid request body", reqID)
		return
	}

	release, ok := iss.cfg.Gate.TryAcquire()
	if !ok {
		w.Header().Set("Retry-After", "1")
		httputil.WriteError(w, http.StatusServiceUnavailable, "server_busy", "too many concurrent login requests", reqID)
		return
	}
	defer release()

	u, err := iss.cfg.Queries.GetUserByEmail(ctx, body.Username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// User not found — still run dummy verify to equalize timing.
			password.DummyVerify(body.Password)
			slog.Warn("login failed: user not found")
			httputil.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", reqID)
			return
		}
		slog.Error("login: db error looking up user", "err", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", reqID)
		return
	}

	if u.Disabled || !u.PasswordHash.Valid {
		// Disabled or no password — still run dummy verify to equalize timing.
		password.DummyVerify(body.Password)
		slog.Warn("login failed: user disabled or no password hash", "user_id", uuid.UUID(u.ID.Bytes))
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", reqID)
		return
	}

	match, verifyErr := password.Verify(u.PasswordHash.String, body.Password)
	if verifyErr != nil {
		slog.Error("password verify error", "user_id", uuid.UUID(u.ID.Bytes), "err", verifyErr)
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", reqID)
		return
	}
	if !match {
		slog.Warn("login failed: wrong password", "user_id", uuid.UUID(u.ID.Bytes))
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password", reqID)
		return
	}

	userID := uuid.UUID(u.ID.Bytes)

	accessToken, err := iss.cfg.Signer.Sign(token.Mint{
		Subject:  userID.String(),
		Role:     string(u.Role),
		Issuer:   iss.cfg.IssuerURL,
		Audience: iss.cfg.Audience,
		TTL:      iss.cfg.AccessTTL,
	})
	if err != nil {
		slog.Error("login: failed to sign access token", "err", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", reqID)
		return
	}

	refreshRaw, err := iss.refresh.issue(ctx, userID, r.UserAgent())
	if err != nil {
		slog.Error("login: failed to issue refresh token", "err", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", reqID)
		return
	}

	slog.Info("login", "user_id", userID)
	writeTokenResponse(w, accessToken, refreshRaw, iss.cfg)
}

// Refresh handles POST /api/v1/auth/refresh.
// It rotates the refresh token and issues a new access token.
func (iss *Issuer) Refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := middleware.RequestIDFromContext(ctx)

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "invalid request body", reqID)
		return
	}

	uid, newRaw, err := iss.refresh.rotate(ctx, body.RefreshToken, r.UserAgent())
	if err != nil {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid_refresh_token", "refresh token is invalid or expired", reqID)
		return
	}

	u, err := iss.cfg.Queries.GetUserByID(ctx, pgUUID(uid))
	if err != nil {
		slog.Error("refresh: failed to look up user", "user_id", uid, "err", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", reqID)
		return
	}

	accessToken, err := iss.cfg.Signer.Sign(token.Mint{
		Subject:  uid.String(),
		Role:     string(u.Role),
		Issuer:   iss.cfg.IssuerURL,
		Audience: iss.cfg.Audience,
		TTL:      iss.cfg.AccessTTL,
	})
	if err != nil {
		slog.Error("refresh: failed to sign access token", "err", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", reqID)
		return
	}

	writeTokenResponse(w, accessToken, newRaw, iss.cfg)
}

// Logout handles POST /api/v1/auth/logout.
// It revokes the provided refresh token (idempotent).
func (iss *Issuer) Logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reqID := middleware.RequestIDFromContext(ctx)

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "invalid request body", reqID)
		return
	}

	// Ignore not-found errors — revoke is idempotent.
	_ = iss.refresh.revoke(ctx, body.RefreshToken)
	w.WriteHeader(http.StatusNoContent)
}

// JWKS handles GET /api/v1/auth/jwks.json — the coordinator mounts the
// JWKS under the auth-group prefix (not /.well-known/jwks.json) so nginx
// can fence /api/v1/auth/* as a single allow-listed group. It returns
// the public key set for token verification.
func (iss *Issuer) JWKS(w http.ResponseWriter, r *http.Request) {
	b, err := iss.cfg.Signer.JWKS()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal_error", "internal server error", "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// Verifier returns an auth.Verifier that validates tokens issued by this Issuer.
func (iss *Issuer) Verifier() auth.Verifier {
	return &localVerifier{
		v:         token.NewVerifier(iss.cfg.Signer.KID(), iss.cfg.Signer.Public()),
		issuerURL: iss.cfg.IssuerURL,
		audience:  iss.cfg.Audience,
	}
}

// localVerifier implements auth.Verifier for tokens issued by this local issuer.
type localVerifier struct {
	v         *token.Verifier
	issuerURL string
	audience  string
}

// Verify validates the raw JWT, enforcing issuer and audience in addition to
// signature and expiry. Returns auth.ErrTokenNotForMe when the issuer does not
// match, so the bearer middleware can try other verifiers.
func (lv *localVerifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	claims, err := lv.v.Verify(raw)
	if err != nil {
		return auth.Identity{}, err
	}

	// CARRY-FORWARD: enforce issuer — return ErrTokenNotForMe so middleware
	// can try the next verifier rather than treating it as a hard 401.
	if claims.Issuer != lv.issuerURL {
		return auth.Identity{}, auth.ErrTokenNotForMe
	}

	// CARRY-FORWARD: enforce audience confinement.
	if !claims.Audience.Contains(lv.audience) {
		return auth.Identity{}, errors.New("localissuer: audience mismatch")
	}

	return auth.Identity{
		UserID: claims.Subject,
		Role:   claims.Role,
		Issuer: claims.Issuer,
	}, nil
}
