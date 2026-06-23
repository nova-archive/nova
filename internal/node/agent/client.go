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
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
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

var _ Client = (*HTTPClient)(nil)
