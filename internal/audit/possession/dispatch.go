package possession

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

type Outcome int

const (
	// OutcomeUnknown is deliberately the ZERO VALUE and is never a decision:
	// classify() records it as a skip. P2-M7 cross-version drill finding —
	// OutcomePass used to be the zero value, so a discarded dispatch error
	// left a zero DispatchResult that was recorded as a PASS (reputation up,
	// transcript over nil bytes) without the donor ever being challenged.
	OutcomeUnknown Outcome = iota
	OutcomePass
	OutcomeFailNotPresent
	OutcomeFailMismatch
	OutcomeFailDeadline
	OutcomeSkipBudget
	OutcomeSkipUnreachable // pre-dispatch connection/TLS failure: donor may never have been challenged
)

type DispatchResult struct {
	Outcome    Outcome
	Bytes      []byte
	ReceivedAt time.Time
	LatencyMS  int
}

// Dispatcher POSTs a synchronous audit challenge to a donor's inbound source
// server over coordinator-identity mTLS and verifies the returned block bytes by
// reconstructing the CID from the stored prefix (D-M6-3). No repair token.
type Dispatcher struct {
	hc  *http.Client
	now func() time.Time
}

func NewDispatcher(clientTLS *tls.Config) *Dispatcher {
	return &Dispatcher{hc: &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}, now: time.Now}
}

const maxAuditResp = 1 << 20 // 1 MiB ceiling on any returned block (importspec leaves <= 256 KiB)

func (d *Dispatcher) Challenge(ctx context.Context, addr string, ch wire.AuditChallenge) (DispatchResult, error) {
	if ch.BlockSize <= 0 || ch.BlockSize > maxAuditResp { // sanity ceiling; scheduler also filters over-cap blocks
		return DispatchResult{Outcome: OutcomeFailMismatch}, nil
	}
	ch.ChallengeKind = wire.AuditChallengeKindBlockHash
	body, _ := json.Marshal(ch)
	// source_nebula_addr is host:port (what donors register/heartbeat) — the
	// scheme is prepended HERE, matching the donor-read fetcher's convention.
	// P2-M7 cross-version drill finding: building the URL from the bare
	// host:port made NewRequest fail on EVERY production challenge, and the
	// error path then fabricated passes (see the zero-value note on Outcome).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/fed/v1/audit/challenge", bytes.NewReader(body))
	if err != nil {
		return DispatchResult{Outcome: OutcomeSkipUnreachable}, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := d.now()
	resp, err := d.hc.Do(req)
	if err != nil {
		// Distinguish "donor was challenged but missed the deadline" (fail) from
		// "could not reach the donor" (skip, no reputation movement): a deadline
		// exceedance after the request began is a fail; a dial/TLS/no-route error
		// before any response is unreachable.
		if errors.Is(err, context.DeadlineExceeded) {
			return DispatchResult{Outcome: OutcomeFailDeadline}, nil
		}
		return DispatchResult{Outcome: OutcomeSkipUnreachable}, nil
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return DispatchResult{Outcome: OutcomeSkipBudget, ReceivedAt: d.now()}, nil
	case http.StatusNotFound:
		return DispatchResult{Outcome: OutcomeFailNotPresent, ReceivedAt: d.now()}, nil
	case http.StatusOK:
		// fall through
	default:
		return DispatchResult{Outcome: OutcomeFailNotPresent, ReceivedAt: d.now()}, nil
	}
	// Read EXACTLY the expected body (+1 to detect over-length), THEN stamp
	// received_at — so a slow-body donor is judged against the deadline after the
	// full read (D-M6-15 #4 late-body), and received_at reflects true completion.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ch.BlockSize+1))
	received := d.now()
	if err != nil {
		return DispatchResult{Outcome: OutcomeFailDeadline, ReceivedAt: received}, nil
	}
	if int64(len(raw)) != ch.BlockSize {
		return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
	}
	// Primary verifier: reconstruct the CID from the stored prefix and compare.
	stored, err := cid.Decode(ch.BlockCID)
	if err != nil {
		return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
	}
	recomputed, err := stored.Prefix().Sum(raw)
	if err != nil || !recomputed.Equals(stored) {
		return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
	}
	return DispatchResult{Outcome: OutcomePass, Bytes: raw, ReceivedAt: received,
		LatencyMS: int(received.Sub(start).Milliseconds())}, nil
}
