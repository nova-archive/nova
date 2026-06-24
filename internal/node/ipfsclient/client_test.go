package ipfsclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/nova-archive/nova/internal/ipfs/importspec"
	ipfsclient "github.com/nova-archive/nova/internal/node/ipfsclient"
)

// fakeKubo records the path + query of the last add-family call and serves
// canned responses for block/put, add, pin/ls, pin/rm, repo/stat, cat.
type fakeKubo struct {
	addPath  string
	addQuery url.Values
	pinned   map[string]bool
	// catPath/catQuery record the last read (cat) call; catBody is the canned
	// envelope the fake serves back for any cat request.
	catPath  string
	catQuery url.Values
	catBody  []byte
}

func newFakeKubo(t *testing.T) (*fakeKubo, *httptest.Server) {
	k := &fakeKubo{pinned: map[string]bool{"bafyKNOWN": true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/block/put":
			k.addPath, k.addQuery = r.URL.Path, r.URL.Query()
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(map[string]any{"Key": "bafyRAW", "Size": 1})
		case "/api/v0/add":
			k.addPath, k.addQuery = r.URL.Path, r.URL.Query()
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(map[string]string{"Hash": "bafyDAGPB"})
		case "/api/v0/cat":
			k.catPath, k.catQuery = r.URL.Path, r.URL.Query()
			w.Write(k.catBody)
		case "/api/v0/pin/ls":
			if k.pinned[r.URL.Query().Get("arg")] {
				json.NewEncoder(w).Encode(map[string]any{"Keys": map[string]any{r.URL.Query().Get("arg"): map[string]string{"Type": "recursive"}}})
				return
			}
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"Message": "path is not pinned"})
		case "/api/v0/pin/rm":
			delete(k.pinned, r.URL.Query().Get("arg"))
			json.NewEncoder(w).Encode(map[string]any{"Pins": []string{r.URL.Query().Get("arg")}})
		case "/api/v0/repo/stat":
			json.NewEncoder(w).Encode(map[string]any{"RepoSize": 4096})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return k, srv
}

func TestAddDeterministicRawCodecPath(t *testing.T) {
	k, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	root, err := c.AddDeterministic(context.Background(), bytes.Repeat([]byte("x"), 1024)) // <= 1 MiB => raw
	if err != nil || root != "bafyRAW" {
		t.Fatalf("root=%q err=%v", root, err)
	}
	if k.addPath != "/api/v0/block/put" || k.addQuery.Get("cid-codec") != "raw" ||
		k.addQuery.Get("mhtype") != importspec.HashAlg || k.addQuery.Get("pin") != "true" {
		t.Fatalf("raw path params drift: %s %v", k.addPath, k.addQuery)
	}
}

func TestAddDeterministicDagPBPath(t *testing.T) {
	k, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	root, err := c.AddDeterministic(context.Background(), bytes.Repeat([]byte("x"), (1<<20)+1)) // > 1 MiB => dag-pb
	if err != nil || root != "bafyDAGPB" {
		t.Fatalf("root=%q err=%v", root, err)
	}
	if k.addPath != "/api/v0/add" || k.addQuery.Get("chunker") != importspec.ChunkerSpec ||
		k.addQuery.Get("cid-version") != "1" || k.addQuery.Get("raw-leaves") != "true" || k.addQuery.Get("hash") != importspec.HashAlg {
		t.Fatalf("dag-pb path params drift: %s %v", k.addPath, k.addQuery)
	}
}

func TestRawThresholdBoundary(t *testing.T) {
	_, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	// exactly threshold => raw (bafyRAW); threshold+1 => dag-pb (bafyDAGPB)
	r1, err := c.AddDeterministic(context.Background(), make([]byte, importspec.RawCodecThresholdBytes))
	if err != nil {
		t.Fatal(err)
	}
	if r1 != "bafyRAW" {
		t.Fatalf("threshold should be raw, got %q", r1)
	}
	r2, err := c.AddDeterministic(context.Background(), make([]byte, importspec.RawCodecThresholdBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	if r2 != "bafyDAGPB" {
		t.Fatalf("threshold+1 should be dag-pb, got %q", r2)
	}
}

func TestHasMeansPinnedAndUnpin(t *testing.T) {
	_, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	ok1, err := c.Has(context.Background(), "bafyKNOWN")
	if err != nil {
		t.Fatal(err)
	}
	if !ok1 {
		t.Fatal("expected Has true for pinned cid")
	}
	ok2, err := c.Has(context.Background(), "bafyMISSING")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("expected Has false for unpinned cid")
	}
	if err := c.Unpin(context.Background(), "bafyKNOWN"); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	ok3, err := c.Has(context.Background(), "bafyKNOWN")
	if err != nil {
		t.Fatal(err)
	}
	if ok3 {
		t.Fatal("expected Has false after unpin")
	}
	n, err := c.RepoStoredBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 4096 {
		t.Fatalf("repo size %d", n)
	}
}

// TestGetUsesCatAndPassesThrough confirms Get hits /api/v0/cat?arg=<cid> and
// returns the served bytes verbatim, for BOTH a small (raw) and a >1 MiB
// (dag-pb) envelope. The real-Kubo round-trip (AddDeterministic→Get→
// AddDeterministic == same CID) is a deferred integration check, mirroring how
// M4 deferred the block/put round-trip — here we assert endpoint+params+exact
// byte passthrough against the mock API.
func TestGetUsesCatAndPassesThrough(t *testing.T) {
	cases := []struct {
		name string
		cid  string
		body []byte
	}{
		{"small_raw", "bafyRAW", bytes.Repeat([]byte("r"), 1024)},
		{"large_dagpb", "bafyDAGPB", bytes.Repeat([]byte("d"), (1<<20)+7)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, srv := newFakeKubo(t)
			k.catBody = tc.body
			c := ipfsclient.New(srv.URL)
			rc, err := c.Get(context.Background(), tc.cid)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("body mismatch: got %d bytes want %d", len(got), len(tc.body))
			}
			if k.catPath != "/api/v0/cat" {
				t.Fatalf("expected /api/v0/cat, got %q", k.catPath)
			}
			if k.catQuery.Get("arg") != tc.cid {
				t.Fatalf("cat arg = %q, want %q", k.catQuery.Get("arg"), tc.cid)
			}
		})
	}
}

// TestGetErrorsOnNon200 confirms a non-200 from the API surfaces as an error and
// no body is returned (a missing/unpinned CID must not look like an empty blob).
func TestGetErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"Message":"no link named"}`)
	}))
	t.Cleanup(srv.Close)
	c := ipfsclient.New(srv.URL)
	rc, err := c.Get(context.Background(), "bafyMISSING")
	if err == nil {
		if rc != nil {
			rc.Close()
		}
		t.Fatal("expected error on non-200, got nil")
	}
	if rc != nil {
		t.Fatal("expected nil ReadCloser on error")
	}
}
