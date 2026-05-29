package storage

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Visibility is the most-permissive collection visibility a blob has.
// Ordered so a higher value is more permissive.
type Visibility int

const (
	VisibilityPrivate  Visibility = iota // no public/unlisted membership
	VisibilityUnlisted                   // readable anonymously by CID
	VisibilityPublic                     // listed + anonymous
)

func (v Visibility) String() string {
	switch v {
	case VisibilityPublic:
		return "public"
	case VisibilityUnlisted:
		return "unlisted"
	default:
		return "private"
	}
}

// BlobView is the resolved, ready-to-serve description of a blob. The
// exported fields drive headers and JSON metadata; the unexported fields
// carry the key material OpenBytes needs for encrypted blobs.
type BlobView struct {
	CID             string
	MIME            string
	PlaintextSize   int64
	EnvelopeVersion int16
	Product         string
	OwnerID         *string
	UploadedAt      time.Time
	Visibility      Visibility
	Encrypted       bool

	wrappedKey         []byte
	masterKeyVersionID *uuid.UUID
}

// resolveVisibility folds a blob's collection memberships into the single
// most-permissive visibility. No membership ⇒ private.
func resolveVisibility(visibilities []string) Visibility {
	best := VisibilityPrivate
	for _, v := range visibilities {
		switch v {
		case "public":
			return VisibilityPublic
		case "unlisted":
			if best < VisibilityUnlisted {
				best = VisibilityUnlisted
			}
		}
	}
	return best
}

// PutContext carries validated, product-agnostic write metadata for Service.Put.
type PutContext struct {
	MIME         string
	Product      string // blob_product; M4 always "raw"
	CollectionID *uuid.UUID
	OwnerID      *uuid.UUID
	SourceIP     netip.Addr // zero value ⟹ not recorded (paranoid / anonymous)
}

// PutResult reports the committed blob.
type PutResult struct {
	CID       string
	ByteSize  int64
	MIME      string
	Product   string
	Encrypted bool
}
