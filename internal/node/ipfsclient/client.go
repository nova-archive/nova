// Package ipfsclient is the donor's Kubo-sidecar blockstore client over the
// loopback HTTP API (D-M4-10). It mirrors internal/ipfs.EmbeddedBackend's
// deterministic import EXACTLY — same raw/dag-pb branch on importspec, same
// params — so the donor's root CIDs match the coordinator's bit-for-bit. The
// donor NEVER embeds Kubo; cmd/node must not import internal/ipfs.
package ipfsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/nova-archive/nova/internal/ipfs/importspec"
)

type Client struct {
	api string
	hc  *http.Client
}

func New(apiAddr string) *Client { return &Client{api: apiAddr, hc: &http.Client{}} }

func (c *Client) post(ctx context.Context, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.hc.Do(req)
}

// AddDeterministic imports envelope with IMPORT_RULES params + pin, branching
// EXACTLY like EmbeddedBackend.AddDeterministic (embedded.go:231): raw-codec
// single block at/under the threshold, dag-pb UnixFS above it. Returns the root
// CID string.
func (c *Client) AddDeterministic(ctx context.Context, envelope []byte) (string, error) {
	if importspec.ShouldUseRawCodec(int64(len(envelope))) {
		return c.blockPutRaw(ctx, envelope)
	}
	return c.unixfsAdd(ctx, envelope)
}

// blockPutRaw mirrors addRaw: Block().Put(Format("raw"), Hash(sha2-256), Pin).
func (c *Client) blockPutRaw(ctx context.Context, envelope []byte) (string, error) {
	q := url.Values{"cid-codec": {importspec.CodecRaw}, "mhtype": {importspec.HashAlg}, "pin": {"true"}}
	resp, err := c.post(ctx, "/api/v0/block/put", q, bytes.NewReader(envelope), "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipfsclient: block put status %d", resp.StatusCode)
	}
	var out struct{ Key string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("ipfsclient: empty block-put key")
	}
	return out.Key, nil
}

// unixfsAdd mirrors addDagPB: Unixfs().Add(CidVersion 1, sha2-256, raw-leaves,
// size-262144 chunker, balanced layout (the /add default), pin).
func (c *Client) unixfsAdd(ctx context.Context, envelope []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "blob")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(envelope); err != nil {
		return "", err
	}
	mw.Close()
	q := url.Values{
		"chunker": {importspec.ChunkerSpec}, "cid-version": {"1"},
		"raw-leaves": {"true"}, "hash": {importspec.HashAlg}, "pin": {"true"},
	}
	resp, err := c.post(ctx, "/api/v0/add", q, &body, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipfsclient: add status %d", resp.StatusCode)
	}
	var out struct{ Hash string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Hash == "" {
		return "", fmt.Errorf("ipfsclient: empty add hash")
	}
	return out.Hash, nil
}

// Has reports whether the CID is RECURSIVELY PINNED (not merely present),
// mirroring EmbeddedBackend.Has (embedded.go:343). A non-200 from pin/ls means
// not pinned.
func (c *Client) Has(ctx context.Context, cidStr string) (bool, error) {
	q := url.Values{"arg": {cidStr}, "type": {"recursive"}}
	resp, err := c.post(ctx, "/api/v0/pin/ls", q, nil, "")
	if err != nil {
		return false, err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// Unpin removes the recursive pin (Pin().Rm) so Kubo GC can reclaim (D-M4-5).
func (c *Client) Unpin(ctx context.Context, cidStr string) error {
	resp, err := c.post(ctx, "/api/v0/pin/rm", url.Values{"arg": {cidStr}}, nil, "")
	if err != nil {
		return err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ipfsclient: pin rm status %d", resp.StatusCode)
	}
	return nil
}

// RepoStoredBytes returns the Kubo repo size in bytes for storage accounting.
func (c *Client) RepoStoredBytes(ctx context.Context) (int64, error) {
	resp, err := c.post(ctx, "/api/v0/repo/stat", nil, nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct{ RepoSize int64 }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.RepoSize, nil
}
