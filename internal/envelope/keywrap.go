package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// WrapKey encrypts a 32-byte per-blob key with the 32-byte operator
// master key using XChaCha20-Poly1305 with empty AAD. The returned
// 72-byte payload is:
//
//	wrap_nonce(24) || ct_of_per_blob_key(32) || tag(16)
//
// This is the byte layout stored in data_encryption_keys.wrapped_key.
//
// Each call generates a fresh random wrap_nonce; identical inputs MUST
// NOT produce identical wrapped outputs.
func WrapKey(masterKey, perBlobKey []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}

	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}

	out := make([]byte, NonceSize, WrappedKeySize)
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("envelope keywrap: rand nonce: %w", err)
	}

	// Seal appends ct+tag to out, growing it to 24+32+16 = 72 bytes.
	out = aead.Seal(out, out[:NonceSize], perBlobKey, nil)
	if len(out) != WrappedKeySize {
		return nil, fmt.Errorf("envelope keywrap: unexpected wrapped length %d", len(out))
	}
	return out, nil
}

// UnwrapKey reverses WrapKey. Returns ErrEnvelopeAuthFailed when the
// master key is wrong or the wrapped bytes were tampered with.
func UnwrapKey(masterKey, wrapped []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(wrapped) != WrappedKeySize {
		return nil, ErrWrappedKeyWrongLength
	}

	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}

	nonce := wrapped[:NonceSize]
	ctAndTag := wrapped[NonceSize:]
	pbk, err := aead.Open(nil, nonce, ctAndTag, nil)
	if err != nil {
		return nil, errors.Join(ErrEnvelopeAuthFailed, err)
	}
	if len(pbk) != KeySize {
		return nil, fmt.Errorf("envelope keywrap: unexpected unwrapped length %d", len(pbk))
	}
	return pbk, nil
}
