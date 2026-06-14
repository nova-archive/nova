// Package upload owns the tus 1.0.0 (Creation + Core) resumable-upload session
// lifecycle: a Postgres upload_sessions row (offset/metadata, the source of
// truth) plus an on-disk chunk file under <dir>/<id>/data. Bytes accumulate on
// disk; finalize hands the assembled plaintext to a Committer (storage.Put).
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Domain errors. The internal/api layer maps these to tus status codes.
var (
	ErrNotFound   = errors.New("upload: session not found")
	ErrConflict   = errors.New("upload: offset conflict")
	ErrIncomplete = errors.New("upload: offset != declared length")
	ErrTooLarge   = errors.New("upload: declared length exceeds max")
)

// Committer is the write surface finalize depends on (storage.Service).
type Committer interface {
	Put(ctx context.Context, r io.Reader, declaredSize int64, pc storage.PutContext) (*storage.PutResult, error)
}

// Store manages tus sessions: DB rows + on-disk chunk files.
type Store struct {
	q       *gen.Queries
	dir     string
	put     Committer
	ttl     time.Duration
	maxSize int64
	locks   *lockmap
}

// NewStore builds a session store. dir is the chunk root (created if absent).
func NewStore(pool *pgxpool.Pool, put Committer, dir string, ttl time.Duration, maxSize int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: mkdir %s: %w", dir, err)
	}
	return &Store{q: gen.New(pool), dir: dir, put: put, ttl: ttl, maxSize: maxSize, locks: newLockmap()}, nil
}

// CreateParams carries validated tus-create metadata.
type CreateParams struct {
	DeclaredLength int64
	MIME           string
	Product        string
	CollectionID   *uuid.UUID
	OwnerID        *uuid.UUID
	UploadTokenID  *uuid.UUID // optional; when set, the session is linked to this upload token
}

// Session is the offset/metadata view returned to handlers.
type Session struct {
	ID             uuid.UUID
	DeclaredLength int64
	OffsetBytes    int64
	MIME           string
	Product        string
	CollectionID   *uuid.UUID
	OwnerID        *uuid.UUID
	State          string
	BlobCID        string
}

func (s *Store) sessionDir(id uuid.UUID) string { return filepath.Join(s.dir, id.String()) }
func (s *Store) dataPath(id uuid.UUID) string   { return filepath.Join(s.sessionDir(id), "data") }

func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

// Create inserts a session row and prepares its on-disk chunk file.
func (s *Store) Create(ctx context.Context, p CreateParams) (uuid.UUID, error) {
	if p.DeclaredLength > s.maxSize {
		return uuid.Nil, ErrTooLarge
	}
	var mime pgtype.Text
	if p.MIME != "" {
		mime = pgtype.Text{String: p.MIME, Valid: true}
	}
	product := p.Product
	if product == "" {
		product = "raw"
	}
	var owner, col pgtype.UUID
	if p.OwnerID != nil {
		owner = pgUUID(*p.OwnerID)
	}
	if p.CollectionID != nil {
		col = pgUUID(*p.CollectionID)
	}
	id, err := s.q.CreateUploadSession(ctx, gen.CreateUploadSessionParams{
		OwnerID: owner, DeclaredLength: p.DeclaredLength, MimeType: mime,
		Product: gen.BlobProduct(product), CollectionID: col,
		ExpiresAt: time.Now().Add(s.ttl),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload: create session: %w", err)
	}
	sid := uuid.UUID(id.Bytes)
	if p.UploadTokenID != nil {
		if err := s.q.SetUploadSessionToken(ctx, gen.SetUploadSessionTokenParams{
			ID:            id,
			UploadTokenID: pgUUID(*p.UploadTokenID),
		}); err != nil {
			return uuid.Nil, fmt.Errorf("upload: set session token: %w", err)
		}
	}
	if err := os.MkdirAll(s.sessionDir(sid), 0o700); err != nil {
		return uuid.Nil, fmt.Errorf("upload: mkdir session: %w", err)
	}
	f, err := os.OpenFile(s.dataPath(sid), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload: create data file: %w", err)
	}
	_ = f.Close()
	return sid, nil
}

// Get loads a session view. Returns ErrNotFound when absent or aborted.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	row, err := s.q.GetUploadSession(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("upload: get session: %w", err)
	}
	if row.State == "aborted" {
		return nil, ErrNotFound
	}
	sess := &Session{
		ID: id, DeclaredLength: row.DeclaredLength, OffsetBytes: row.OffsetBytes,
		MIME: row.MimeType.String, Product: row.Product, State: row.State,
		BlobCID: row.BlobCid.String,
	}
	if row.CollectionID.Valid {
		c := uuid.UUID(row.CollectionID.Bytes)
		sess.CollectionID = &c
	}
	if row.OwnerID.Valid {
		o := uuid.UUID(row.OwnerID.Bytes)
		sess.OwnerID = &o
	}
	return sess, nil
}

// AppendChunk writes a chunk at clientOffset (idempotent seek-write) and
// advances the DB offset optimistically. Concurrent PATCHes on one session are
// serialized by an in-process lock; the loser gets ErrConflict. No DB
// transaction is held across the byte transfer.
func (s *Store) AppendChunk(ctx context.Context, id uuid.UUID, clientOffset int64, r io.Reader) (int64, error) {
	mu := s.locks.get(id)
	if !mu.TryLock() {
		return 0, ErrConflict
	}
	defer mu.Unlock()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if sess.State != "in_progress" || sess.OffsetBytes != clientOffset {
		return 0, ErrConflict
	}

	f, err := os.OpenFile(s.dataPath(id), os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("upload: open data: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(clientOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("upload: seek: %w", err)
	}
	remaining := sess.DeclaredLength - clientOffset
	n, err := io.Copy(f, io.LimitReader(r, remaining))
	if err != nil {
		return 0, fmt.Errorf("upload: write chunk: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("upload: fsync: %w", err)
	}

	newOffset := clientOffset + n
	rows, err := s.q.AdvanceUploadOffset(ctx, gen.AdvanceUploadOffsetParams{
		ID: pgUUID(id), OffsetBytes: newOffset, OffsetBytes_2: clientOffset,
	})
	if err != nil {
		return 0, fmt.Errorf("upload: advance offset: %w", err)
	}
	if rows == 0 {
		return 0, ErrConflict
	}
	return newOffset, nil
}

// Finalize commits the assembled bytes via the Committer. Idempotent: a second
// finalize of a finalized session returns its result fields (Encrypted is not
// surfaced in the HTTP UploadResult and is left zero on this path).
func (s *Store) Finalize(ctx context.Context, id uuid.UUID) (*storage.PutResult, error) {
	mu := s.locks.get(id)
	mu.Lock()
	defer mu.Unlock()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == "finalized" {
		return &storage.PutResult{
			CID: sess.BlobCID, ByteSize: sess.DeclaredLength, MIME: sess.MIME, Product: sess.Product,
		}, nil
	}
	if sess.OffsetBytes != sess.DeclaredLength {
		return nil, ErrIncomplete
	}

	f, err := os.Open(s.dataPath(id))
	if err != nil {
		return nil, fmt.Errorf("upload: open assembled: %w", err)
	}
	defer f.Close()

	res, err := s.put.Put(ctx, f, sess.DeclaredLength, storage.PutContext{
		MIME: sess.MIME, Product: sess.Product, CollectionID: sess.CollectionID, OwnerID: sess.OwnerID,
	})
	if err != nil {
		return nil, err
	}
	if err := s.q.FinalizeUploadSession(ctx, gen.FinalizeUploadSessionParams{
		ID: pgUUID(id), BlobCid: pgtype.Text{String: res.CID, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("upload: mark finalized: %w", err)
	}
	_ = os.RemoveAll(s.sessionDir(id))
	s.locks.forget(id)
	return res, nil
}

// Abort marks the session aborted and removes its chunk dir.
func (s *Store) Abort(ctx context.Context, id uuid.UUID) error {
	mu := s.locks.get(id)
	mu.Lock()
	defer mu.Unlock()
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.q.AbortUploadSession(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("upload: abort: %w", err)
	}
	_ = os.RemoveAll(s.sessionDir(id))
	s.locks.forget(id)
	return nil
}

// GC removes abandoned in_progress sessions past their TTL. Filesystem cleanup
// precedes the row delete so a crash mid-sweep leaves the row (retried next
// tick), never an orphaned directory.
func (s *Store) GC(ctx context.Context) (int, error) {
	ids, err := s.q.ListExpiredUploadSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("upload: list expired: %w", err)
	}
	n := 0
	for _, pgid := range ids {
		id := uuid.UUID(pgid.Bytes)
		_ = os.RemoveAll(s.sessionDir(id))
		if err := s.q.DeleteUploadSession(ctx, pgid); err != nil {
			return n, fmt.Errorf("upload: delete session: %w", err)
		}
		s.locks.forget(id)
		n++
	}
	return n, nil
}
