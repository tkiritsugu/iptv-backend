package password

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	hasher := New(1_000) // cheap on purpose: the format is what is under test

	stored, err := hasher.Hash("секретный пароль")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if strings.Contains(stored, "секретный пароль") {
		t.Fatal("stored hash contains the password")
	}
	ok, err := hasher.Verify(stored, "секретный пароль")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}

	ok, err = hasher.Verify(stored, "СЕКРЕТНЫЙ ПАРОЛЬ")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("a different password verified")
	}
}

func TestSaltMakesHashesDistinct(t *testing.T) {
	hasher := New(1_000)
	first, err := hasher.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	second, err := hasher.Hash("same-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password are identical: the salt is missing or fixed")
	}
	for _, stored := range []string{first, second} {
		if ok, _ := hasher.Verify(stored, "same-password"); !ok {
			t.Fatalf("salted hash %q does not verify", stored)
		}
	}
}

// The iteration count travels with the hash, so raising the work factor must
// keep existing hashes valid — that is what makes the upgrade path possible.
func TestOlderWorkFactorStillVerifies(t *testing.T) {
	old := New(1_000)
	current := New(5_000)

	stored, err := old.Hash("password123")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err := current.Verify(stored, "password123")
	if err != nil || !ok {
		t.Fatalf("current hasher rejected an older hash: ok=%v err=%v", ok, err)
	}
	if !current.NeedsUpgrade(stored) {
		t.Error("NeedsUpgrade is false for a weaker hash")
	}
	upgraded, err := current.Hash("password123")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if current.NeedsUpgrade(upgraded) {
		t.Error("NeedsUpgrade is true for a current hash")
	}
	if old.NeedsUpgrade(upgraded) {
		t.Error("a stronger hash must not look like an upgrade candidate")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	hasher := New(1_000)
	for _, stored := range []string{
		"",
		"plaintext",
		"pbkdf2-sha256$notanumber$c2FsdA==$a2V5",
		"pbkdf2-sha256$1000$salt-not-base64!!!$key",
		"argon2id$1000$c2FsdA==$a2V5",
	} {
		if ok, err := hasher.Verify(stored, "password123"); ok || !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Verify(%q) = (%v, %v), want (false, ErrInvalidHash)", stored, ok, err)
		}
	}
}

func TestHasherFallsBackToDefaultIterations(t *testing.T) {
	// A zero Hasher must not quietly produce a single-iteration hash.
	if got := New(0).iterations(); got != DefaultIterations {
		t.Errorf("New(0) iterations = %d, want %d", got, DefaultIterations)
	}
	stored, err := Default.Hash("password123")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(stored, algorithm+"$"+strconv.Itoa(DefaultIterations)+"$") {
		t.Errorf("unexpected hash format %q", stored)
	}
}

func TestValidateLength(t *testing.T) {
	if err := Validate("short"); err == nil {
		t.Error("a 5 character password was accepted")
	}
	if err := Validate(strings.Repeat("p", MinimumLength)); err != nil {
		t.Errorf("the minimum length password was rejected: %v", err)
	}
	if err := Validate(strings.Repeat("p", 2000)); err == nil {
		t.Error("an absurdly long password was accepted")
	}
}

// TestFingerprintIsStableAndKeyed documents the property the auth cache relies
// on: the same password maps to the same key, a different password does not,
// and the password itself is not recoverable or readable from the output.
func TestFingerprintIsStableAndKeyed(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("fedcba9876543210fedcba9876543210")

	if Fingerprint(key, "password123") != Fingerprint(key, "password123") {
		t.Error("fingerprint is not deterministic")
	}
	if Fingerprint(key, "password123") == Fingerprint(key, "password124") {
		t.Error("different passwords share a fingerprint")
	}
	if Fingerprint(key, "password123") == Fingerprint(other, "password123") {
		t.Error("fingerprint does not depend on the key")
	}
	if strings.Contains(Fingerprint(key, "password123"), "password123") {
		t.Error("fingerprint contains the password")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// BenchmarkKDFCost measures the work one password check costs, which is what
// decides how expensive a brute-force attempt is — and why the verified
// login/password pair is cached.
func BenchmarkKDFCost(b *testing.B) {
	h := New(150_000)
	sum, err := h.Hash("viewer12345")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Verify(sum, "viewer12345"); err != nil {
			b.Fatal(err)
		}
	}
}
