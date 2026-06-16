// Package audit is the donor's possession-challenge responder seam: answer a
// coordinator challenge from the LOCAL blockstore only (no lawful in-window
// fetch). M1 ships only the interface; the synchronous responder lands in M6.
package audit

import (
	"context"
	"errors"
)

// ErrNotImplemented marks the M1 stub.
var ErrNotImplemented = errors.New("audit: not implemented until P2-M6")

// Challenge / Response are placeholder shapes refined in M6.
type Challenge struct {
	CID        string
	BlockIndex int64
	Nonce      string
}
type Response struct {
	Digest string
}

// Responder answers possession challenges.
type Responder interface {
	Respond(ctx context.Context, c Challenge) (Response, error)
}

type stub struct{}

// NewStub returns an M1 placeholder Responder.
func NewStub() Responder { return stub{} }

func (stub) Respond(context.Context, Challenge) (Response, error) {
	return Response{}, ErrNotImplemented
}
