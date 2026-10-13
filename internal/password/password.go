// Package password hashes and verifies user passwords.
//
// Only the derived key is stored, never the password itself. The encoded form
// is self describing so that the work factor can be raised later without a
// database migration:
//
//	pbkdf2-sha256$<iterations>$<base64 salt>$<base64 derived key>
//
// PBKDF2-HMAC-SHA256 comes from the standard library (crypto/pbkdf2), which the
// task requires us to prefer over third party code. A memory hard KDF such as
// argon2id is the better choice for a real deployment; see README for the
// trade-off and for how this package would be swapped (Verify and NeedsUpgrade
// already keep the stored format versioned, so old hashes keep working).
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
	"hash"
	"strconv"
	"strings"
)

const (
	// algorithm names the KDF inside the stored string.
	algorithm = "pbkdf2-sha256"

	// Iterations is the work factor used for newly created hashes. Measured on
	// a modest CPU this costs roughly 50 ms per verification, which is the
	// intended price for an offline guess and the reason the HTTP layer caches
	// successful authentications.
	Iterations = 150_000

	keyLength = 32 // 256 bit derived key
	saltSize  = 16 // 128 bit random salt
)

// ErrInvalidHash reports a stored hash that this package cannot parse. It is an
// internal data problem, never a user error, and must not leak to the client.
var ErrInvalidHash = errors.New("password: stored hash is not readable")

// MinimumLength is the shortest acceptable password.
const MinimumLength = 8

// Validate rejects passwords that are too short or too long to be sane. Long
// passwords are hashed, not stored, so an upper bound only protects the request
// body from being used as a CPU denial of service vector.
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

// Hash derives a key from pw with a fresh random salt.
func Hash(pw string) (string, error) {
	return hashWith(pw, Iterations)
}

// hashWith exists so that tests can afford to create many users quickly. The
// iteration count travels with the hash, so mixed work factors in one database
// are fine.
func hashWith(pw string, iterations int) (string, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: generate salt: %w", err)
	}
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

// Verify reports whether pw matches the stored hash. The comparison is
// constant time with respect to the derived key.
func Verify(stored, pw string) (bool, error) {
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
// factor than the current one. The service re-hashes on successful login, so
// work factors migrate without a batch job.
func NeedsUpgrade(stored string) bool {
	_, iterations, _, _, err := parse(stored)
	if err != nil {
		return false
	}
	return iterations < Iterations
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

// burnHash is a hash of a password nobody can guess, used to spend the same
// amount of CPU on an unknown login as on a known one. Without it a caller can
// tell "no such user" from "wrong password" by timing the response.
var burnHash = mustHash("iptv-timing-equaliser-not-a-real-password")

func mustHash(s string) string {
	h, err := hashWith(s, Iterations)
	if err != nil {
		panic("password: " + err.Error())
	}
	return h
}

// Burn spends one verification worth of CPU on a fixed dummy hash.
func Burn() bool {
	ok, err := Verify(burnHash, "wrong-on-purpose")
	return err == nil && ok
}

// Fingerprint returns a keyed digest of a password for use as part of a cache
// key. It keeps plaintext credentials out of process memory maps and logs while
// still making two different passwords of the same login land on different
// entries.
func Fingerprint(key []byte, pw string) string {
	m := hmac.New(sha256.New, key)
	var h hash.Hash = m
	h.Write([]byte(pw))
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil))[:22]
}
