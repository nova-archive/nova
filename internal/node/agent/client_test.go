package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
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
