package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

type v1Codec struct{}

// V1 returns the singleton v1 codec.
func V1() Codec { return v1Codec{} }

func (v1Codec) Version() byte { return VersionV1 }

// Encrypt produces a v1 envelope: NOVE || 0x01 || 0x01 || 0x0000 ||
// nonce(24) || ciphertext || tag(16). The per-blob key MUST be 32 bytes.
// A fresh random nonce is generated each call.
func (v1Codec) Encrypt(plaintext, perBlobKey []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}

	out := make([]byte, HeaderSize, HeaderSize+len(plaintext)+TagSize)
	copy(out[0:4], magic[:])
	out[4] = VersionV1
	out[5] = AlgorithmXChaCha20Poly1305
	// out[6], out[7] are already zero (reserved)
	if _, err := rand.Read(out[8:HeaderSize]); err != nil {
		return nil, fmt.Errorf("envelope v1: rand nonce: %w", err)
	}

	// Seal appends ciphertext+tag to dst.
	nonce := out[8:HeaderSize]
	out = aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt verifies the AEAD tag and returns the plaintext. Returns
// ErrEnvelopeTooShort/ErrEnvelopeBadMagic/ErrEnvelopeUnsupported on
// header trouble, ErrKeyWrongLength on bad key, ErrEnvelopeAuthFailed
// on tag mismatch.
func (v1Codec) Decrypt(env, perBlobKey []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	// We accept callers having pre-validated the header via Decode, but
	// re-validate here because callers may invoke Decrypt directly when
	// they already know the codec.
	if len(env) < HeaderSize+TagSize {
		return nil, ErrEnvelopeTooShort
	}
	if env[0] != magic[0] || env[1] != magic[1] || env[2] != magic[2] || env[3] != magic[3] {
		return nil, ErrEnvelopeBadMagic
	}
	if env[4] != VersionV1 || env[5] != AlgorithmXChaCha20Poly1305 {
		return nil, ErrEnvelopeUnsupported
	}
	if env[6] != 0 || env[7] != 0 {
		return nil, ErrEnvelopeUnsupported
	}

	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}

	nonce := env[8:HeaderSize]
	ctAndTag := env[HeaderSize:]
	plaintext, err := aead.Open(nil, nonce, ctAndTag, nil)
	if err != nil {
		// chacha20poly1305 returns its own opaque error on tag mismatch.
		// We normalise to the sentinel so callers can errors.Is against it.
		return nil, errors.Join(ErrEnvelopeAuthFailed, err)
	}
	return plaintext, nil
}
