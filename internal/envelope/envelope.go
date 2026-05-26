package envelope

const (
	// HeaderSize is the fixed envelope header length: 4 (magic) +
	// 1 (version) + 1 (algorithm) + 2 (reserved) + 24 (nonce).
	HeaderSize = 32

	// TagSize is the Poly1305 authentication tag length.
	TagSize = 16

	// NonceSize is the XChaCha20 extended-nonce length.
	NonceSize = 24

	// KeySize is the symmetric key length for both per-blob and
	// master keys.
	KeySize = 32

	// WrappedKeySize is wrap_nonce (24) || ct_of_key (32) || tag (16).
	WrappedKeySize = 72

	// VersionV1 = 0x01 per ENCRYPTION_ENVELOPE.md.
	VersionV1 byte = 0x01

	// VersionV2 = 0x02. Reserved for the Phase 2 streaming-AEAD codec.
	// The dispatcher recognises the byte and returns ErrEnvelopeUnsupported
	// in Phase 1 so future v2 envelopes do not silently round-trip as v1.
	VersionV2 byte = 0x02

	// AlgorithmXChaCha20Poly1305 = 0x01.
	AlgorithmXChaCha20Poly1305 byte = 0x01
)

// magic is ASCII "NOVE" — the envelope header marker.
var magic = [4]byte{0x4E, 0x4F, 0x56, 0x45}

// Codec is the abstract operation set every envelope version provides.
// v1 (Phase 1) implements the single-shot variant; v2 (Phase 2) will
// add a streaming Decrypter without changing this interface — the
// streaming variant will be a separate optional interface that v1
// does not implement.
type Codec interface {
	// Version returns the envelope-format version byte this codec emits
	// and accepts. Useful for assertions and audit logging.
	Version() byte

	// Encrypt produces the wire-format envelope for the plaintext under
	// the given per-blob key. The returned bytes are:
	//   header(32) || ciphertext(len(plaintext)) || tag(16)
	// Implementations MUST generate a fresh random nonce per call.
	Encrypt(plaintext, perBlobKey []byte) ([]byte, error)

	// Decrypt verifies and returns the plaintext for envelope bytes under
	// the given per-blob key. Authentication failures return
	// ErrEnvelopeAuthFailed; format failures return one of the
	// ErrEnvelope* sentinels.
	Decrypt(envelope, perBlobKey []byte) ([]byte, error)
}

// Decode parses the envelope header, validates magic/version/algorithm/
// reserved bytes, and returns the version byte plus the Codec capable of
// decrypting the envelope. It does not perform decryption — callers must
// provide the per-blob key separately.
//
// Returns ErrEnvelopeTooShort, ErrEnvelopeBadMagic, or
// ErrEnvelopeUnsupported on header validation failure.
func Decode(b []byte) (version byte, codec Codec, err error) {
	if len(b) < HeaderSize {
		return 0, nil, ErrEnvelopeTooShort
	}
	if !(b[0] == magic[0] && b[1] == magic[1] && b[2] == magic[2] && b[3] == magic[3]) {
		return 0, nil, ErrEnvelopeBadMagic
	}
	v := b[4]
	algo := b[5]
	reserved := uint16(b[6])<<8 | uint16(b[7])
	if reserved != 0 {
		return 0, nil, ErrEnvelopeUnsupported
	}
	switch v {
	case VersionV1:
		if algo != AlgorithmXChaCha20Poly1305 {
			return 0, nil, ErrEnvelopeUnsupported
		}
		return v, V1(), nil
	case VersionV2:
		// Reserved for Phase 2 streaming AEAD. Phase 1 refuses cleanly
		// rather than reaching for a codec that does not exist.
		return 0, nil, ErrEnvelopeUnsupported
	default:
		return 0, nil, ErrEnvelopeUnsupported
	}
}
