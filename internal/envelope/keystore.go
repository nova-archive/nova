package envelope

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/config"
)

// defaultSecretsDir is the base directory for the file-mount fallback of the
// master-key resolver chain (env → _FILE → <dir>/master-key-<label>). It is a
// package var only so tests can redirect it; production never changes it.
var defaultSecretsDir = "/run/secrets"

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

// NewKeystoreFromEnv loads the operator's master keys. NOVA_MASTER_KEY_ACTIVE
// selects the active label (<LABEL> uppercased in env — V1, V2, V2026Q2; stored
// lowercase in the keystore and master_key_versions.version_label).
//
// Each label is resolved through the standard secret precedence (the same chain
// the OIDC signing key uses), so the master key never has to sit in the process
// environment — it can be a Docker/Kubernetes secret mount:
//
//	NOVA_MASTER_KEY_<LABEL>          (inline hex)
//	  → NOVA_MASTER_KEY_<LABEL>_FILE (path to a file holding the hex)
//	  → /run/secrets/master-key-<label>  (default mount path)
//
// The active label is always resolved through the full chain, so the common
// case — drop /run/secrets/master-key-v1, set NOVA_MASTER_KEY_ACTIVE=v1 — works
// with no key material in the environment. Additional (rotation) labels are
// declared by an inline value or a _FILE env. A declared label that resolves
// from no source, or a set-but-unreadable _FILE, is fatal (never silently
// skipped). See REVIEW_2026_05_25.md C3 and THREAT_MODEL.md boundary ③.
func NewKeystoreFromEnv(pool *pgxpool.Pool) (*Keystore, error) {
	active := strings.TrimSpace(os.Getenv("NOVA_MASTER_KEY_ACTIVE"))
	if active == "" {
		return nil, errors.New("keystore: NOVA_MASTER_KEY_ACTIVE is required")
	}
	active = strings.ToLower(active)

	// Build the candidate label set: the active label (always) plus every
	// label declared inline (NOVA_MASTER_KEY_<LABEL>) or by a mount-path env
	// (NOVA_MASTER_KEY_<LABEL>_FILE).
	const prefix = "NOVA_MASTER_KEY_"
	labels := map[string]struct{}{active: {}}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			continue
		}
		rest := e[len(prefix):eq] // <LABEL>, ACTIVE, FILE, or <LABEL>_FILE
		if u := strings.ToUpper(rest); strings.HasSuffix(u, "_FILE") {
			rest = rest[:len(rest)-len("_FILE")]
		}
		// Filter the ACTIVE/FILE pseudo-labels AFTER stripping _FILE so typo'd
		// forms (ACTIVE_FILE, FILE_FILE) don't leak in as phantom labels.
		if rest == "" || strings.EqualFold(rest, "active") || strings.EqualFold(rest, "file") {
			continue
		}
		labels[strings.ToLower(rest)] = struct{}{}
	}

	// Resolve each label through the shared precedence:
	// env NOVA_MASTER_KEY_<LABEL> → NOVA_MASTER_KEY_<LABEL>_FILE → <dir>/master-key-<label>.
	// The mount-path leaf is lowercased (Linux paths are case-sensitive).
	masters := make(map[string][]byte, len(labels))
	for label := range labels {
		up := strings.ToUpper(label)
		val, source, err := config.ResolveSecret(
			prefix+up,
			prefix+up+"_FILE",
			filepath.Join(defaultSecretsDir, "master-key-"+label),
		)
		if err != nil {
			if label == active {
				return nil, fmt.Errorf("keystore: active master key %q resolves from no source "+
					"($%s%s, $%s%s_FILE, or %s/master-key-%s): %w",
					label, prefix, up, prefix, up, defaultSecretsDir, label, err)
			}
			// A declared (non-active) label that fails to resolve is fatal —
			// never silently drop a key (its wrapped blobs would be unreadable).
			return nil, fmt.Errorf("keystore: declared master key %q failed to resolve: %w", label, err)
		}
		raw, err := hex.DecodeString(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s is not valid hex: %w", up, err)
		}
		if len(raw) != KeySize {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s must be %d bytes (got %d)", up, KeySize, len(raw))
		}
		masters[label] = raw
		// Log the source per label so operators can confirm whether each key
		// came from an inline env, a _FILE redirect, or the default mount.
		// No key bytes are logged. M6.2 B5.
		slog.Info("keystore: master key loaded",
			"label", label, "source", string(source), "active", label == active)
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
//
// ctx is propagated to the cache-miss DB reload path (loadVersions) so a
// shutting-down coordinator can drop late requests promptly instead of
// hanging on a Postgres query that outlives the request lifetime. M6.2
// B6.
func (k *Keystore) Unwrap(ctx context.Context, wrapped []byte, versionID uuid.UUID) ([]byte, error) {
	label, ok := k.versionByID[versionID]
	if !ok {
		// Not in cache; re-load and try again.
		if err := k.loadVersions(ctx); err != nil {
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
