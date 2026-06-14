// Package uploadtoken implements the nova_ut_ scoped upload-token credential:
// wire-format helpers, hash utilities, and the auth.Verifier implementation.
package uploadtoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrNotUploadToken is returned by ParseWire when the raw string does not
// carry the nova_ut_ prefix — the token is not ours to validate.
var ErrNotUploadToken = errors.New("uploadtoken: not an upload token")

const prefix = "nova_ut_"

// Generate mints a new upload token, returning:
//
//   - wire: the bearer string to hand to the client (nova_ut_<id>.<secret>)
//   - id:   the token's UUID (stored in the DB as the primary key)
//   - hash: hex(sha256(secret)) — stored in token_hash column
//   - err:  non-nil only on RNG or encoding failure
func Generate() (wire string, id uuid.UUID, hash string, err error) {
	id = uuid.New()
	secret, err := GenerateSecret()
	if err != nil {
		return "", uuid.Nil, "", err
	}
	return BuildWire(id, secret), id, HashSecret(secret), nil
}

// ParseWire decodes a wire-format upload token into its UUID and raw secret.
// Returns ErrNotUploadToken if the raw string does not begin with "nova_ut_".
// Any structural failure after the prefix matches is a plain parse error
// (mine-but-broken → 401).
func ParseWire(raw string) (uuid.UUID, []byte, error) {
	if !strings.HasPrefix(raw, prefix) {
		return uuid.Nil, nil, ErrNotUploadToken
	}

	body := raw[len(prefix):]
	dot := strings.IndexByte(body, '.')
	if dot < 0 {
		return uuid.Nil, nil, fmt.Errorf("uploadtoken: missing '.' separator in token")
	}

	idPart := body[:dot]
	secretPart := body[dot+1:]

	idBytes, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(idPart)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("uploadtoken: decode id part: %w", err)
	}

	id, err := uuid.FromBytes(idBytes)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("uploadtoken: id from bytes: %w", err)
	}

	secret, err := base64.RawURLEncoding.DecodeString(secretPart)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("uploadtoken: decode secret part: %w", err)
	}

	return id, secret, nil
}

// GenerateSecret mints 32 random secret bytes for use with BuildWire and
// HashSecret. Callers who need to obtain a DB-assigned id before building the
// wire token should call GenerateSecret, insert the hash into the DB, then call
// BuildWire with the DB-returned id and the same secret bytes.
func GenerateSecret() ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("uploadtoken: generate secret: %w", err)
	}
	return secret, nil
}

// BuildWire encodes a UUID and raw secret into the nova_ut_ wire format.
// The caller is responsible for storing HashSecret(secret) in the DB.
func BuildWire(id uuid.UUID, secret []byte) string {
	idPart := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(id[:])
	secretPart := base64.RawURLEncoding.EncodeToString(secret)
	return prefix + idPart + "." + secretPart
}

// HashSecret returns hex(sha256(secret)).
// A plain SHA-256 is correct here: the 32-byte random secret is high-entropy
// so a slow KDF (bcrypt/argon2) is not needed.
func HashSecret(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])
}

// EqualHash compares two token hashes in constant time to prevent timing attacks.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
