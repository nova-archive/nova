package localissuer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func newIssuer(t *testing.T, ctx context.Context) (*localissuer.Issuer, *gen.Queries, uuid.UUID, *pgxpool.Pool) {
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	hash, err := password.Hash("hunter2hunter2")
	require.NoError(t, err)
	u, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email:        "u@example.com",
		Role:         gen.UserRole("operator"),
		PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	require.NoError(t, err)
	signer, err := token.NewSignerFromSeed("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	require.NoError(t, err)
	iss, err := localissuer.New(localissuer.Config{
		Queries: q, Signer: signer, Gate: password.NewGate(4),
		IssuerURL: "https://nova.test/", Audience: "nova",
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)
	return iss, q, uuid.UUID(u.ID.Bytes), pool
}

func TestLoginThenVerify(t *testing.T) {
	ctx := context.Background()
	iss, _, uid, _ := newIssuer(t, ctx)
	body, _ := json.Marshal(map[string]string{"username": "u@example.com", "password": "hunter2hunter2"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"access_token"`)
	require.Contains(t, rr.Body.String(), `"token_type":"bearer"`)
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &tr))
	require.Equal(t, "bearer", tr.TokenType)
	id, err := iss.Verifier().Verify(ctx, tr.AccessToken)
	require.NoError(t, err)
	require.Equal(t, uid.String(), id.UserID)
	require.Equal(t, "operator", id.Role)
}

func TestLoginWrongPasswordIsGeneric401(t *testing.T) {
	ctx := context.Background()
	iss, _, _, _ := newIssuer(t, ctx)
	body, _ := json.Marshal(map[string]string{"username": "u@example.com", "password": "nope"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid_credentials")
}

func TestLoginUnknownUserSameStatusAndCode(t *testing.T) {
	ctx := context.Background()
	iss, _, _, _ := newIssuer(t, ctx)
	body, _ := json.Marshal(map[string]string{"username": "ghost@example.com", "password": "whatever"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid_credentials")
}

func TestRefreshRotatesAndVerifies(t *testing.T) {
	ctx := context.Background()
	iss, _, _, _ := newIssuer(t, ctx)
	body, _ := json.Marshal(map[string]string{"username": "u@example.com", "password": "hunter2hunter2"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &tr))

	rb, _ := json.Marshal(map[string]string{"refresh_token": tr.RefreshToken})
	rr2 := httptest.NewRecorder()
	iss.Refresh(rr2, httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewReader(rb)))
	require.Equal(t, http.StatusOK, rr2.Code)
	var tr2 struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &tr2))
	require.NotEqual(t, tr.RefreshToken, tr2.RefreshToken)
	_, err := iss.Verifier().Verify(ctx, tr2.AccessToken)
	require.NoError(t, err)
}

func TestVerifierRejectsForeignIssuer(t *testing.T) {
	ctx := context.Background()
	iss, _, _, _ := newIssuer(t, ctx)
	other, _ := token.NewSignerFromSeed("ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100")
	raw, _ := other.Sign(token.Mint{Subject: "x", Role: "operator", Issuer: "https://evil/", Audience: "nova", TTL: time.Minute})
	_, err := iss.Verifier().Verify(ctx, raw)
	require.Error(t, err)
}

// TestLoginDisabledUserIs401 seeds a disabled user and asserts login returns
// 401 invalid_credentials — not a 500 or a success.
func TestLoginDisabledUserIs401(t *testing.T) {
	ctx := context.Background()
	iss, _, _, pool := newIssuer(t, ctx)

	_, err := pool.Exec(ctx, "UPDATE users SET disabled = true WHERE email = $1", "u@example.com")
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]string{"username": "u@example.com", "password": "hunter2hunter2"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid_credentials")
}

// TestVerifierRejectsWrongAudience mints a token with the correct issuer and
// signer but a wrong audience, and asserts Verify returns a hard error that is
// NOT auth.ErrTokenNotForMe (audience mismatch is a hard fail, not a routing
// signal).
func TestVerifierRejectsWrongAudience(t *testing.T) {
	ctx := context.Background()
	iss, _, _, _ := newIssuer(t, ctx)

	// Use the same seed as newIssuer so the signature is valid.
	signer, err := token.NewSignerFromSeed("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	require.NoError(t, err)

	raw, err := signer.Sign(token.Mint{
		Subject:  "x",
		Role:     "operator",
		Issuer:   "https://nova.test/",
		Audience: "not-nova",
		TTL:      time.Minute,
	})
	require.NoError(t, err)

	_, verifyErr := iss.Verifier().Verify(ctx, raw)
	require.Error(t, verifyErr)
	require.NotErrorIs(t, verifyErr, auth.ErrTokenNotForMe)
}

// withCapturedDefaultLogger swaps the slog default logger for one that
// writes JSON records to buf at the given level, restoring the previous
// default when the test ends. Not safe to use in t.Parallel() — these
// tests intentionally do not parallelize because they mutate global
// slog state.
func withCapturedDefaultLogger(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoginFailureLogsAreUniformAtInfo verifies the M6.2 B3 contract:
// at the default Info log level, login-failure log lines carry no
// per-user differentiator (no user_id, no reason, no email). An
// observer aggregating these logs cannot enumerate existing users.
func TestLoginFailureLogsAreUniformAtInfo(t *testing.T) {
	ctx := context.Background()
	iss, _, _, pool := newIssuer(t, ctx)
	buf := withCapturedDefaultLogger(t, slog.LevelInfo)

	// Disabled user case requires the seed user to be disabled.
	_, err := pool.Exec(ctx, "UPDATE users SET disabled = true WHERE email = $1", "u@example.com")
	require.NoError(t, err)

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"unknown user", "ghost@example.com", "whatever"},
		{"disabled user", "u@example.com", "hunter2hunter2"},
	}
	for _, c := range cases {
		body, _ := json.Marshal(map[string]string{"username": c.username, "password": c.password})
		rr := httptest.NewRecorder()
		iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
		require.Equal(t, http.StatusUnauthorized, rr.Code, c.name)
	}

	out := buf.String()
	require.Contains(t, out, `"msg":"login failed"`, "at least one failure logged")
	require.NotContains(t, out, "user_id", "Info-level logs must not leak user_id")
	require.NotContains(t, out, "reason", "Info-level logs must not leak the failure reason")
	require.NotContains(t, out, "u@example.com", "Info-level logs must not echo the submitted username")
	require.NotContains(t, out, "ghost@example.com", "Info-level logs must not echo the submitted username")
}

// TestLoginFailureLogsCarryDetailAtDebug verifies that operators who
// turn the slog level to Debug see the granular reason + user_id for
// triage. This is the intended escape hatch — production runs at
// Info, debug runs see the structured fields.
func TestLoginFailureLogsCarryDetailAtDebug(t *testing.T) {
	ctx := context.Background()
	iss, _, uid, _ := newIssuer(t, ctx)
	buf := withCapturedDefaultLogger(t, slog.LevelDebug)

	body, _ := json.Marshal(map[string]string{"username": "u@example.com", "password": "wrong"})
	rr := httptest.NewRecorder()
	iss.Login(rr, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, rr.Code)

	out := buf.String()
	require.Contains(t, out, `"msg":"login failed"`, "Warn line still emitted at Debug")
	require.Contains(t, out, `"msg":"login failed (detail)"`, "Debug-level detail line emitted")
	require.Contains(t, out, `"reason":"wrong_password"`)
	require.Contains(t, out, `"user_id":"`+uid.String()+`"`)

	// Unknown-user case has reason but no user_id.
	buf.Reset()
	body2, _ := json.Marshal(map[string]string{"username": "ghost@example.com", "password": "x"})
	rr2 := httptest.NewRecorder()
	iss.Login(rr2, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body2)))
	require.Equal(t, http.StatusUnauthorized, rr2.Code)
	out2 := buf.String()
	require.Contains(t, out2, `"reason":"user_not_found"`)
	require.NotContains(t, out2, `"user_id"`, "no user_id for unknown-user path (no record to reference)")

	// Sanity: the debug line emits "request_id" so operators can correlate
	// triage details back to the corresponding HTTP request log line.
	require.True(t, strings.Contains(out, `"request_id"`) || strings.Contains(out2, `"request_id"`),
		"detail lines must carry a request_id field for correlation")
}

// TestNewRejectsEmptyAudience ensures New fails fast when Audience is empty,
// preventing a fail-open audience check at runtime.
func TestNewRejectsEmptyAudience(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	signer, err := token.NewSignerFromSeed("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	require.NoError(t, err)

	_, err = localissuer.New(localissuer.Config{
		Queries:    q,
		Signer:     signer,
		Gate:       password.NewGate(4),
		IssuerURL:  "https://nova.test/",
		Audience:   "", // intentionally empty
		AccessTTL:  15 * time.Minute,
		RefreshTTL: time.Hour,
	})
	require.Error(t, err)
}
