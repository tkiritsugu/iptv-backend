package store

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"iptv/internal/testutil"
)

func newStore(t *testing.T) (*Store, *testutil.Clock) {
	t.Helper()
	return openStore(t, filepath.Join(t.TempDir(), "test.db"))
}

// openStore returns a store on the given path so a test can close it and open
// the same file again.
func openStore(t *testing.T, path string) (*Store, *testutil.Clock) {
	t.Helper()
	clock := testutil.NewClock()
	st, err := Open(t.Context(), DSN(path), slog.New(slog.DiscardHandler), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, clock
}

func (s *Store) mustUser(t *testing.T, login string, active bool) User {
	t.Helper()
	user, err := s.CreateUser(t.Context(), NewUser{
		Login:        login,
		PasswordHash: "pbkdf2-sha256$1000$c2FsdQ==$a2V5",
		Active:       active,
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", login, err)
	}
	return user
}

func (s *Store) mustChannel(t *testing.T, number int, name string, active bool) Channel {
	t.Helper()
	channel, err := s.CreateChannel(t.Context(), NewChannel{
		Number:     number,
		Name:       name,
		StreamPath: "/live/" + name + "/index.m3u8",
		Active:     active,
	})
	if err != nil {
		t.Fatalf("CreateChannel(%d): %v", number, err)
	}
	return channel
}

func TestSchemaIsCreatedOnFirstStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iptv.db")

	st, _ := openStore(t, path)

	var version int
	if err := st.DB().QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
	// Counters the cache layer depends on exist from the beginning.
	for _, group := range []string{VersionUsers, VersionCatalog} {
		if v, err := st.Version(t.Context(), group); err != nil || v != 1 {
			t.Errorf("Version(%s) = (%d, %v), want (1, nil)", group, v, err)
		}
	}
}

// Reopening must not recreate or wipe anything: this is what happens on every
// container restart.
func TestReopenKeepsDataAndAppliesNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iptv.db")
	first, _ := openStore(t, path)
	user := first.mustUser(t, "keeper", true)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, _ := openStore(t, path)
	loaded, err := second.UserByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("UserByID after reopen: %v", err)
	}
	if loaded.Login != "keeper" {
		t.Errorf("login = %q, want keeper", loaded.Login)
	}
}

func TestPasswordHashIsStoredVerbatimAndReadBack(t *testing.T) {
	st, _ := newStore(t)
	const hash = "pbkdf2-sha256$1000$c2FsdA==$a2V5"
	created, err := st.CreateUser(t.Context(), NewUser{Login: "hasher", PasswordHash: hash, Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.PasswordHash != hash {
		t.Error("the store changed the hash it was given")
	}
}

func TestDuplicateLoginIsAConflict(t *testing.T) {
	st, _ := newStore(t)
	st.mustUser(t, "viewer", true)

	_, err := st.CreateUser(t.Context(), NewUser{Login: "viewer", PasswordHash: "x", Active: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestUniqueConstraintIsNotLeakedAsAGenericError(t *testing.T) {
	st, _ := newStore(t)
	st.mustChannel(t, 5, "five", true)

	_, err := st.CreateChannel(t.Context(), NewChannel{Number: 5, Name: "dup", StreamPath: "/x", Active: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a conflict must not look like a missing row")
	}
}

func TestUserPatchChangesOnlyWhatIsGiven(t *testing.T) {
	st, _ := newStore(t)
	user := st.mustUser(t, "patchme", true)

	login := "patched"
	active := false
	if _, err := st.UpdateUser(t.Context(), user.ID, UserPatch{Login: &login, Active: &active}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got, err := st.UserByLogin(t.Context(), "patched")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	if got.Active {
		t.Error("is_active was not updated")
	}
	if got.Admin {
		t.Error("is_admin changed without being in the patch")
	}
	if got.PasswordHash != user.PasswordHash {
		t.Error("password hash changed without being in the patch")
	}

	if _, err := st.UpdateUser(t.Context(), 424242, UserPatch{Active: &active}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	// An empty patch is a read, not an error.
	unchanged, err := st.UpdateUser(t.Context(), got.ID, UserPatch{})
	if err != nil || unchanged.Login != "patched" {
		t.Errorf("empty patch = (%v, %v)", unchanged, err)
	}
}

func TestChannelPatchAndLookup(t *testing.T) {
	st, clock := newStore(t)
	channel := st.mustChannel(t, 1, "Первый", true)
	clock.Advance(time.Minute)

	name := "Первый канал"
	inactive := false
	updated, err := st.UpdateChannel(t.Context(), channel.ID, ChannelPatch{Name: &name, Active: &inactive})
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if updated.Name != name || updated.Active {
		t.Errorf("got %+v, want the patched values", updated)
	}
	if updated.CreatedAt.Equal(updated.UpdatedAt) {
		t.Error("updated_at was not moved")
	}
	if _, err := st.ChannelByID(t.Context(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// accessFixture builds the smallest world that exercises every rule: unlimited
// and time limited grants, an expired grant, a missing grant, a switched off
// channel and an admin.
type accessFixture struct {
	store  *Store
	clock  *testutil.Clock
	grants User
	none   User
	admin  User
	on     Channel
	other  Channel
	off    Channel
	limit  Channel
}

func newAccessFixture(t *testing.T) *accessFixture {
	t.Helper()
	st, clock := newStore(t)
	ctx := t.Context()

	f := &accessFixture{store: st, clock: clock}
	f.grants = st.mustUser(t, "grants", true)
	f.none = st.mustUser(t, "none", true)
	f.admin = st.mustUser(t, "admin", true)
	f.on = st.mustChannel(t, 1, "one", true)
	f.other = st.mustChannel(t, 2, "two", true)
	f.off = st.mustChannel(t, 3, "three", false)
	f.limit = st.mustChannel(t, 4, "four", true)

	if _, err := st.GrantSubscription(ctx, f.grants.ID, f.on.ID, nil); err != nil {
		t.Fatalf("grant unlimited: %v", err)
	}
	expires := clock.Now().Add(time.Hour)
	if _, err := st.GrantSubscription(ctx, f.grants.ID, f.limit.ID, &expires); err != nil {
		t.Fatalf("grant limited: %v", err)
	}
	return f
}

func TestAccessDecisions(t *testing.T) {
	f := newAccessFixture(t)
	ctx := t.Context()

	tests := []struct {
		name      string
		user      int64
		admin     bool
		channel   int64
		wantErr   error
		wantExpir bool
	}{
		{"unlimited grant", f.grants.ID, false, f.on.ID, nil, false},
		{"time limited grant still valid", f.grants.ID, false, f.limit.ID, nil, true},
		{"channel without a grant", f.grants.ID, false, f.other.ID, ErrNoSubscription, false},
		{"switched off channel", f.grants.ID, false, f.off.ID, ErrDisabled, false},
		{"unknown channel", f.grants.ID, false, 424242, ErrNotFound, false},
		{"no grants at all", f.none.ID, false, f.on.ID, ErrNoSubscription, false},
		{"admin sees what a grant does not", f.admin.ID, true, f.other.ID, nil, false},
		{"admin still cannot watch a switched off channel", f.admin.ID, true, f.off.ID, ErrDisabled, false},
		{"non admin with the admin flag off", f.none.ID, false, f.off.ID, ErrDisabled, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			access, err := f.store.Access(ctx, tc.user, tc.channel, tc.admin)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if !access.Available {
				t.Error("Available = false for an allowed channel")
			}
			if got := access.ExpiresAt != nil; got != tc.wantExpir {
				t.Errorf("expiry present = %v, want %v", got, tc.wantExpir)
			}
			if access.Channel.ID != tc.channel {
				t.Errorf("channel id = %d, want %d", access.Channel.ID, tc.channel)
			}
		})
	}
}

// A time limited subscription stops granting access on its own: no cron, no
// admin action, nothing to forget to run.
func TestTimeLimitedSubscriptionExpiresWithoutAWrite(t *testing.T) {
	f := newAccessFixture(t)
	ctx := t.Context()

	if _, err := f.store.Access(ctx, f.grants.ID, f.limit.ID, false); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	f.clock.Advance(2 * time.Hour)

	_, err := f.store.Access(ctx, f.grants.ID, f.limit.ID, false)
	if !errors.Is(err, ErrNoSubscription) {
		t.Fatalf("err = %v, want ErrNoSubscription after the expiry moment", err)
	}
	// The unlimited grant is untouched.
	if _, err := f.store.Access(ctx, f.grants.ID, f.on.ID, false); err != nil {
		t.Errorf("unlimited grant broke: %v", err)
	}
}

func TestAccessForUserReportEveryActiveChannel(t *testing.T) {
	f := newAccessFixture(t)

	access, err := f.store.AccessForUser(t.Context(), f.grants.ID, false)
	if err != nil {
		t.Fatalf("AccessForUser: %v", err)
	}
	if len(access) != 3 {
		t.Fatalf("got %d channels, want 3: the switched off one must not appear", len(access))
	}
	want := map[int64]bool{f.on.ID: true, f.limit.ID: true, f.other.ID: false}
	for _, a := range access {
		available, ok := want[a.Channel.ID]
		if !ok {
			t.Fatalf("unexpected channel %d", a.Channel.ID)
		}
		if a.Available != available {
			t.Errorf("channel %d available = %v, want %v", a.Channel.ID, a.Available, available)
		}
	}

	adminAccess, err := f.store.AccessForUser(t.Context(), f.admin.ID, true)
	if err != nil {
		t.Fatalf("AccessForUser(admin): %v", err)
	}
	for _, a := range adminAccess {
		if !a.Available {
			t.Errorf("admin cannot see channel %d", a.Channel.ID)
		}
		if a.ExpiresAt != nil {
			t.Errorf("admin entry for channel %d carries a grant expiry", a.Channel.ID)
		}
	}
}

func TestNextExpiryPicksTheEarliestGrant(t *testing.T) {
	f := newAccessFixture(t)
	access, err := f.store.AccessForUser(t.Context(), f.grants.ID, false)
	if err != nil {
		t.Fatalf("AccessForUser: %v", err)
	}
	next := NextExpiry(access)
	if next.IsZero() {
		t.Fatal("NextExpiry returned zero although a limited grant exists")
	}
	if !next.Equal(f.clock.Now().Add(time.Hour)) {
		t.Errorf("NextExpiry = %v, want one hour from now", next)
	}
}

func TestGrantIsUpsertAndRevocable(t *testing.T) {
	f := newAccessFixture(t)
	ctx := t.Context()

	later := f.clock.Now().Add(24 * time.Hour)
	sub, err := f.store.GrantSubscription(ctx, f.grants.ID, f.on.ID, &later)
	if err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	if sub.ExpiresAt == nil || !sub.ExpiresAt.Equal(later) {
		t.Errorf("expiry = %v, want %v", sub.ExpiresAt, later)
	}
	subs, err := f.store.ListSubscriptions(ctx, f.grants.ID)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 2 {
		t.Errorf("got %d grants, want 2: the re-grant must not insert a second row", len(subs))
	}
	// The unlimited grant became a limited one and the access decision follows.
	access, err := f.store.Access(ctx, f.grants.ID, f.on.ID, false)
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access.ExpiresAt == nil {
		t.Error("the access decision lost the new expiry")
	}

	removed, err := f.store.RevokeSubscription(ctx, f.grants.ID, f.on.ID)
	if err != nil || !removed {
		t.Fatalf("Revoke = (%v, %v), want (true, nil)", removed, err)
	}
	if _, err := f.store.Access(ctx, f.grants.ID, f.on.ID, false); !errors.Is(err, ErrNoSubscription) {
		t.Errorf("err = %v, want ErrNotFound after revoke", err)
	}
	removed, err = f.store.RevokeSubscription(ctx, f.grants.ID, f.on.ID)
	if err != nil || removed {
		t.Errorf("second Revoke = (%v, %v), want (false, nil)", removed, err)
	}
}

func TestGrantRejectsUnknownReferences(t *testing.T) {
	f := newAccessFixture(t)
	ctx := t.Context()

	if _, err := f.store.GrantSubscription(ctx, 424242, f.on.ID, nil); !errors.Is(err, ErrInUse) {
		t.Errorf("unknown user: err = %v, want ErrInUse", err)
	}
	if _, err := f.store.GrantSubscription(ctx, f.grants.ID, 424242, nil); !errors.Is(err, ErrInUse) {
		t.Errorf("unknown channel: err = %v, want ErrInUse", err)
	}
}

func TestListSubscriptionsCarriesChannelInfo(t *testing.T) {
	f := newAccessFixture(t)
	subs, err := f.store.ListSubscriptions(t.Context(), f.grants.ID)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d grants, want 2", len(subs))
	}
	for _, sub := range subs {
		if sub.ChannelNum == 0 {
			t.Errorf("grant %d has no channel number", sub.ID)
		}
		if !sub.ChannelActive {
			t.Errorf("grant %d reports an inactive channel", sub.ID)
		}
	}
}

// Writes and version counters must move together, otherwise a cache guarded by
// the counter can serve data that no longer matches reality.
func TestWritesBumpTheVersionCounter(t *testing.T) {
	f := newAccessFixture(t)
	ctx := t.Context()

	usersBefore, err := f.store.Version(ctx, VersionUsers)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	catalogBefore, err := f.store.Version(ctx, VersionCatalog)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}

	if _, err := f.store.CreateChannel(ctx, NewChannel{Number: 90, Name: "new", StreamPath: "/new", Active: true}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := f.store.GrantSubscription(ctx, f.grants.ID, f.other.ID, nil); err != nil {
		t.Fatalf("GrantSubscription: %v", err)
	}
	active := false
	if _, err := f.store.UpdateUser(ctx, f.grants.ID, UserPatch{Active: &active}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	usersAfter, _ := f.store.Version(ctx, VersionUsers)
	catalogAfter, _ := f.store.Version(ctx, VersionCatalog)
	if usersAfter != usersBefore+1 {
		t.Errorf("users version %d -> %d, want one bump", usersBefore, usersAfter)
	}
	if catalogAfter != catalogBefore+2 {
		t.Errorf("catalog version %d -> %d, want two bumps", catalogBefore, catalogAfter)
	}
}

// Every query in this package is parameterised; this test proves a login that
// looks like SQL is only data.
func TestQuotedLoginIsDataNotSQL(t *testing.T) {
	st, _ := newStore(t)
	ctx := t.Context()
	nasty := `x' OR 1=1; DROP TABLE users;--`

	st.mustUser(t, "victim", true)
	if _, err := st.CreateUser(ctx, NewUser{Login: nasty, PasswordHash: "h", Active: true}); err != nil {
		t.Fatalf("CreateUser with a hostile login: %v", err)
	}
	if _, err := st.UserByLogin(ctx, `x' OR 1=1--`); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	// The table is still there and still readable.
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Errorf("ListUsers = (%d, %v), want 2 users", len(users), err)
	}
}

func TestClosedStoreReportsErrorsInsteadOfPanicking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.db")
	st, _ := openStore(t, path)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := st.ListUsers(t.Context()); err == nil {
		t.Error("a read on a closed store must fail, not return empty")
	}
	if err := st.Close(); err != nil {
		t.Errorf("the second Close should be harmless, got %v", err)
	}
}
