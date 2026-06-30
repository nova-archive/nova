package ipfsclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// ErrBlockNotLocal signals the local Kubo blockstore does not hold the block.
// The audit handler maps this to a clean 404 (the lying-donor indication).
var ErrBlockNotLocal = errors.New("ipfsclient: block not present locally")

// BlockGetLocal returns the raw bytes of a single block from the LOCAL Kubo
// blockstore ONLY. It passes offline=true so Kubo never triggers a Bitswap
// network fetch (D-M6-4a); a missing block yields ErrBlockNotLocal, never a
// remote read. Used by the possession-audit responder.
func (c *Client) BlockGetLocal(ctx context.Context, blockCID string) ([]byte, error) {
	q := url.Values{"arg": {blockCID}, "offline": {"true"}}
	resp, err := c.post(ctx, "/api/v0/block/get", q, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, ErrBlockNotLocal
	}
	return io.ReadAll(resp.Body)
}
