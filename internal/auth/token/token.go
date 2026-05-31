package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Mint holds the caller's request to mint an access token.
type Mint struct {
	Subject  string
	Role     string
	Issuer   string
	Audience string
	TTL      time.Duration
}

// AccessClaims extends jwt.Claims with a custom role field.
type AccessClaims struct {
	jwt.Claims
	Role string `json:"role"`
}

// Signer mints EdDSA-signed access JWTs.
type Signer struct {
	priv ed25519.PrivateKey
	kid  string
}

// NewSignerFromSeed constructs a Signer from a hex-encoded 32-byte seed.
func NewSignerFromSeed(hexSeed string) (*Signer, error) {
	raw, err := hex.DecodeString(hexSeed)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.SeedSize {
		return nil, errors.New("token: seed must be 32 bytes")
	}
	priv := ed25519.NewKeyFromSeed(raw)
	pub := priv.Public().(ed25519.PublicKey)
	kid := hex.EncodeToString(pub[:8])
	return &Signer{priv: priv, kid: kid}, nil
}

// KID returns the key ID (first 8 bytes of public key, hex-encoded).
func (s *Signer) KID() string { return s.kid }

// Public returns the Ed25519 public key.
func (s *Signer) Public() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// Sign mints a signed compact JWT from the provided Mint request.
func (s *Signer) Sign(m Mint) (string, error) {
	now := time.Now()
	claims := AccessClaims{
		Claims: jwt.Claims{
			Issuer:   m.Issuer,
			Subject:  m.Subject,
			Audience: jwt.Audience{m.Audience},
			IssuedAt: jwt.NewNumericDate(now),
			Expiry:   jwt.NewNumericDate(now.Add(m.TTL)),
			ID:       randID(),
		},
		Role: m.Role,
	}

	sig, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: jose.EdDSA,
			Key:       jose.JSONWebKey{Key: s.priv, KeyID: s.kid},
		},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", err
	}

	return jwt.Signed(sig).Claims(claims).Serialize()
}

// JWKS returns the JSON-encoded JWK Set containing the public key.
func (s *Signer) JWKS() ([]byte, error) {
	return json.Marshal(jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       s.Public(),
				KeyID:     s.kid,
				Algorithm: "EdDSA",
				Use:       "sig",
			},
		},
	})
}

// Verifier verifies EdDSA access JWTs.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier creates a Verifier that trusts the given kid/public-key pair.
func NewVerifier(kid string, pub ed25519.PublicKey) *Verifier {
	return &Verifier{keys: map[string]ed25519.PublicKey{kid: pub}}
}

// Verify parses and validates a compact JWT, returning the AccessClaims on success.
func (v *Verifier) Verify(raw string) (AccessClaims, error) {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.EdDSA})
	if err != nil {
		return AccessClaims{}, err
	}

	kid := tok.Headers[0].KeyID
	pub, ok := v.keys[kid]
	if !ok {
		return AccessClaims{}, errors.New("token: unknown key ID: " + kid)
	}

	var claims AccessClaims
	if err := tok.Claims(pub, &claims); err != nil {
		return AccessClaims{}, err
	}

	if err := claims.ValidateWithLeeway(jwt.Expected{Time: time.Now()}, 0); err != nil {
		return AccessClaims{}, err
	}

	return claims, nil
}

// randID returns a 16-byte random value hex-encoded (32 chars).
func randID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("token: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
