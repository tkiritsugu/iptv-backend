// Package password hashes and verifies user passwords.
//
// Only the derived key is stored, never the password itself. The encoded form is
// self describing, so the work factor can be raised later without a database
// migration and old hashes keep working until their owner logs in once:
//
//	pbkdf2-sha256$<iterations>$<base64 salt>$<base64 derived key>
//
// PBKDF2-HMAC-SHA256 comes from the standard library (crypto/pbkdf2), which the
// task asks us to prefer over third party code. A memory hard KDF such as
// argon2id is the better choice for a real deployment; README covers the
// trade-off and the one-file swap this format allows.
//
// The work factor is a property of a Hasher rather than a constant baked into
// every call, so it can be tuned per deployment (IPTV_PBKDF2_ITERATIONS) and set
// low in tests. The iteration count travels with the hash, so mixing work
// factors in one database is fine.
package password

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// algorithm names the KDF inside the stored string.
const algorithm = "pbkdf2-sha256"

// DefaultIterations is the work factor used when nothing is configured. On a
// modest CPU this costs roughly 50 ms per verification, which is the intended
// price for an offline guess — and the reason the HTTP layer caches successful
// authentications for a few seconds instead of deriving a key per request.
const DefaultIterations = 150_000

// MinimumIterations is the lowest value the configuration accepts: below it the
// setting looks like a tuning knob but is effectively "store the password in
// plain PBKDF2".
const MinimumIterations = 1_000

const (
	keyLength = 32 // 256 bit derived key
	saltSize  = 16 // 128 bit random salt
)

// ErrInvalidHash reports a stored hash this package cannot parse. It is an
// internal data problem, never a user error, and must not reach the client.
var ErrInvalidHash = errors.New("password: stored hash is not readable")

// MinimumLength is the shortest acceptable password.
const MinimumLength = 8

// Validate rejects passwords too short to resist guessing and too long to be
// anything but an attempt to spend CPU. Long passwords are hashed, not stored,
// so the upper bound exists only to keep a request body from becoming a denial
// of service vector.
func Validate(pw string) error {
	switch l := len([]rune(pw)); {
	case l < MinimumLength:
		return fmt.Errorf("password must be at least %d characters", MinimumLength)
	case l > 1024:
		return fmt.Errorf("password must be at most 1024 characters")
	default:
		return nil
	}
}

// Hasher derives and checks keys at a given work factor.
type Hasher struct {
	Iterations int
}

// Default is the Hasher a deployment gets when it does not configure anything.
var Default = Hasher{}

// New returns a Hasher, falling back to DefaultIterations for a non-positive
// count so a missing setting can never silently produce a cheap hash.
func New(iterations int) Hasher {
	if iterations < 1 {
		return Default
	}
	return Hasher{Iterations: iterations}
}

func (h Hasher) iterations() int {
	if h.Iterations < 1 {
		return DefaultIterations
	}
	return h.Iterations
}

// Hash derives a key from pw with a fresh random salt. Two calls with the same
// password produce different strings, which is the point of the salt.
func (h Hasher) Hash(pw string) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: generate salt: %w", err)
	}
	iterations := h.iterations()
	dk, err := pbkdf2.Key(sha256.New, pw, salt, iterations, keyLength)
	if err != nil {
		return "", fmt.Errorf("password: derive key: %w", err)
	}
	return strings.Join([]string{
		algorithm,
		strconv.Itoa(iterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	}, "$"), nil
}

// Verify reports whether pw matches a stored hash, using the work factor that
// was recorded when the hash was created. The comparison is constant time with
// respect to the derived key.
func (h Hasher) Verify(stored, pw string) (bool, error) {
	algorithmName, iterations, salt, want, err := parse(stored)
	if err != nil {
		return false, err
	}
	if algorithmName != algorithm {
		return false, fmt.Errorf("%w: unknown algorithm %q", ErrInvalidHash, algorithmName)
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iterations, len(want))
	if err != nil {
		return false, fmt.Errorf("password: derive key: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsUpgrade reports whether a stored hash was created with a weaker work
// factor than the one in use. The service re-hashes on successful login — the
// only moment the plaintext is available — which is how the work factor can be
// raised without a batch migration.
func (h Hasher) NeedsUpgrade(stored string) bool {
	_, iterations, _, _, err := parse(stored)
	if err != nil {
		return false
	}
	return iterations < h.iterations()
}

// Burn spends one verification worth of CPU on a throwaway derivation. Callers
// use it when the login does not exist, so that "no such user" cannot be told
// apart from "wrong password" by measuring the response.
func (h Hasher) Burn() {
	// The salt is random and the result discarded; only the cost matters.
	_, _ = h.Hash("timing equaliser - never a real password")
}

func parse(stored string) (alg string, iterations int, salt, key []byte, err error) {
	parts := strings.Split(strings.TrimSpace(stored), "$")
	if len(parts) != 4 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	iterations, err = strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return "", 0, nil, nil, ErrInvalidHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[2]); err != nil {
		return "", 0, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil {
		return "", 0, nil, nil, ErrInvalidHash
	}
	return parts[0], iterations, salt, key, nil
}

// Fingerprint returns a keyed digest of a password for use inside a cache key.
// It keeps plaintext credentials out of maps and logs while still making two
// different passwords of the same login land on different entries. The key is
// per process and never leaves it.
func Fingerprint(key []byte, pw string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(pw))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))[:22]
}
