package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/transfer"
)

func TestGetChangesAndSnapshotRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fed/v1/pins/changes":
			if r.URL.Query().Get("since_seq") == "999" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(wire.ErrorResponse{Code: wire.CodeSnapshotRequired})
				return
			}
			json.NewEncoder(w).Encode(wire.ChangesResponse{Changes: []wire.PinChange{{Sequence: 1, Kind: "assign", CID: "bafy1"}}, NextSeq: 1, CurrentEpoch: 1})
		case "/fed/v1/pins/snapshot":
			json.NewEncoder(w).Encode(wire.SnapshotResponse{Data: []wire.SnapshotItem{{CID: "bafy1", AssignmentID: "a", Generation: 1}}, Cursor: "", SnapshotEpoch: 5})
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, nil)

	resp, err := c.GetChanges(context.Background(), 0)
	if err != nil || len(resp.Changes) != 1 {
		t.Fatalf("GetChanges: %+v err=%v", resp, err)
	}
	if _, err := c.GetChanges(context.Background(), 999); !errors.Is(err, ErrSnapshotRequired) {
		t.Fatalf("want ErrSnapshotRequired, got %v", err)
	}
	snap, err := c.GetSnapshot(context.Background(), "", 0)
	if err != nil || snap.SnapshotEpoch != 5 {
		t.Fatalf("GetSnapshot: %+v err=%v", snap, err)
	}
}

func TestAckSuccessAndStale(t *testing.T) {
	var statusCode int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ack") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(statusCode)
		if statusCode == http.StatusConflict {
			json.NewEncoder(w).Encode(wire.ErrorResponse{Code: wire.CodeStaleAssignment})
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, nil)

	statusCode = http.StatusNoContent
	if err := c.Ack(context.Background(), "bafyX", wire.Ack{AssignmentID: "a1", Generation: 1, CID: "bafyX"}); err != nil {
		t.Fatalf("204 should succeed: %v", err)
	}

	statusCode = http.StatusConflict
	if err := c.Ack(context.Background(), "bafyX", wire.Ack{AssignmentID: "a1", Generation: 1, CID: "bafyX"}); !errors.Is(err, ErrStaleAssignment) {
		t.Fatalf("409 should be ErrStaleAssignment, got: %v", err)
	}
}

func TestFailSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/fail") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, nil)

	if err := c.Fail(context.Background(), "bafyX", wire.Fail{AssignmentID: "a1", Generation: 1, CID: "bafyX", Reason: wire.FailReasonOther}); err != nil {
		t.Fatalf("204 should succeed: %v", err)
	}
}

func TestFetchClassifiesStatus(t *testing.T) {
	type testCase struct {
		status  int
		body    string
		wantErr error
		wantRC  bool
	}
	cases := []testCase{
		{status: http.StatusOK, body: "hello-bytes", wantRC: true},
		{status: http.StatusNotFound, wantErr: transfer.ErrSourceMissing},
		{status: http.StatusForbidden, wantErr: transfer.ErrSourceUnauthorized},
	}

	for _, tc := range cases {
		tc := tc
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/fed/v1/blob/") {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
			if tc.body != "" {
				w.Write([]byte(tc.body))
			}
		}))
		c := NewHTTPClient(srv.URL, nil)
		src := wire.ChangeSource{Token: "tok1"}
		rc, err := c.Fetch(context.Background(), src, "bafyX", 1<<20)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("status %d: want %v, got %v", tc.status, tc.wantErr, err)
			}
		} else if tc.wantRC {
			if err != nil {
				t.Errorf("status %d: unexpected error %v", tc.status, err)
			} else {
				rc.Close()
			}
		}
		srv.Close()
	}
}

func TestHTTPClientRegisterPostsToFedEndpoint(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"selected_protocol":"fed/v1","node_id":"n1"}`))
	}))
	defer ts.Close()

	c := NewHTTPClient(ts.URL, &tls.Config{})
	c.hc = ts.Client() // exercise request/JSON plumbing over plain HTTP

	resp, err := c.Register(context.Background(), wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.NodeID != "n1" || gotPath != "/fed/v1/register" {
		t.Fatalf("resp=%+v path=%s", resp, gotPath)
	}
}
