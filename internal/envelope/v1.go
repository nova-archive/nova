package envelope

// v1Codec implements the single-shot XChaCha20-Poly1305 envelope from
// docs/specs/ENCRYPTION_ENVELOPE.md § "Envelope wire format". The full
// implementation lands in Task 3; this stub exists so the version
// dispatcher in envelope.go links.
type v1Codec struct{}

// V1 returns the singleton v1 codec.
func V1() Codec { return v1Codec{} }

func (v1Codec) Version() byte { return VersionV1 }

func (v1Codec) Encrypt(plaintext, perBlobKey []byte) ([]byte, error) {
	panic("v1Codec.Encrypt: not implemented yet (Task 3)")
}

func (v1Codec) Decrypt(envelope, perBlobKey []byte) ([]byte, error) {
	panic("v1Codec.Decrypt: not implemented yet (Task 3)")
}
