package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

// SigningAdminHandler serves the M7 signed-URL admin endpoints:
//
//	POST /api/v1/admin/keys/rotate-signing   (operator)
//	POST /api/v1/admin/signed-urls/revoke    (operator+moderator)
//	POST /api/v1/admin/signed-urls/sign      (operator+moderator)
type SigningAdminHandler struct {
	pool   *pgxpool.Pool
	q      *gen.Queries
	ks     *envelope.Keystore
	keys   *signedurl.DBKeySource
	revs   *signedurl.DBRevocations
	grace  time.Duration
	maxTTL time.Duration
}

// NewSigningAdminHandler builds the handler. graceDefault is the rotation grace
// window when a request omits grace_seconds; maxTTL caps minted-URL lifetimes.
func NewSigningAdminHandler(pool *pgxpool.Pool, ks *envelope.Keystore, keys *signedurl.DBKeySource, revs *signedurl.DBRevocations, graceDefault, maxTTL time.Duration) *SigningAdminHandler {
	return &SigningAdminHandler{
		pool: pool, q: gen.New(pool), ks: ks, keys: keys, revs: revs,
		grace: graceDefault, maxTTL: maxTTL,
	}
}

type rotateSigningRequest struct {
	GraceSeconds int `json:"grace_seconds,omitempty"`
}

type rotateSigningResponse struct {
	KID            string `json:"kid"`
	GraceExpiresAt string `json:"grace_expires_at"`
}

// RotateSigning mints a new active signing key and retires the prior active key
// with retire_after = now + grace (URLs minted under it verify until then).
func (h *SigningAdminHandler) RotateSigning(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	var req rotateSigningRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // empty/garbage body ⇒ default grace
	}
	grace := h.grace
	if req.GraceSeconds > 0 {
		grace = time.Duration(req.GraceSeconds) * time.Second
	}
	retireAfter := time.Now().Add(grace)

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotate failed", rid)
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.q.WithTx(tx)

	kid, err := signedurl.MintKey(ctx, qtx, h.ks)
	if err != nil {
		slog.Error("rotate-signing: mint", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotate failed", rid)
		return
	}
	if err := qtx.RetirePriorActiveSigningKey(ctx, gen.RetirePriorActiveSigningKeyParams{
		RetireAfter: pgtype.Timestamptz{Time: retireAfter, Valid: true},
		Kid:         kid,
	}); err != nil {
		slog.Error("rotate-signing: retire", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotate failed", rid)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotate failed", rid)
		return
	}
	h.keys.Invalidate()

	graceExpires := retireAfter.UTC().Format(time.RFC3339)
	slog.Info("signing-key rotated", "kid", kid, "grace_expires_at", graceExpires, "request_id", rid)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rotateSigningResponse{KID: kid, GraceExpiresAt: graceExpires})
}

var validRevokeKinds = map[string]bool{"cid": true, "aud": true, "kid": true, "path_prefix": true}

type revokeRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type revokeResponse struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	RevokedAt string `json:"revoked_at"`
}

// RevokeSignedURL writes a (kind, value) revocation row (idempotent on the
// unique pair) and invalidates the in-process revocation cache so it takes
// effect immediately.
func (h *SigningAdminHandler) RevokeSignedURL(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	var req revokeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
		return
	}
	if !validRevokeKinds[req.Kind] {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_kind", "kind must be one of cid, aud, kid, path_prefix", rid)
		return
	}
	if req.Value == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "value is required", rid)
		return
	}

	if err := h.q.InsertRevocation(ctx, gen.InsertRevocationParams{Kind: req.Kind, Value: req.Value}); err != nil {
		slog.Error("revoke: insert", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "revoke failed", rid)
		return
	}
	if err := h.revs.Invalidate(ctx); err != nil {
		// Non-fatal: the periodic refresh will pick the row up.
		slog.Warn("revoke: cache invalidate", "err", err, "request_id", rid)
	}
	slog.Info("signed-url revoked", "kind", req.Kind, "value", req.Value, "request_id", rid)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(revokeResponse{
		Kind: req.Kind, Value: req.Value, RevokedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

type signRequest struct {
	Path       string `json:"path"`
	TTLSeconds int    `json:"ttl_seconds"`
	Aud        string `json:"aud"`
}

type signResponse struct {
	URL string `json:"url"`
	KID string `json:"kid"`
	Exp int64  `json:"exp"`
}

// SignSignedURL mints a signed URL for a content path under the active signing
// key, valid for ttl_seconds (≥1, capped at the configured max), bound to aud.
// Minting needs the unwrapped HMAC secret, so it is necessarily server-side.
func (h *SigningAdminHandler) SignSignedURL(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	ctx := r.Context()

	var req signRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
		return
	}
	if !validOrigin(req.Aud) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "aud must be an origin (scheme://host)", rid)
		return
	}
	if req.TTLSeconds < 1 {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "ttl_seconds must be positive", rid)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl > h.maxTTL {
		ttl = h.maxTTL
	}

	res, err := signedurl.Mint(ctx, h.keys, req.Path, ttl, req.Aud)
	if errors.Is(err, signedurl.ErrInvalidPath) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "path must be a content path (/blob/{cid} or /i/{cid}/...)", rid)
		return
	}
	if err != nil {
		slog.Error("sign: mint", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "sign failed", rid)
		return
	}
	slog.Info("signed-url minted", "path", req.Path, "aud", req.Aud, "kid", res.KID, "exp", res.Exp, "request_id", rid)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(signResponse{URL: res.URL, KID: res.KID, Exp: res.Exp})
}

// validOrigin reports whether s is a bare origin (scheme://host[:port], no path
// or query) — the shape an embedding context's Origin header carries.
func validOrigin(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}
