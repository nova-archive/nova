package envelope_test

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

// updateVectors regenerates testdata/vectors.json from the seeded RNG.
// Run via `go test ./internal/envelope/... -run TestVectors -update`.
// Review the diff before committing.
var updateVectors = flag.Bool("update", false, "regenerate envelope golden vectors")

type vector struct {
	Name         string `json:"name"`
	MasterKey    string `json:"master_key_hex"`     // 32 bytes hex
	PerBlobKey   string `json:"per_blob_key_hex"`   // 32 bytes hex
	Plaintext    string `json:"plaintext_hex"`      // arbitrary length hex
	WrapSeedHex  string `json:"wrap_seed_hex"`      // 24-byte wrap_nonce hex; deterministic via seeded reader
	EnvelopeSeed string `json:"envelope_seed_hex"`  // 24-byte envelope nonce hex; deterministic via seeded reader
	WrappedKey   string `json:"wrapped_key_hex"`    // 72-byte hex; expected output
	Envelope     string `json:"envelope_hex"`       // arbitrary length hex; expected output
}

// seededReader is a deterministic byte source for vector generation.
type seededReader struct {
	src *rand.ChaCha8
}

func newSeededReader(seedHex string) *seededReader {
	seed := mustHex(seedHex)
	var s [32]byte
	copy(s[:], seed)
	return &seededReader{src: rand.NewChaCha8(s)}
}

func (s *seededReader) Read(p []byte) (int, error) {
	return s.src.Read(p)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestVectorsRoundTrip(t *testing.T) {
	if *updateVectors {
		regenerateVectors(t)
		return
	}

	data, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err, "run with -update to generate")

	var vectors []vector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, v := range vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			mk := mustHex(v.MasterKey)
			pbk := mustHex(v.PerBlobKey)
			plain := mustHex(v.Plaintext)
			wantWrapped := mustHex(v.WrappedKey)
			wantEnvelope := mustHex(v.Envelope)

			// Wrapping is deterministic only when WrapKey reads its nonce
			// from a fixed seed; we test instead that unwrap recovers the
			// per-blob key from the committed wrapped bytes.
			got, err := envelope.UnwrapKey(mk, wantWrapped)
			require.NoError(t, err, "unwrap committed wrapped_key with committed master_key")
			require.Equal(t, pbk, got)

			// Same for envelope decrypt: committed bytes must decrypt to
			// the committed plaintext under the committed key.
			gotPlain, err := envelope.V1().Decrypt(wantEnvelope, pbk)
			require.NoError(t, err, "decrypt committed envelope")
			// Treat nil and empty as equivalent: aead.Open returns nil
			// for empty plaintexts while mustHex("") returns []byte{}.
			require.Equal(t, len(plain), len(gotPlain))
			if len(plain) > 0 {
				require.Equal(t, plain, gotPlain)
			}
		})
	}
}

// regenerateVectors is invoked when `-update` is passed. It writes a
// new testdata/vectors.json with deterministically-seeded random bytes
// for every nonce.
func regenerateVectors(t *testing.T) {
	t.Helper()
	cases := []struct {
		name     string
		seedMK   string
		seedPBK  string
		seedWrap string
		seedEnv  string
		plainHex string
	}{
		{
			name:     "empty_plaintext",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000aa",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000bb",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000cc",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000dd",
			plainHex: "",
		},
		{
			name:     "short_ascii",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000a1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000a2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000a3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000a4",
			plainHex: hex.EncodeToString([]byte("hello, nova\n")),
		},
		{
			name:     "binary_1kib",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000b1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000b2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000b3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000b4",
			plainHex: hex.EncodeToString(deterministicBytes("plain-1kib", 1024)),
		},
		{
			name:     "binary_64kib",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000c1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000c2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000c3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000c4",
			plainHex: hex.EncodeToString(deterministicBytes("plain-64kib", 64*1024)),
		},
	}

	out := make([]vector, 0, len(cases))
	for _, c := range cases {
		mk := drainExactly(newSeededReader(c.seedMK), envelope.KeySize)
		pbk := drainExactly(newSeededReader(c.seedPBK), envelope.KeySize)

		wrapped, err := envelope.WrapKeyWithNonceForTest(mk, pbk, drainExactly(newSeededReader(c.seedWrap), envelope.NonceSize))
		require.NoError(t, err)

		env, err := envelope.V1EncryptWithNonceForTest(mustHex(c.plainHex), pbk, drainExactly(newSeededReader(c.seedEnv), envelope.NonceSize))
		require.NoError(t, err)

		out = append(out, vector{
			Name:         c.name,
			MasterKey:    hex.EncodeToString(mk),
			PerBlobKey:   hex.EncodeToString(pbk),
			Plaintext:    c.plainHex,
			WrapSeedHex:  c.seedWrap,
			EnvelopeSeed: c.seedEnv,
			WrappedKey:   hex.EncodeToString(wrapped),
			Envelope:     hex.EncodeToString(env),
		})
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')

	require.NoError(t, os.MkdirAll(filepath.Join("testdata"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("testdata", "vectors.json"), encoded, 0o644))
	t.Logf("regenerated testdata/vectors.json with %d vectors", len(out))
}

func drainExactly(r io.Reader, n int) []byte {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		panic(err)
	}
	return buf
}

// deterministicBytes produces n bytes seeded from name. Used for
// constructing reproducible "binary" plaintexts in vectors.
func deterministicBytes(name string, n int) []byte {
	var seed [32]byte
	copy(seed[:], name)
	r := rand.NewChaCha8(seed)
	buf := make([]byte, n)
	_, _ = r.Read(buf)
	return buf
}
