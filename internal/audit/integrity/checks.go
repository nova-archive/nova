package integrity

import (
	"context"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

// Backend is the subset of ipfs.Backend the integrity checks need. It is kept
// narrow so tests can supply a fake; the embedded backend satisfies it.
type Backend interface {
	Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error)
	Has(ctx context.Context, c cid.Cid) (bool, error)
	BlockGet(ctx context.Context, c cid.Cid) ([]byte, error)
}

// Check runs one audit_kind against a sample of up to sampleSize rows and
// returns one Finding per sampled item. A returned error is an infrastructure
// failure (e.g. the sampling query failed), distinct from a per-item fail
// Finding.
type Check interface {
	Kind() Kind
	Run(ctx context.Context, sampleSize int) ([]Finding, error)
}

// defaultMaxDecryptBytes caps the per-blob work of sample_decrypt. v1 is
// single-shot AEAD, so the whole sampled envelope is decrypted; larger blobs
// are recorded skip (still covered by the cheap envelope_decode + key_unwrap).
const defaultMaxDecryptBytes int64 = 8 << 20 // 8 MiB

// NewChecks builds the seven checks over the given dependencies.
func NewChecks(q *gen.Queries, backend Backend, ks *envelope.Keystore) map[Kind]Check {
	d := &checkDeps{q: q, backend: backend, ks: ks, maxDecryptBytes: defaultMaxDecryptBytes}
	return map[Kind]Check{
		KindEnvelopeDecode:            funcCheck{KindEnvelopeDecode, d.envelopeDecode},
		KindKeyUnwrap:                 funcCheck{KindKeyUnwrap, d.keyUnwrap},
		KindSampleDecrypt:             funcCheck{KindSampleDecrypt, d.sampleDecrypt},
		KindKuboPinPresent:            funcCheck{KindKuboPinPresent, d.kuboPinPresent},
		KindDerivativeStateConsistent: funcCheck{KindDerivativeStateConsistent, d.derivativeStateConsistent},
		KindBlockHashValid:            funcCheck{KindBlockHashValid, d.blockHashValid},
		KindManifestConsistent:        funcCheck{KindManifestConsistent, d.manifestConsistent},
	}
}

type checkDeps struct {
	q               *gen.Queries
	backend         Backend
	ks              *envelope.Keystore
	maxDecryptBytes int64
}

// funcCheck adapts a kind + run closure to the Check interface.
type funcCheck struct {
	kind Kind
	run  func(ctx context.Context, sampleSize int) ([]Finding, error)
}

func (c funcCheck) Kind() Kind                                        { return c.kind }
func (c funcCheck) Run(ctx context.Context, n int) ([]Finding, error) { return c.run(ctx, n) }

func pass(cid string) Finding         { return Finding{CID: cid, Result: ResultPass} }
func fail(cid, detail string) Finding { return Finding{CID: cid, Result: ResultFail, Detail: detail} }
func skip(cid, detail string) Finding { return Finding{CID: cid, Result: ResultSkip, Detail: detail} }

// envelope_decode: sampled blob bytes parse as a valid envelope header
// (magic, version, algorithm, reserved-zero, nonce length).
func (d *checkDeps) envelopeDecode(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleEncryptedBlobs(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample encrypted blobs: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, d.checkEnvelopeHeader(ctx, r.Cid))
	}
	return out, nil
}

func (d *checkDeps) checkEnvelopeHeader(ctx context.Context, cidStr string) Finding {
	c, err := cid.Decode(cidStr)
	if err != nil {
		return fail(cidStr, "decode cid: "+err.Error())
	}
	rc, err := d.backend.Get(ctx, c)
	if err != nil {
		return fail(cidStr, "backend get: "+err.Error())
	}
	hdr := make([]byte, envelope.HeaderSize)
	_, rerr := io.ReadFull(rc, hdr)
	_ = rc.Close()
	if rerr != nil {
		return fail(cidStr, "read header: "+rerr.Error())
	}
	if _, _, derr := envelope.Decode(hdr); derr != nil {
		return fail(cidStr, "decode envelope: "+derr.Error())
	}
	return pass(cidStr)
}

// key_unwrap: the blob's DEK unwraps with its recorded master-key version.
func (d *checkDeps) keyUnwrap(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleEncryptedBlobs(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample encrypted blobs: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, d.checkKeyUnwrap(ctx, r.Cid))
	}
	return out, nil
}

func (d *checkDeps) checkKeyUnwrap(ctx context.Context, cidStr string) Finding {
	dek, err := d.q.GetDEKByBlob(ctx, cidStr)
	if err != nil {
		return fail(cidStr, "get dek: "+err.Error())
	}
	if dek.State == "shredded" {
		return skip(cidStr, "dek shredded")
	}
	mkv, err := uuid.Parse(dek.MasterKeyVersionID)
	if err != nil {
		return fail(cidStr, "parse master key version: "+err.Error())
	}
	if _, err := d.ks.Unwrap(ctx, dek.WrappedKey, mkv); err != nil {
		return fail(cidStr, "unwrap: "+err.Error())
	}
	return pass(cidStr)
}

// sample_decrypt: the full envelope decrypts and the AEAD tag verifies.
func (d *checkDeps) sampleDecrypt(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleEncryptedBlobs(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample encrypted blobs: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, d.checkSampleDecrypt(ctx, r.Cid, r.ByteSize))
	}
	return out, nil
}

func (d *checkDeps) checkSampleDecrypt(ctx context.Context, cidStr string, byteSize int64) Finding {
	if byteSize > d.maxDecryptBytes {
		return skip(cidStr, "over sample_decrypt size cap")
	}
	dek, err := d.q.GetDEKByBlob(ctx, cidStr)
	if err != nil {
		return fail(cidStr, "get dek: "+err.Error())
	}
	if dek.State == "shredded" {
		return skip(cidStr, "dek shredded")
	}
	mkv, err := uuid.Parse(dek.MasterKeyVersionID)
	if err != nil {
		return fail(cidStr, "parse master key version: "+err.Error())
	}
	key, err := d.ks.Unwrap(ctx, dek.WrappedKey, mkv)
	if err != nil {
		return fail(cidStr, "unwrap: "+err.Error())
	}
	c, err := cid.Decode(cidStr)
	if err != nil {
		return fail(cidStr, "decode cid: "+err.Error())
	}
	rc, err := d.backend.Get(ctx, c)
	if err != nil {
		return fail(cidStr, "backend get: "+err.Error())
	}
	env, rerr := io.ReadAll(rc)
	_ = rc.Close()
	if rerr != nil {
		return fail(cidStr, "read envelope: "+rerr.Error())
	}
	_, codec, err := envelope.Decode(env)
	if err != nil {
		return fail(cidStr, "decode envelope: "+err.Error())
	}
	if _, err := codec.Decrypt(env, key); err != nil {
		return fail(cidStr, "decrypt: "+err.Error())
	}
	return pass(cidStr)
}

// kubo_pin_present: the local Kubo daemon reports the CID as pinned.
func (d *checkDeps) kuboPinPresent(ctx context.Context, n int) ([]Finding, error) {
	cids, err := d.q.SampleActiveBlobs(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample active blobs: %w", err)
	}
	out := make([]Finding, 0, len(cids))
	for _, cidStr := range cids {
		out = append(out, d.checkPin(ctx, cidStr))
	}
	return out, nil
}

func (d *checkDeps) checkPin(ctx context.Context, cidStr string) Finding {
	c, err := cid.Decode(cidStr)
	if err != nil {
		return fail(cidStr, "decode cid: "+err.Error())
	}
	ok, err := d.backend.Has(ctx, c)
	if err != nil {
		return fail(cidStr, "has: "+err.Error())
	}
	if !ok {
		return fail(cidStr, "not pinned")
	}
	return pass(cidStr)
}

// derivative_state_consistent: a derivative is not more available than its
// parent. Phase 1 (no state-change timestamp) samples derivatives and fails
// when the parent is no longer active but the derivative still is.
func (d *checkDeps) derivativeStateConsistent(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleDerivatives(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample derivatives: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		if r.ParentState != "active" && r.State == "active" {
			out = append(out, fail(r.Cid, "parent state "+r.ParentState+" but derivative active"))
			continue
		}
		out = append(out, pass(r.Cid))
	}
	return out, nil
}

// block_hash_valid: a recorded block_cid, refetched and rehashed, matches.
func (d *checkDeps) blockHashValid(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleMultiBlockBlocks(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample blocks: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, d.checkBlockHash(ctx, r.BlobCid, r.BlockCid))
	}
	return out, nil
}

func (d *checkDeps) checkBlockHash(ctx context.Context, blobCID, blockCID string) Finding {
	stored, err := cid.Decode(blockCID)
	if err != nil {
		return fail(blobCID, "decode block cid: "+err.Error())
	}
	raw, err := d.backend.BlockGet(ctx, stored)
	if err != nil {
		return fail(blobCID, "block get "+blockCID+": "+err.Error())
	}
	got, err := stored.Prefix().Sum(raw)
	if err != nil {
		return fail(blobCID, "rehash: "+err.Error())
	}
	if !got.Equals(stored) {
		return fail(blobCID, "block hash mismatch for "+blockCID)
	}
	return pass(blobCID)
}

// manifest_consistent: blob_manifests.block_count and envelope_size match the
// aggregate of the blob's blob_blocks rows.
func (d *checkDeps) manifestConsistent(ctx context.Context, n int) ([]Finding, error) {
	rows, err := d.q.SampleManifestConsistency(ctx, int32(n))
	if err != nil {
		return nil, fmt.Errorf("integrity: sample manifests: %w", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		if int64(r.BlockCount) != r.ActualCount || r.EnvelopeSize != r.ActualSize {
			out = append(out, fail(r.Cid, fmt.Sprintf(
				"manifest block_count=%d blocks=%d envelope_size=%d sum=%d",
				r.BlockCount, r.ActualCount, r.EnvelopeSize, r.ActualSize)))
			continue
		}
		out = append(out, pass(r.Cid))
	}
	return out, nil
}
