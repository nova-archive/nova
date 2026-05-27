package envelope

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// WrapKeyWithNonceForTest performs WrapKey with a caller-supplied
// nonce. It exists to support deterministic golden-vector generation
// in vectors_test.go and is not used in production code paths.
//
// Production code MUST call WrapKey (which generates its own random
// nonce). Re-using a nonce across two distinct per-blob keys with the
// same master key is a catastrophic AEAD misuse.
func WrapKeyWithNonceForTest(masterKey, perBlobKey, wrapNonce []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(wrapNonce) != NonceSize {
		return nil, fmt.Errorf("envelope keywrap: nonce wrong length %d", len(wrapNonce))
	}
	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}
	out := make([]byte, NonceSize, WrappedKeySize)
	copy(out, wrapNonce)
	out = aead.Seal(out, wrapNonce, perBlobKey, nil)
	if len(out) != WrappedKeySize {
		return nil, errors.New("envelope keywrap: unexpected wrapped length")
	}
	return out, nil
}

// V1EncryptWithNonceForTest performs v1 envelope encryption with a
// caller-supplied nonce. Test-only.
func V1EncryptWithNonceForTest(plaintext, perBlobKey, nonce []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("envelope v1: nonce wrong length %d", len(nonce))
	}
	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}
	out := make([]byte, HeaderSize, HeaderSize+len(plaintext)+TagSize)
	copy(out[0:4], magic[:])
	out[4] = VersionV1
	out[5] = AlgorithmXChaCha20Poly1305
	copy(out[8:HeaderSize], nonce)
	out = aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}
