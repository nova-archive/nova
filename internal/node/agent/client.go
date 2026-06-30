package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/transfer"
)

// HTTPClient is the donor's mTLS federation client.
type HTTPClient struct {
	base string
	hc   *http.Client
}

// NewHTTPClient builds an mTLS client targeting coordinatorURL with tlsCfg.
func NewHTTPClient(coordinatorURL string, tlsCfg *tls.Config) *HTTPClient {
	return &HTTPClient{
		base: coordinatorURL,
		hc: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

func (c *HTTPClient) post(ctx context.Context, path string, in, out any, okStatus int) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != okStatus && !(path == "/fed/v1/register" && resp.StatusCode == http.StatusOK) {
		var e wire.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: status %d (%s)", path, resp.StatusCode, e.Code)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *HTTPClient) Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error) {
	var out wire.RegisterResponse
	err := c.post(ctx, "/fed/v1/register", req, &out, http.StatusCreated)
	return out, err
}

func (c *HTTPClient) Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	var out wire.HeartbeatResponse
	err := c.post(ctx, "/fed/v1/heartbeat", req, &out, http.StatusOK)
	return out, err
}

// Sentinels the agent branches on.
var (
	ErrSnapshotRequired     = errors.New("agent: snapshot_required")
	ErrSnapshotEpochChanged = errors.New("agent: snapshot epoch changed")
)

func (c *HTTPClient) GetChanges(ctx context.Context, sinceSeq int64) (wire.ChangesResponse, error) {
	u := fmt.Sprintf("%s/fed/v1/pins/changes?since_seq=%d&limit=%d", c.base, sinceSeq, 1000)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.ChangesResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		var er wire.ErrorResponse
		json.NewDecoder(resp.Body).Decode(&er)
		if er.Code == wire.CodeSnapshotRequired {
			return wire.ChangesResponse{}, ErrSnapshotRequired
		}
		return wire.ChangesResponse{}, fmt.Errorf("changes: %s", er.Code)
	}
	if resp.StatusCode != http.StatusOK {
		return wire.ChangesResponse{}, fmt.Errorf("changes: status %d", resp.StatusCode)
	}
	var out wire.ChangesResponse
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *HTTPClient) GetSnapshot(ctx context.Context, cursor string, epoch int64) (wire.SnapshotResponse, error) {
	u := fmt.Sprintf("%s/fed/v1/pins/snapshot?limit=%d", c.base, 1000)
	if cursor != "" {
		u += "&cursor=" + url.QueryEscape(cursor)
	}
	if epoch > 0 {
		u += "&snapshot_epoch=" + strconv.FormatInt(epoch, 10)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return wire.SnapshotResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return wire.SnapshotResponse{}, ErrSnapshotEpochChanged
	}
	if resp.StatusCode != http.StatusOK {
		return wire.SnapshotResponse{}, fmt.Errorf("snapshot: status %d", resp.StatusCode)
	}
	var out wire.SnapshotResponse
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// ErrStaleAssignment is returned by Ack when the coordinator rejects the
// ack with a 409 stale_assignment (the assignment has been superseded).
var ErrStaleAssignment = errors.New("agent: stale_assignment")

// Ack reports successful replication of cid to the coordinator.
// Returns ErrStaleAssignment on 409 stale_assignment.
func (c *HTTPClient) Ack(ctx context.Context, cid string, a wire.Ack) error {
	err := c.post(ctx, "/fed/v1/pins/"+url.PathEscape(cid)+"/ack", a, nil, http.StatusNoContent)
	if err != nil && strings.Contains(err.Error(), wire.CodeStaleAssignment) {
		return ErrStaleAssignment
	}
	return err
}

// Fail reports a replication failure for cid to the coordinator.
func (c *HTTPClient) Fail(ctx context.Context, cid string, f wire.Fail) error {
	return c.post(ctx, "/fed/v1/pins/"+url.PathEscape(cid)+"/fail", f, nil, http.StatusNoContent)
}

// Fetch fetches ciphertext for cid from the source using its repair token. For a
// coordinator-as-source grant (M4) it targets c.base; for a donor↔donor grant (M5)
// it targets the source donor's advertised address (src.NebulaAddr) over the same
// federation mTLS client (the source donor presents a federation-CA server cert).
// Returns transfer.ErrSourceMissing on 404, transfer.ErrSourceUnauthorized on 403.
func (c *HTTPClient) Fetch(ctx context.Context, src wire.ChangeSource, cid string, _ int64) (io.ReadCloser, error) {
	base := c.base
	if src.NodeID != wire.CoordinatorSourceID && src.NebulaAddr != "" {
		base = "https://" + src.NebulaAddr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/fed/v1/blob/"+url.PathEscape(cid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Nova-Repair-Token", src.Token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, transfer.ErrSourceMissing
	case http.StatusForbidden:
		resp.Body.Close()
		return nil, transfer.ErrSourceUnauthorized
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("fetch %s: status %d", cid, resp.StatusCode)
	}
}

var _ Client = (*HTTPClient)(nil)
