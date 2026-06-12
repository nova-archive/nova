//go:build novasim

// Package calib measures per-operation costs from the REAL Nova production
// primitives (internal/envelope, internal/ipfs) so the discrete-event model
// runs on host-measured constants rather than guesses. This is the "real where
// it's cheap" half of the calibrated-hybrid approach: AEAD and key-wrap need no
// infrastructure; deterministic IPFS import runs against an offline embedded
// Kubo in a temp repo. Build-tag-gated (novasim) so it never enters the default
// build / CI surface.
package calib

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/simulations/go/model"
)

// Options tunes the calibration workload.
type Options struct {
	AEADObjectBytes   int           // buffer size for AEAD throughput (default 4 MiB)
	AEADWindow        time.Duration // per-direction measurement window (default 800ms)
	WrapIters         int           // wrap/unwrap latency iterations (default 200000)
	ImportObjectBytes int           // size per deterministic import (default 4 MiB)
	ImportIters       int           // number of imports (default 40)
}

// DefaultOptions returns a quick-but-stable workload (~a few seconds).
func DefaultOptions() Options {
	return Options{
		AEADObjectBytes:   4 << 20,
		AEADWindow:        800 * time.Millisecond,
		WrapIters:         200000,
		ImportObjectBytes: 4 << 20,
		ImportIters:       40,
	}
}

// Run executes the calibration and returns measured per-operation costs.
func Run(ctx context.Context, opts Options) (model.Calibration, error) {
	d := DefaultOptions()
	if opts.AEADObjectBytes <= 0 {
		opts.AEADObjectBytes = d.AEADObjectBytes
	}
	if opts.AEADWindow <= 0 {
		opts.AEADWindow = d.AEADWindow
	}
	if opts.WrapIters <= 0 {
		opts.WrapIters = d.WrapIters
	}
	if opts.ImportObjectBytes <= 0 {
		opts.ImportObjectBytes = d.ImportObjectBytes
	}
	if opts.ImportIters <= 0 {
		opts.ImportIters = d.ImportIters
	}

	host, _ := os.Hostname()
	cal := model.Calibration{Host: host, Cores: runtime.NumCPU(), Measured: true}

	// --- AEAD encrypt/decrypt (real envelope.V1 codec; single-goroutine = per-core).
	codec := envelope.V1()
	key := mustRand(envelope.KeySize)
	pt := mustRand(opts.AEADObjectBytes)
	env, err := codec.Encrypt(pt, key)
	if err != nil {
		return cal, fmt.Errorf("calib: warm encrypt: %w", err)
	}
	cal.EncryptBytesPerSecPerCore = throughput(opts.AEADWindow, opts.AEADObjectBytes, func() error {
		_, e := codec.Encrypt(pt, key)
		return e
	})
	cal.DecryptBytesPerSecPerCore = throughput(opts.AEADWindow, opts.AEADObjectBytes, func() error {
		_, e := codec.Decrypt(env, key)
		return e
	})

	// --- Per-blob key unwrap (real envelope.WrapKey/UnwrapKey).
	mk := mustRand(envelope.KeySize)
	pbk := mustRand(envelope.KeySize)
	wrapped, err := envelope.WrapKey(mk, pbk)
	if err != nil {
		return cal, fmt.Errorf("calib: wrap: %w", err)
	}
	cal.KeyUnwrapSeconds = latency(opts.WrapIters, func() error {
		_, e := envelope.UnwrapKey(mk, wrapped)
		return e
	})

	// --- Deterministic IPFS import (real internal/ipfs embedded backend).
	importBps, err := measureImport(ctx, opts)
	if err != nil {
		return cal, fmt.Errorf("calib: import: %w", err)
	}
	cal.ImportBytesPerSec = importBps

	cal.Notes = fmt.Sprintf("measured on %s (%d logical cores); AEAD obj=%d B, import obj=%d B ×%d",
		host, cal.Cores, opts.AEADObjectBytes, opts.ImportObjectBytes, opts.ImportIters)
	return cal, nil
}

// throughput runs fn repeatedly for window and returns bytes/sec (objBytes per call).
func throughput(window time.Duration, objBytes int, fn func() error) float64 {
	// brief warm-up
	for i := 0; i < 3; i++ {
		_ = fn()
	}
	var ops int64
	start := time.Now()
	deadline := start.Add(window)
	for time.Now().Before(deadline) {
		// batch to reduce clock-read overhead
		for i := 0; i < 16; i++ {
			if err := fn(); err != nil {
				return 0
			}
			ops++
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(ops) * float64(objBytes) / elapsed
}

// latency times iters calls of fn and returns mean seconds per call.
func latency(iters int, fn func() error) float64 {
	for i := 0; i < 100; i++ {
		_ = fn()
	}
	start := time.Now()
	for i := 0; i < iters; i++ {
		if err := fn(); err != nil {
			return 0
		}
	}
	return time.Since(start).Seconds() / float64(iters)
}

// measureImport drives real deterministic imports through an offline embedded
// Kubo and returns bytes/sec. Each import uses fresh random bytes so CIDs
// differ and the blockstore actually writes (no dedup shortcut).
func measureImport(ctx context.Context, opts Options) (float64, error) {
	repo, err := os.MkdirTemp("", "novasim-calib-repo-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(repo)

	swarmKey := filepath.Join(repo, "swarm.key.src")
	if err := os.WriteFile(swarmKey, []byte(
		"/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		return 0, err
	}

	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmKey,
		Online:       false,
	})
	if err != nil {
		return 0, err
	}
	defer func() {
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = be.Close(sc)
	}()

	bufs := make([][]byte, opts.ImportIters)
	for i := range bufs {
		bufs[i] = mustRand(opts.ImportObjectBytes)
	}

	// One warm import (plugin/repo warm-up) excluded from timing.
	if _, err := be.AddDeterministic(ctx, mustRand(opts.ImportObjectBytes)); err != nil {
		return 0, err
	}

	start := time.Now()
	var total int64
	for _, b := range bufs {
		if _, err := be.AddDeterministic(ctx, b); err != nil {
			return 0, err
		}
		total += int64(len(b))
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, nil
	}
	return float64(total) / elapsed, nil
}

func mustRand(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
