package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestDMCAIntakeSubmit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	h := handlers.NewDMCAIntakeHandler(gen.New(pool))

	// Happy path: 202 + a parseable case_id, and the row lands in dmca_cases.
	body := `{"claimant_name":"Acme Legal","claimant_email":"legal@acme.test","sworn_statement":"I attest under penalty of perjury","target_cid":"bafyintake1"}`
	rec := httptest.NewRecorder()
	h.Submit(rec, httptest.NewRequest("POST", "/legal/dmca", strings.NewReader(body)))
	require.Equal(t, 202, rec.Code, rec.Body.String())

	var out struct {
		CaseID string `json:"case_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	caseID, err := uuid.Parse(out.CaseID)
	require.NoError(t, err)

	var (
		name, email, cid, status string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT claimant_name, claimant_email, target_cid, status::text FROM dmca_cases WHERE id=$1`,
		caseID).Scan(&name, &email, &cid, &status))
	require.Equal(t, "Acme Legal", name)
	require.Equal(t, "legal@acme.test", email)
	require.Equal(t, "bafyintake1", cid)
	require.Equal(t, "received", status, "intake takes no action; case starts in received")

	// Each missing-field variant ⇒ 400.
	for _, b := range []string{
		`{"claimant_email":"x@y.z","sworn_statement":"s","target_cid":"c"}`,                    // no claimant_name
		`{"claimant_name":"n","sworn_statement":"s","target_cid":"c"}`,                         // no claimant_email
		`{"claimant_name":"n","claimant_email":"x@y.z","target_cid":"c"}`,                      // no sworn_statement
		`{"claimant_name":"n","claimant_email":"x@y.z","sworn_statement":"s"}`,                 // no target_cid
		`{"claimant_name":"","claimant_email":"x@y.z","sworn_statement":"s","target_cid":"c"}`, // empty claimant_name
		`not json`, // malformed
	} {
		rec := httptest.NewRecorder()
		h.Submit(rec, httptest.NewRequest("POST", "/legal/dmca", strings.NewReader(b)))
		require.Equal(t, 400, rec.Code, b)
	}
}
