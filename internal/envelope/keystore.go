package envelope

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Keystore holds the operator's master keys (one per version label)
// in process memory and exposes Wrap/Unwrap that record and resolve
// the master_key_versions.id for each wrapped per-blob key.
//
// Lifecycle:
//  1. NewKeystoreFromEnv reads NOVA_MASTER_KEY_<LABEL> + NOVA_MASTER_KEY_ACTIVE.
//  2. Bootstrap(ctx) inserts the active version's master_key_versions
//     row if it does not already exist (idempotent), and caches the
//     label → id mapping for every version it finds in the DB.
//  3. Wrap(perBlobKey) wraps under the active version and returns the
//     wrapped bytes + the active version's id.
//  4. Unwrap(wrapped, id) looks up the version by id and unwraps with
//     the matching in-memory master key.
type Keystore struct {
	pool        *pgxpool.Pool
	masters     map[string][]byte    // label → 32-byte master key
	versionByID map[uuid.UUID]string // master_key_versions.id → label
	idByLabel   map[string]uuid.UUID // label → master_key_versions.id
	activeLabel string
}

// NewKeystoreFromEnv parses NOVA_MASTER_KEY_<LABEL> entries from the
// process environment (where <LABEL> is uppercased — V1, V2, V2026Q2)
// and NOVA_MASTER_KEY_ACTIVE selects the default. Labels are stored
// lowercase in the keystore and in master_key_versions.version_label.
//
// At least one NOVA_MASTER_KEY_<LABEL> matching ACTIVE must be set, or
// the constructor returns an error.
func NewKeystoreFromEnv(pool *pgxpool.Pool) (*Keystore, error) {
	active := strings.TrimSpace(os.Getenv("NOVA_MASTER_KEY_ACTIVE"))
	if active == "" {
		return nil, errors.New("keystore: NOVA_MASTER_KEY_ACTIVE is required")
	}
	active = strings.ToLower(active)

	masters := make(map[string][]byte)
	for _, e := range os.Environ() {
		const prefix = "NOVA_MASTER_KEY_"
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			continue
		}
		key := e[:eq]
		val := strings.TrimSpace(e[eq+1:])
		label := strings.ToLower(strings.TrimPrefix(key, prefix))
		if label == "active" || label == "file" || strings.HasSuffix(label, "_file") {
			continue
		}
		if val == "" {
			continue
		}
		raw, err := hex.DecodeString(val)
		if err != nil {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s is not valid hex: %w", strings.ToUpper(label), err)
		}
		if len(raw) != KeySize {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s must be %d bytes (got %d)", strings.ToUpper(label), KeySize, len(raw))
		}
		masters[label] = raw
	}

	if _, ok := masters[active]; !ok {
		return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_ACTIVE=%s but NOVA_MASTER_KEY_%s is not set", active, strings.ToUpper(active))
	}

	return &Keystore{
		pool:        pool,
		masters:     masters,
		versionByID: make(map[uuid.UUID]string),
		idByLabel:   make(map[string]uuid.UUID),
		activeLabel: active,
	}, nil
}

// ActiveLabel returns the active version label (lowercase).
func (k *Keystore) ActiveLabel() string { return k.activeLabel }

// Bootstrap ensures master_key_versions has a row for every label in
// k.masters that is not yet recorded. The active row is created with
// state='active'; non-active labels are not auto-inserted (operators
// rotate via novactl in M10 which sets state explicitly). Returns the
// active version's id.
//
// Idempotent. Safe to call from multiple processes concurrently.
func (k *Keystore) Bootstrap(ctx context.Context) (uuid.UUID, error) {
	// 1. Load every existing row.
	if err := k.loadVersions(ctx); err != nil {
		return uuid.Nil, err
	}
	// 2. Insert the active label if absent.
	if id, ok := k.idByLabel[k.activeLabel]; ok {
		return id, nil
	}
	var id uuid.UUID
	err := k.pool.QueryRow(ctx, `
		INSERT INTO master_key_versions (version_label, state)
		VALUES ($1, 'active')
		ON CONFLICT (version_label) DO UPDATE SET version_label = EXCLUDED.version_label
		RETURNING id
	`, k.activeLabel).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("keystore: insert master_key_versions: %w", err)
	}
	k.idByLabel[k.activeLabel] = id
	k.versionByID[id] = k.activeLabel
	return id, nil
}

func (k *Keystore) loadVersions(ctx context.Context) error {
	rows, err := k.pool.Query(ctx, `SELECT id, version_label FROM master_key_versions`)
	if err != nil {
		return fmt.Errorf("keystore: load versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("keystore: scan version: %w", err)
		}
		label = strings.ToLower(label)
		k.idByLabel[label] = id
		k.versionByID[id] = label
	}
	return rows.Err()
}

// Wrap encrypts perBlobKey under the active master key and returns the
// 72-byte wrapped payload plus the active master_key_versions.id.
// Bootstrap must have been called at least once.
func (k *Keystore) Wrap(perBlobKey []byte) ([]byte, uuid.UUID, error) {
	mk, ok := k.masters[k.activeLabel]
	if !ok {
		return nil, uuid.Nil, fmt.Errorf("keystore: active master key %q not loaded", k.activeLabel)
	}
	id, ok := k.idByLabel[k.activeLabel]
	if !ok {
		return nil, uuid.Nil, errors.New("keystore: Bootstrap must be called before Wrap")
	}
	wrapped, err := WrapKey(mk, perBlobKey)
	if err != nil {
		return nil, uuid.Nil, err
	}
	return wrapped, id, nil
}

// Unwrap decrypts wrapped under the master key recorded by versionID.
// Returns ErrEnvelopeAuthFailed if the master key has been rotated
// away and is no longer loaded (operators must keep prior versions in
// env until rotation completes for every wrapped key).
func (k *Keystore) Unwrap(wrapped []byte, versionID uuid.UUID) ([]byte, error) {
	label, ok := k.versionByID[versionID]
	if !ok {
		// Not in cache; re-load and try again.
		if err := k.loadVersions(context.Background()); err != nil {
			return nil, err
		}
		label, ok = k.versionByID[versionID]
		if !ok {
			return nil, fmt.Errorf("keystore: master_key_versions.id %s not found", versionID)
		}
	}
	mk, ok := k.masters[label]
	if !ok {
		return nil, fmt.Errorf("keystore: master key for version %q is not loaded (env missing?)", label)
	}
	return UnwrapKey(mk, wrapped)
}

// Force the import to be referenced even if we move queries to a sqlc
// build later. Keep the import quiet by referencing pgx through the
// pgxpool, but expose a no-op alias for static analysers.
var _ = pgx.ErrNoRows
