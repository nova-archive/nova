package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

var _ Client = (*HTTPClient)(nil)
