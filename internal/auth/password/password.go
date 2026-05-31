package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB = 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

// Hash returns a PHC-format argon2id encoded hash of plain.
func Hash(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(sum)), nil
}

// Verify reports whether plain matches the encoded argon2id hash.
func Verify(encoded, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("password: malformed hash")
	}
	var mem, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, fmt.Errorf("password: malformed params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("password: bad salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("password: bad digest: %w", err)
	}
	got := argon2.IDKey([]byte(plain), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

var dummyHash string

func init() {
	h, err := Hash("nova-dummy-password-for-timing-equalization")
	if err != nil {
		panic(err) // CSPRNG failure at startup is fatal
	}
	dummyHash = h
}

// DummyVerify spends argon2 time on a static hash so failure branches
// (user-not-found / disabled / nil hash) match the real-verify timing.
func DummyVerify(plain string) { _, _ = Verify(dummyHash, plain) }

// Gate bounds concurrent argon2 computations to protect against memory
// exhaustion under distributed login floods.
type Gate struct{ ch chan struct{} }

// NewGate returns a gate allowing at most n concurrent holders (min 1).
func NewGate(n int) *Gate {
	if n < 1 {
		n = 1
	}
	return &Gate{ch: make(chan struct{}, n)}
}

// TryAcquire grabs a slot without blocking. The returned release frees it
// once; ok=false means the gate is full (caller should 503).
func (g *Gate) TryAcquire() (func(), bool) {
	select {
	case g.ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.ch }) }, true
	default:
		return nil, false
	}
}
