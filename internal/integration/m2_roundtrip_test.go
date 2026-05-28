// Package integration holds cross-package integration tests that
// exercise multiple internal subsystems together. Each test in this
// package is the exit criterion for a specific milestone; M2's test
// is m2_roundtrip_test.go.
//
// Tests use the Integration name prefix for selection in the Makefile's
// test-integration target.
package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

// roundtripPayload is the JSON descriptor carried by an m2.roundtrip
// job. Job payloads are JSON metadata (the jobs queue stores payload as
// jsonb) — real Nova job kinds carry CIDs, sizes, and preset names, not
// raw blob bytes. The handler generates SizeBytes of plaintext, runs
// the full encrypt → import → fetch → decrypt cycle, and proves the
// recovered bytes match what it generated.
type roundtripPayload struct {
	SizeBytes int `json:"size_bytes"`
}

// TestIntegrationM2EncryptImportFetchDecrypt is the M2 exit test.
//
// Setup:
//   - postgres + migrations (via dbtest)
//   - envelope keystore bootstrapped against NOVA_MASTER_KEY_V1
//   - embedded Kubo node (offline)
//   - jobs queue + worker pool, with a synthetic "m2.roundtrip" kind
//     that performs the full encrypt/import/fetch/decrypt path
//
// Exercise:
//   - generate random plaintext (one ≤ raw threshold, one above)
//   - enqueue an m2.roundtrip job carrying the plaintext
//   - worker leases, runs encrypt → wrap → AddDeterministic → Get →
//     decrypt → bytes match
//   - test observes the result via a channel
//
// Verifies:
//   - envelope.Keystore.Wrap returns a wrapped key tied to the active
//     master_key_versions.id
//   - ipfs.EmbeddedBackend.AddDeterministic round-trips bytes for both
//     the raw (≤1MiB envelope) and dag-pb (>1MiB envelope) paths
//   - jobs.WorkerPool consumes and completes the work
//   - the three subsystems compose without coupling assumptions
func TestIntegrationM2EncryptImportFetchDecrypt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M2 integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// --- DB + keystore ---
	pool := dbtest.New(t, ctx)
	masterHex := randHex(t, envelope.KeySize)
	t.Setenv("NOVA_MASTER_KEY_V1", masterHex)
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	masterBytes, err := hex.DecodeString(masterHex)
	require.NoError(t, err)

	// --- IPFS embedded ---
	swarmPath := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarmPath,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     t.TempDir(),
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmPath,
		Online:       false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = be.Close(shutdownCtx)
	})

	// --- Jobs ---
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 60 * time.Second,
	})

	// The job payload is a JSON {size_bytes} descriptor. The handler:
	//   1. parses the size from the JSON payload
	//   2. generates size_bytes of random plaintext
	//   3. generates a per-blob key
	//   4. wraps it with the keystore (active master-key version)
	//   5. sanity-unwraps with the raw master to confirm composition
	//   6. v1.Encrypt(plaintext, perBlobKey)
	//   7. AddDeterministic(envelope) → CID
	//   8. Get(CID) → envelope bytes
	//   9. v1.Decrypt(envelope, perBlobKey) → recovered plaintext
	//  10. bytes-equal the generated plaintext; report success on resultCh
	var handlerDone atomic.Int32
	type result struct {
		ok    bool
		codec string
	}
	resultCh := make(chan result, 2)

	fail := func(err error) error {
		resultCh <- result{ok: false}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return err
	}

	wp.RegisterHandler("m2.roundtrip", func(ctx context.Context, payload []byte) error {
		var p roundtripPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fail(err)
		}
		if p.SizeBytes <= 0 {
			return fail(io.ErrUnexpectedEOF)
		}

		plaintext := make([]byte, p.SizeBytes)
		if _, err := rand.Read(plaintext); err != nil {
			return fail(err)
		}

		pbk := make([]byte, envelope.KeySize)
		if _, err := rand.Read(pbk); err != nil {
			return fail(err)
		}

		wrapped, _, err := ks.Wrap(pbk)
		if err != nil {
			return fail(err)
		}

		unwrapped, err := envelope.UnwrapKey(masterBytes, wrapped)
		if err != nil || !bytes.Equal(unwrapped, pbk) {
			return fail(err)
		}

		env, err := envelope.V1().Encrypt(plaintext, pbk)
		if err != nil {
			return fail(err)
		}

		addRes, err := be.AddDeterministic(ctx, env)
		if err != nil {
			return fail(err)
		}

		rc, err := be.Get(ctx, addRes.CID)
		if err != nil {
			return fail(err)
		}
		gotEnv, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fail(err)
		}

		gotPlain, err := envelope.V1().Decrypt(gotEnv, pbk)
		if err != nil {
			return fail(err)
		}

		if !bytes.Equal(gotPlain, plaintext) {
			resultCh <- result{ok: false}
			return nil
		}
		handlerDone.Add(1)
		resultCh <- result{ok: true, codec: addRes.Codec}
		return nil
	})

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	// Raw-codec path: 768 KiB plaintext + 48 envelope overhead ≤ 1 MiB.
	_, err = q.Enqueue(ctx, "m2.roundtrip", mustJSON(t, roundtripPayload{SizeBytes: 768 * 1024}))
	require.NoError(t, err)

	select {
	case r := <-resultCh:
		require.True(t, r.ok, "raw-codec round-trip handler reported failure")
		require.Equal(t, ipfs.CodecRaw, r.codec, "768 KiB envelope must use raw codec")
	case <-time.After(2 * time.Minute):
		t.Fatal("M2 roundtrip (raw) did not complete within 2 minutes")
	}
	require.Equal(t, int32(1), handlerDone.Load())

	// dag-pb path: 3 MiB plaintext drives the chunked import.
	_, err = q.Enqueue(ctx, "m2.roundtrip", mustJSON(t, roundtripPayload{SizeBytes: 3 * 1024 * 1024}))
	require.NoError(t, err)

	select {
	case r := <-resultCh:
		require.True(t, r.ok, "dag-pb round-trip handler reported failure")
		require.Equal(t, ipfs.CodecDagPB, r.codec, "3 MiB envelope must use dag-pb codec")
	case <-time.After(3 * time.Minute):
		t.Fatal("M2 roundtrip (dag-pb) did not complete within 3 minutes")
	}
	require.Equal(t, int32(2), handlerDone.Load())
}

// --- helpers ---

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	return hex.EncodeToString(randomBytes(t, n))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
