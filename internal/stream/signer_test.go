package stream

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testKey  = "0123456789abcdef0123456789abcdef"
	testPath = "/live/ch01/index.m3u8"
)

func newTestSigner(t *testing.T, base string, ttl time.Duration) *Signer {
	t.Helper()
	signer, err := New(base, []byte(testKey), ttl, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return signer
}

func TestPlaybackURLShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := newTestSigner(t, "http://stream.example.com", 15*time.Minute)

	link, expires, err := signer.Playback(testPath, now)
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}
	if want := now.Add(15 * time.Minute); !expires.Equal(want) {
		t.Errorf("expiry = %v, want %v", expires, want)
	}
	if !strings.HasPrefix(link, "http://stream.example.com/live/ch01/index.m3u8?") {
		t.Errorf("unexpected link %q", link)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link is not a valid URL: %v", err)
	}
	if got := parsed.Query().Get("exp"); got != strconv.FormatInt(expires.Unix(), 10) {
		t.Errorf("exp = %q, want %d", got, expires.Unix())
	}
	if len(parsed.Query().Get("sig")) != 64 {
		t.Errorf("signature %q is not a hex sha256", parsed.Query().Get("sig"))
	}
	if err := signer.Verify(link, testPath, now); err != nil {
		t.Errorf("a fresh link failed verification: %v", err)
	}
}

// A CDN path prefix in the configuration must be joined with the channel path
// without producing a double slash.
func TestBasePathIsJoinedCleanly(t *testing.T) {
	for _, base := range []string{"https://cdn.example.tv", "https://cdn.example.tv/", "https://cdn.example.tv/hls", "https://cdn.example.tv/hls/"} {
		signer := newTestSigner(t, base, time.Minute)
		link, _, err := signer.Playback(testPath, time.Now())
		if err != nil {
			t.Fatalf("Playback(%s): %v", base, err)
		}
		if strings.Contains(strings.TrimPrefix(link, "https://"), "//") {
			t.Errorf("link %q contains a doubled slash", link)
		}
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signer := newTestSigner(t, "http://stream.example.com", time.Minute)

	link, _, err := signer.Playback(testPath, now)
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}
	other, _, err := signer.Playback("/live/ch02/index.m3u8", now)
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}

	t.Run("expired", func(t *testing.T) {
		if err := signer.Verify(link, testPath, now.Add(2*time.Minute)); err == nil {
			t.Error("an expired link was accepted")
		}
	})
	t.Run("signature replayed on another channel", func(t *testing.T) {
		// Take the signature minted for channel 2 and use it for channel 1: the
		// path is inside the signed message, so this must fail.
		sig := url.QueryEscape(queryOf(t, other).Get("sig"))
		exp := queryOf(t, other).Get("exp")
		forge := "http://stream.example.com" + testPath + "?exp=" + exp + "&sig=" + sig
		if err := signer.Verify(forge, testPath, now); err == nil {
			t.Error("a signature from another channel was accepted")
		}
	})
	t.Run("expiry extended", func(t *testing.T) {
		parsed := queryOf(t, link)
		later, _ := time.ParseDuration("10m")
		forge := "http://stream.example.com" + testPath + "?exp=" +
			strconv.FormatInt(now.Add(later).Unix(), 10) + "&sig=" + parsed.Get("sig")
		if err := signer.Verify(forge, testPath, now); err == nil {
			t.Error("a link with a stretched expiry was accepted")
		}
	})
	t.Run("unsigned", func(t *testing.T) {
		if err := signer.Verify("http://stream.example.com"+testPath, testPath, now); err == nil {
			t.Error("an unsigned link was accepted")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		otherSigner, err := New("http://stream.example.com", []byte("a-completely-different-key"), time.Minute, true)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := otherSigner.Verify(link, testPath, now); err == nil {
			t.Error("a link signed with another key was accepted")
		}
	})
}

func TestUnsignedMode(t *testing.T) {
	signer, err := New("http://stream.example.com", nil, time.Minute, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	link, expires, err := signer.Playback(testPath, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("Playback: %v", err)
	}
	if strings.Contains(link, "?") {
		t.Errorf("unsigned link carries a query: %q", link)
	}
	if err := signer.Verify(link, testPath, expires.Add(time.Hour)); err != nil {
		t.Errorf("verification is a no-op when signing is off, got %v", err)
	}
}

func TestRejectsBadInput(t *testing.T) {
	if _, err := New("http://stream.example.com", nil, time.Minute, true); err == nil {
		t.Error("signing with an empty key must be refused rather than produce unverifiable links")
	}
	signer := newTestSigner(t, "http://stream.example.com", time.Minute)
	for _, path := range []string{"", "live/ch01/index.m3u8", "http://evil.example/x"} {
		if _, _, err := signer.Playback(path, time.Now()); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
}

func queryOf(t *testing.T, link string) url.Values {
	t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return parsed.Query()
}
