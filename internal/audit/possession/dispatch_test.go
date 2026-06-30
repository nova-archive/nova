package possession

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/nova-archive/nova/internal/federation/wire"
)

func rawLeafCID(t *testing.T, raw []byte) string {
	t.Helper()
	c, err := cid.V1Builder{Codec: cid.Raw, MhType: mh.SHA2_256}.Sum(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c.String()
}

func TestDispatchVerifiesByCIDReconstruction(t *testing.T) {
	raw := []byte("hello-block")
	blkCID := rawLeafCID(t, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(raw)
	}))
	defer srv.Close()
	d := &Dispatcher{hc: srv.Client(), now: time.Now}
	res, err := d.Challenge(context.Background(), srv.URL, wire.AuditChallenge{
		BlockCID:  blkCID,
		BlockSize: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePass {
		t.Fatalf("want OutcomePass, got %v", res.Outcome)
	}
}

func TestDispatchWrongBytesIsMismatch(t *testing.T) {
	raw := []byte("hello-block")
	tampered := []byte("HELLO-BLOCK") // same length, different content
	blkCID := rawLeafCID(t, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tampered)
	}))
	defer srv.Close()
	d := &Dispatcher{hc: srv.Client(), now: time.Now}
	res, err := d.Challenge(context.Background(), srv.URL, wire.AuditChallenge{
		BlockCID:  blkCID,
		BlockSize: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailMismatch {
		t.Fatalf("want OutcomeFailMismatch, got %v", res.Outcome)
	}
}

func TestDispatch404IsNotPresent(t *testing.T) {
	raw := []byte("hello-block")
	blkCID := rawLeafCID(t, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	d := &Dispatcher{hc: srv.Client(), now: time.Now}
	res, err := d.Challenge(context.Background(), srv.URL, wire.AuditChallenge{
		BlockCID:  blkCID,
		BlockSize: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailNotPresent {
		t.Fatalf("want OutcomeFailNotPresent, got %v", res.Outcome)
	}
}

func TestDispatch429IsSkipBudget(t *testing.T) {
	raw := []byte("hello-block")
	blkCID := rawLeafCID(t, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	d := &Dispatcher{hc: srv.Client(), now: time.Now}
	res, err := d.Challenge(context.Background(), srv.URL, wire.AuditChallenge{
		BlockCID:  blkCID,
		BlockSize: int64(len(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeSkipBudget {
		t.Fatalf("want OutcomeSkipBudget, got %v", res.Outcome)
	}
}
