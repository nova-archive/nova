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
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	sum := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(sum)), nil
}

// Verify reports whether plain matches the encoded argon2id hash.
// It is total: no input (including degenerate stored hashes) can cause a panic.
func Verify(encoded, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("password: malformed hash")
	}
	// parts[2] holds "v=19". argon2id v1.3 is the only variant we generate;
	// we intentionally do not branch on the version field today.
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
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

	// Reject degenerate parameters before reaching argon2.IDKey, which panics
	// on zero values. Defense-in-depth: the recover guard below catches any
	// other unexpected panic from the library as well.
	if memory == 0 || timeCost == 0 || threads == 0 {
		return false, errors.New("password: invalid argon2 parameters")
	}
	if len(want) == 0 {
		return false, errors.New("password: empty digest")
	}
	if len(salt) == 0 {
		return false, errors.New("password: empty salt")
	}

	return argon2Compare(plain, salt, want, timeCost, memory, threads)
}

// argon2Compare runs the constant-time compare inside a recover guard so that
// any unexpected argon2 library panic becomes a returned error instead of
// crashing the process.
func argon2Compare(plain string, salt, want []byte, timeCost, memory uint32, threads uint8) (match bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			match, err = false, fmt.Errorf("password: argon2 verify failed: %v", r)
		}
	}()
	got := argon2.IDKey([]byte(plain), salt, timeCost, memory, threads, uint32(len(want)))
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
