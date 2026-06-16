package agent

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
)

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
