package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type capture struct {
	ts, sig     string
	body        []byte
	contentType string
}

func captureServer(t *testing.T, capCh chan<- capture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capCh <- capture{
			ts:          r.Header.Get("X-Nova-Webhook-Timestamp"),
			sig:         r.Header.Get("X-Nova-Webhook-Signature"),
			body:        body,
			contentType: r.Header.Get("Content-Type"),
		}
	}))
}

func waitCapture(t *testing.T, ch <-chan capture) capture {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook delivery")
		return capture{}
	}
}

func TestParanoidSkipsEmit(t *testing.T) {
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hit, 1) }))
	defer srv.Close()
	e := NewBestEffortHTTP([]Destination{{URL: srv.URL}}, nil, Options{Paranoid: true})
	e.Emit(context.Background(), Event{Type: "federation.degraded"})
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&hit), "paranoid skips all emission")
}

func TestHMACSignatureFormatV1(t *testing.T) {
	secret := []byte("s3cr3t")
	capCh := make(chan capture, 1)
	srv := captureServer(t, capCh)
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	e := NewBestEffortHTTP([]Destination{{URL: srv.URL, Secret: secret}}, nil, Options{Now: func() time.Time { return now }})

	e.Emit(context.Background(), Event{Type: "federation.degraded", ScopeKey: "global"})
	c := waitCapture(t, capCh)

	require.Equal(t, "application/json", c.contentType)
	require.Equal(t, "1700000000", c.ts)
	signed := c.ts + "." + string(c.body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signed))
	require.Equal(t, "v1="+hex.EncodeToString(mac.Sum(nil)), c.sig,
		"signature is v1=hex(HMAC_SHA256(secret, timestamp.body))")
}

func TestNoSignatureWhenSecretUnset(t *testing.T) {
	capCh := make(chan capture, 1)
	srv := captureServer(t, capCh)
	defer srv.Close()
	e := NewBestEffortHTTP([]Destination{{URL: srv.URL}}, nil, Options{})

	e.Emit(context.Background(), Event{Type: "federation.degraded"})
	c := waitCapture(t, capCh)
	require.Empty(t, c.sig, "no signature header without a secret")
	require.Empty(t, c.ts, "no timestamp header without a secret")
}

func TestEventsFilterPerDestination(t *testing.T) {
	revCh := make(chan capture, 1)
	degCh := make(chan capture, 1)
	revSrv := captureServer(t, revCh)
	defer revSrv.Close()
	degSrv := captureServer(t, degCh)
	defer degSrv.Close()

	e := NewBestEffortHTTP([]Destination{
		{URL: revSrv.URL, Events: []string{"federation.node_revoked"}},
		{URL: degSrv.URL, Events: []string{"federation.degraded"}},
	}, nil, Options{})

	e.Emit(context.Background(), Event{Type: "federation.node_revoked", ScopeKey: "n1"})
	_ = waitCapture(t, revCh) // the node_revoked destination is hit

	select {
	case <-degCh:
		t.Fatal("degraded destination must not receive a node_revoked event")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestBestEffortPostTimeoutNoCascade(t *testing.T) {
	// A slow destination must not block the caller or cascade into the healing loop.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()
	e := NewBestEffortHTTP([]Destination{{URL: srv.URL}}, nil, Options{Timeout: 100 * time.Millisecond})

	start := time.Now()
	e.Emit(context.Background(), Event{Type: "federation.degraded"})
	require.Less(t, time.Since(start), 200*time.Millisecond, "Emit must return without waiting on the slow POST")
}

func TestBoundedConcurrencyUnderStorm(t *testing.T) {
	const cap = 2
	release := make(chan struct{})
	var concurrent, maxConcurrent, received int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&received, 1)
		c := atomic.AddInt32(&concurrent, 1)
		for {
			m := atomic.LoadInt32(&maxConcurrent)
			if c <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, c) {
				break
			}
		}
		<-release
		atomic.AddInt32(&concurrent, -1)
	}))
	defer srv.Close()

	// suppress=nil so distinct-scope events all attempt delivery; the semaphore is
	// the only bound.
	e := NewBestEffortHTTP([]Destination{{URL: srv.URL}}, nil, Options{Concurrency: cap, Timeout: 5 * time.Second})
	for i := 0; i < 20; i++ {
		e.Emit(context.Background(), Event{Type: "federation.concentrated", ScopeKey: "s" + string(rune('a'+i))})
	}
	time.Sleep(150 * time.Millisecond) // let the admitted deliveries reach the server

	require.LessOrEqual(t, atomic.LoadInt32(&received), int32(cap), "excess events dropped, not queued")
	require.LessOrEqual(t, atomic.LoadInt32(&maxConcurrent), int32(cap), "never more than `cap` concurrent POSTs")
	close(release)
}

func TestNodeSuspectWindowIs24h(t *testing.T) {
	e := &BestEffortHTTP{massCasualtyWindow: time.Hour}
	if e.windowSeconds("federation.node_suspect") != int((24 * time.Hour).Seconds()) {
		t.Fatal("node_suspect should suppress per 24h, scoped by node_id")
	}
}
