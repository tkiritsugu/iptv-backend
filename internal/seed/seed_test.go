package seed

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"iptv/internal/password"
	"iptv/internal/store"
	"iptv/internal/testutil"
)

func newStore(t *testing.T) (*store.Store, *testutil.Clock) {
	t.Helper()
	clock := testutil.NewClock()
	path := filepath.Join(t.TempDir(), "seed.db")
	st, err := store.Open(t.Context(), store.DSN(path), slog.New(slog.DiscardHandler), store.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, clock
}

// The task asks for at least two users and five channels after the first start.
func TestDemoData(t *testing.T) {
	st, clock := newStore(t)
	hasher := password.New(password.MinimumIterations)

	seeded, err := Demo(t.Context(), st, hasher, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Demo: %v", err)
	}
	if !seeded {
		t.Fatal("an empty database was not seeded")
	}

	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) < 2 {
		t.Errorf("%d users, want at least 2", len(users))
	}
	channels, err := st.ListChannels(t.Context())
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(channels) < 5 {
		t.Errorf("%d channels, want at least 5", len(channels))
	}

	// The accounts documented in the README must be the ones that exist.
	for _, want := range []struct{ login, secret string }{
		{"admin", "admin12345"},
		{"viewer", "viewer12345"},
		{"viewer2", "viewer12345"},
		{"blocked", "blocked12345"},
	} {
		user, err := st.UserByLogin(t.Context(), want.login)
		if err != nil {
			t.Fatalf("no seeded user %q: %v", want.login, err)
		}
		ok, err := hasher.Verify(user.PasswordHash, want.secret)
		if err != nil || !ok {
			t.Errorf("user %q does not accept the documented password (ok=%v err=%v)", want.login, ok, err)
		}
	}

	// One switched off channel (the 404 case) and at least one grant of each
	// kind: unlimited, time limited, already expired.
	var inactive, unlimited, limited, expired int
	viewer, err := st.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	subs, err := st.ListSubscriptions(t.Context(), viewer.ID)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	for _, channel := range channels {
		if !channel.Active {
			inactive++
		}
	}
	for _, sub := range subs {
		switch {
		case sub.ExpiresAt == nil:
			unlimited++
		case sub.ExpiresAt.After(clock.Now()):
			limited++
		default:
			expired++
		}
	}
	if inactive == 0 {
		t.Error("no switched off channel to demonstrate the 404 case")
	}
	if unlimited == 0 || limited == 0 {
		t.Errorf("viewer grants: %d unlimited, %d limited — both kinds are expected", unlimited, limited)
	}
	viewer2, err := st.UserByLogin(t.Context(), "viewer2")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	subs2, err := st.ListSubscriptions(t.Context(), viewer2.ID)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	expired = 0
	for _, sub := range subs2 {
		if sub.ExpiresAt != nil && !sub.ExpiresAt.After(clock.Now()) {
			expired++
		}
	}
	if expired == 0 {
		t.Error("viewer2 should have a grant that has already expired")
	}
}

// Restarting the service must not re-insert data or reset a password somebody
// already changed.
func TestSeedRunsOnlyOnAnEmptyDatabase(t *testing.T) {
	st, _ := newStore(t)
	hasher := password.New(password.MinimumIterations)
	logger := slog.New(slog.DiscardHandler)

	if _, err := Demo(t.Context(), st, hasher, logger); err != nil {
		t.Fatalf("first Demo: %v", err)
	}
	viewer, err := st.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	changed := "a-new-password"
	hash, err := hasher.Hash(changed)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if _, err := st.UpdateUser(t.Context(), viewer.ID, store.UserPatch{PasswordHash: &hash}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	second, err := Demo(t.Context(), st, hasher, logger)
	if err != nil {
		t.Fatalf("second Demo: %v", err)
	}
	if second {
		t.Error("Demo reported seeding a database that was not empty")
	}
	after, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(after) != 4 {
		t.Errorf("%d users after the second run, want the original 4", len(after))
	}
	current, err := st.UserByID(t.Context(), viewer.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if ok, _ := hasher.Verify(current.PasswordHash, changed); !ok {
		t.Error("the seeded password overwrote the one the operator changed")
	}
}

// The seed decides expiry through the store clock, so a test that controls time
// controls the demo data as well.
func TestSeedUsesTheStoreClock(t *testing.T) {
	st, clock := newStore(t)
	if _, err := Demo(t.Context(), st, password.New(password.MinimumIterations), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Demo: %v", err)
	}
	viewer, err := st.UserByLogin(t.Context(), "viewer")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	access, err := st.AccessForUser(t.Context(), viewer.ID, false)
	if err != nil {
		t.Fatalf("AccessForUser: %v", err)
	}
	deadline := clock.Now().Add(7 * 24 * time.Hour)
	found := false
	for _, a := range access {
		if a.ExpiresAt != nil && a.ExpiresAt.Equal(deadline) {
			found = true
		}
	}
	if !found {
		t.Error("the time limited demo grant is not anchored to the store clock")
	}
}
