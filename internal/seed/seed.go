// Package seed populates a fresh database with demo data so the service is
// useful immediately after clone, which the task asks for: at least two users
// and five channels.
//
// The accounts are development only, documented in the README, and the seeder
// refuses to run on a database that already contains users — it never
// overwrites real accounts or resets a changed password.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"iptv/internal/password"
	"iptv/internal/store"
)

// Demo writes the demo data set when the users table is empty and reports
// whether it inserted anything.
func Demo(ctx context.Context, st *store.Store, log *slog.Logger) (bool, error) {
	users, err := st.ListUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("seed: list users: %w", err)
	}
	if len(users) > 0 {
		return false, nil
	}

	channels := []store.NewChannel{
		{Number: 1, Name: "Первый", StreamPath: "/live/ch01/index.m3u8", Active: true},
		{Number: 2, Name: "Россия 1", StreamPath: "/live/ch02/index.m3u8", Active: true},
		{Number: 3, Name: "Матч ТВ", StreamPath: "/live/ch03/index.m3u8", Active: true},
		{Number: 4, Name: "НТВ", StreamPath: "/live/ch04/index.m3u8", Active: true},
		{Number: 5, Name: "ТНТ", StreamPath: "/live/ch05/index.m3u8", Active: true},
		// Switched off on purpose: it demonstrates the 404 case for a channel
		// that exists but does not broadcast.
		{Number: 6, Name: "Ретро ТВ", StreamPath: "/live/ch06/index.m3u8", Active: false},
	}
	byName := make(map[string]store.Channel, len(channels))
	for _, ch := range channels {
		created, err := st.CreateChannel(ctx, ch)
		if err != nil {
			return false, fmt.Errorf("seed: channel %q: %w", ch.Name, err)
		}
		byName[ch.Name] = created
	}

	now := time.Now()
	accounts := []struct {
		user store.NewUser
		subs []store.Subscription
	}{
		{
			user: store.NewUser{Login: "admin", PasswordHash: hash("admin12345"), Active: true, Admin: true},
			// The admin role sees every active channel, no grants needed.
		},
		{
			user: store.NewUser{Login: "viewer", PasswordHash: hash("viewer12345"), Active: true},
			subs: []store.Subscription{
				{ChannelID: byName["Первый"].ID},
				{ChannelID: byName["Россия 1"].ID},
				{ChannelID: byName["Матч ТВ"].ID},
				// Time limited: expires a week from now, so the demo shows a
				// channel that will stop working by itself.
				{ChannelID: byName["НТВ"].ID, ExpiresAt: ptr(now.Add(7 * 24 * time.Hour))},
				// ТНТ is deliberately not granted: requesting its link answers 403.
			},
		},
		{
			user: store.NewUser{Login: "viewer2", PasswordHash: hash("viewer12345"), Active: true},
			subs: []store.Subscription{
				{ChannelID: byName["Первый"].ID},
				// Already expired: access is refused without anybody touching it.
				{ChannelID: byName["Россия 1"].ID, ExpiresAt: ptr(now.Add(-24 * time.Hour))},
			},
		},
		{
			user: store.NewUser{Login: "blocked", PasswordHash: hash("blocked12345"), Active: false},
			subs: []store.Subscription{
				{ChannelID: byName["Первый"].ID},
			},
		},
	}

	for _, acc := range accounts {
		u, err := st.CreateUser(ctx, acc.user)
		if err != nil {
			return false, fmt.Errorf("seed: user %q: %w", acc.user.Login, err)
		}
		for _, sub := range acc.subs {
			if _, err := st.GrantSubscription(ctx, u.ID, sub.ChannelID, sub.ExpiresAt); err != nil {
				return false, fmt.Errorf("seed: subscription for %q: %w", acc.user.Login, err)
			}
		}
	}

	// Passwords go to the README, never to the log.
	log.Info("seeded demo data",
		"users", len(accounts), "channels", len(channels),
		"logins", []string{"admin", "viewer", "viewer2", "blocked"})
	return true, nil
}

func hash(pw string) string {
	// Demo data is created once at startup; a failure here means the KDF or the
	// platform RNG is broken and the service cannot work at all.
	h, err := password.Hash(pw)
	if err != nil {
		panic("seed: " + err.Error())
	}
	return h
}

func ptr(t time.Time) *time.Time { return &t }
