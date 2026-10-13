// Package stream builds playback links for channels.
//
// There is no real streaming server behind this service, so the interesting
// part of the job is making the link correct and hard to abuse:
//
//   - the link is assembled from a configured origin plus a path stored per
//     channel, so moving the CDN is a configuration change rather than a data
//     migration;
//   - the link is HMAC signed with an expiry, so a leaked playlist URL stops
//     working on its own and the edge server can verify it statelessly (one
//     HMAC, no database lookup on the hot path).
//
// Verification is not part of the request path here — the edge would check the
// signature — but Verify is exported so the format is covered by tests and can
// be reused by whoever runs the stream server.
package stream

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Signer builds playback URLs.
type Signer struct {
	base    *url.URL
	key     []byte
	signing bool
	ttl     time.Duration
}

// New returns a signer. An empty key with signing enabled is rejected rather
// than silently producing URLs nobody can verify; main() generates a key in
// that case and warns that it is per-process.
func New(baseURL string, key []byte, ttl time.Duration, signing bool) (*Signer, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("stream: base url: %w", err)
	}
	if signing && len(key) == 0 {
		return nil, fmt.Errorf("stream: signing enabled but key is empty")
	}
	return &Signer{base: u, key: key, signing: signing, ttl: ttl}, nil
}

// TTL is how long a generated link stays valid.
func (s *Signer) TTL() time.Duration { return s.ttl }

// Playback returns the URL a client should open for path, valid until expires.
// path is the channel's stream path and must start with "/".
func (s *Signer) Playback(path string, now time.Time) (link string, expires time.Time, err error) {
	if path == "" || path[0] != '/' {
		return "", time.Time{}, fmt.Errorf("stream: path %q must start with /", path)
	}
	u := *s.base
	u.Path = joinPath(s.base.Path, path)
	expires = now.Add(s.ttl)
	if s.signing {
		q := u.Query()
		q.Set("exp", strconv.FormatInt(expires.Unix(), 10))
		q.Set("sig", s.signature(path, expires.Unix()))
		u.RawQuery = q.Encode()
	}
	return u.String(), expires, nil
}

// Signature produces the token for a path and expiry.
//
// The signed message covers the path and the expiry only — never the query
// string as a whole — so the token cannot be replayed against a different
// channel and the encoding of other parameters cannot change the verdict.
func (s *Signer) signature(path string, exp int64) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(path))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether link is a correctly signed URL for path that has not
// expired at now. This is what an edge server would do before serving the
// playlist.
func (s *Signer) Verify(link, path string, now time.Time) error {
	if !s.signing {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Errorf("stream: verify: %w", err)
	}
	expRaw := u.Query().Get("exp")
	sig := u.Query().Get("sig")
	if expRaw == "" || sig == "" {
		return fmt.Errorf("stream: link is not signed")
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return fmt.Errorf("stream: bad expiry")
	}
	if !now.Before(time.Unix(exp, 0)) {
		return fmt.Errorf("stream: link expired")
	}
	want := s.signature(path, exp)
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return fmt.Errorf("stream: bad signature")
	}
	return nil
}

// joinPath appends a channel path to a base path without producing "//".
func joinPath(base, path string) string {
	switch {
	case base == "" || base == "/":
		return path
	case base[len(base)-1] == '/':
		return base + path[1:]
	default:
		return base + path
	}
}
