package turnstate

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func state(value string) string {
	return tokenWithTimestampAndMarker(time.Date(2026, time.September, 18, 11, 31, 0, 0, time.UTC), value[0])
}

func degradedState(value string) string {
	return strings.Repeat(value, DegradedLength/len(value))
}

// tokenWithTimestamp builds a Fernet-shaped value of exactly the template
// length: 0x80 version byte, 8-byte big-endian timestamp, arbitrary body.
// 217 bytes base64url-encode to 292 chars including padding, which is the
// observed shape of a real template.
func tokenWithTimestamp(issued time.Time) string {
	return tokenWithTimestampAndMarker(issued, 0)
}

func tokenWithTimestampAndMarker(issued time.Time, marker byte) string {
	raw := make([]byte, 217)
	raw[0] = 0x80
	secs := uint64(issued.Unix())
	for i := 0; i < 8; i++ {
		raw[1+i] = byte(secs >> (8 * (7 - i)))
	}
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i) ^ marker
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestCacheAcceptsOnlyOne292ByteState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	valid := tokenWithTimestamp(now.Add(-time.Minute))
	cache.BindRequest("request", key)

	if gotKey, outcome := cache.StoreResponseForRequest("request", []string{valid}); outcome != StoreTemplate || gotKey != key {
		t.Fatalf("first write = (%+v, %d)", gotKey, outcome)
	}
	for _, values := range [][]string{
		nil,
		{""},
		{strings.Repeat("x", StateLength-1)},
		{strings.Repeat("x", StateLength+1)},
		{valid, valid},
	} {
		if _, outcome := cache.StoreResponseForRequest("request", values); outcome != StoreIgnored {
			t.Fatalf("stored invalid header values: %#v", values)
		}
		if got, ok := cache.Lookup(key); !ok || got != valid {
			t.Fatalf("invalid values replaced cache: (%q, %t)", got, ok)
		}
	}
}

func TestCacheCountsDegradedStateButNeverStoresIt(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)

	if _, outcome := cache.StoreResponseForRequest("request", []string{degradedState("d")}); outcome != StoreDegraded {
		t.Fatalf("degraded write outcome = %d", outcome)
	}
	if _, ok := cache.Lookup(key); ok {
		t.Fatal("degraded state was stored")
	}
	if cache.Snapshot().Counters.DegradedObserved != 1 {
		t.Fatalf("degraded counter = %d", cache.Snapshot().Counters.DegradedObserved)
	}
}

func TestCacheExpiryKeyedOnFernetTimestamp(t *testing.T) {
	issued := time.Date(2026, time.September, 18, 11, 30, 0, 0, time.UTC)
	// Captured 30 minutes into the token's own 1-hour life.
	now := issued.Add(30 * time.Minute)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	value := tokenWithTimestamp(issued)
	cache.BindRequest("request", key)

	if _, outcome := cache.StoreResponseForRequest("request", []string{value}); outcome != StoreTemplate {
		t.Fatalf("store outcome = %d", outcome)
	}
	// 31 minutes after issuance: still inside the token's own window.
	now = now.Add(1 * time.Minute)
	if got, ok := cache.Lookup(key); !ok || got != value {
		t.Fatalf("inside token window = (%q, %t)", got, ok)
	}
	// 61 minutes after issuance: expired even though only 31 minutes passed
	// since capture. A capture-time clock would wrongly keep it alive.
	now = now.Add(30 * time.Minute)
	if got, ok := cache.Lookup(key); ok || got != "" {
		t.Fatalf("past token expiry = (%q, %t)", got, ok)
	}
}

func TestCacheRejectsFutureFernetTimestamp(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)

	future := tokenWithTimestamp(now.Add(10 * time.Minute))
	if _, outcome := cache.StoreResponseForRequest("request", []string{future}); outcome != StoreIgnored {
		t.Fatalf("future timestamp outcome = %d", outcome)
	}
	if _, ok := cache.Lookup(key); ok {
		t.Fatal("future-stamped token was stored")
	}
}

func TestCacheRejectsMalformedFernetState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}

	badVersionRaw := make([]byte, 217)
	badVersionRaw[0] = 0x81
	badTimestampRaw := make([]byte, 217)
	badTimestampRaw[0] = 0x80
	wrongDecodedLength := make([]byte, 219)
	wrongDecodedLength[0] = 0x80
	secs := uint64(now.Unix())
	for i := 0; i < 8; i++ {
		wrongDecodedLength[1+i] = byte(secs >> (8 * (7 - i)))
	}

	tests := map[string]string{
		"not base64":            strings.Repeat("!", StateLength),
		"standard base64":       base64.StdEncoding.EncodeToString(make([]byte, 217)),
		"wrong version":         base64.URLEncoding.EncodeToString(badVersionRaw),
		"implausible timestamp": base64.URLEncoding.EncodeToString(badTimestampRaw),
		"wrong decoded length":  base64.RawURLEncoding.EncodeToString(wrongDecodedLength),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			cache := NewCache(10, 10, func() time.Time { return now })
			if outcome := cache.StoreForBucket(key, value); outcome != StoreIgnored {
				t.Fatalf("store outcome = %d, want %d", outcome, StoreIgnored)
			}
			if _, ok := cache.Lookup(key); ok {
				t.Fatal("malformed state was stored")
			}
		})
	}
}

func TestCacheRejectsExpiredFernetState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}

	for name, issuedAt := range map[string]time.Time{
		"expired":               now.Add(-stateTTL - time.Second),
		"less than margin left": now.Add(-stateTTL + 29*time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			cache := NewCache(10, 10, func() time.Time { return now })
			if outcome := cache.StoreForBucket(key, tokenWithTimestamp(issuedAt)); outcome != StoreStale {
				t.Fatalf("store outcome = %d, want %d", outcome, StoreStale)
			}
			if _, ok := cache.Lookup(key); ok {
				t.Fatal("stale state was stored")
			}
		})
	}

	cache := NewCache(10, 10, func() time.Time { return now })
	if outcome := cache.StoreForBucket(key, tokenWithTimestamp(now.Add(-stateTTL+30*time.Second))); outcome != StoreTemplate {
		t.Fatalf("exact margin outcome = %d, want %d", outcome, StoreTemplate)
	}
}

func TestCacheDoesNotRegressToOlderFernetState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	newer := tokenWithTimestampAndMarker(now.Add(-time.Minute), 1)
	older := tokenWithTimestampAndMarker(now.Add(-2*time.Minute), 2)

	if outcome := cache.StoreForBucket(key, newer); outcome != StoreTemplate {
		t.Fatalf("newer outcome = %d, want %d", outcome, StoreTemplate)
	}
	if outcome := cache.StoreForBucket(key, older); outcome != StoreStale {
		t.Fatalf("older outcome = %d, want %d", outcome, StoreStale)
	}
	if got, ok := cache.Lookup(key); !ok || got != newer {
		t.Fatalf("cached state = (%q, %t), want newer state", got, ok)
	}

	if outcome := cache.StoreForBucket(key, newer); outcome != StoreIgnored {
		t.Fatalf("duplicate outcome = %d, want %d", outcome, StoreIgnored)
	}
	equalButDifferent := tokenWithTimestampAndMarker(now.Add(-time.Minute), 3)
	if outcome := cache.StoreForBucket(key, equalButDifferent); outcome != StoreReplaced {
		t.Fatalf("equal-time replacement outcome = %d, want %d", outcome, StoreReplaced)
	}
	if got, ok := cache.Lookup(key); !ok || got != equalButDifferent {
		t.Fatalf("cached state = (%q, %t), want equal-time replacement", got, ok)
	}
}

func TestCacheReplacementHasFixedOneHourLifetime(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)
	first := tokenWithTimestampAndMarker(now, 'a')
	if _, outcome := cache.StoreResponseForRequest("request", []string{first}); outcome != StoreTemplate {
		t.Fatalf("first write outcome=%d", outcome)
	}

	now = now.Add(59 * time.Minute)
	replacement := tokenWithTimestampAndMarker(now.Add(-time.Minute), 'b')
	if _, outcome := cache.StoreResponseForRequest("request", []string{replacement}); outcome != StoreReplaced {
		t.Fatalf("replacement outcome=%d", outcome)
	}
	now = now.Add(2 * time.Minute)
	if got, ok := cache.Lookup(key); !ok || got != replacement {
		t.Fatalf("replacement after old expiry = (%q, %t)", got, ok)
	}
	now = now.Add(58*time.Minute + time.Second)
	if got, ok := cache.Lookup(key); ok || got != "" {
		t.Fatalf("replacement at fixed expiry = (%q, %t)", got, ok)
	}
}

func TestCacheUsesLatestBindingAndRejectsUnboundResponses(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	first := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	last := CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"}
	cache.BindRequest("retry", first)
	cache.BindRequest("retry", last)

	if gotKey, outcome := cache.StoreResponseForRequest("retry", []string{state("s")}); outcome != StoreTemplate || gotKey != last {
		t.Fatalf("retry write = (%+v, %d)", gotKey, outcome)
	}
	if _, ok := cache.Lookup(first); ok {
		t.Fatal("response was stored for the superseded binding")
	}
	if got, ok := cache.Lookup(last); !ok || got != state("s") {
		t.Fatalf("latest binding state = (%q, %t)", got, ok)
	}
	if _, outcome := cache.StoreResponseForRequest("unknown", []string{state("u")}); outcome != StoreIgnored {
		t.Fatal("unbound response was stored")
	}

	late := CacheKey{AuthID: "auth-a", Model: "gpt-late"}
	cache.BindRequest("late", late)
	cache.ForgetRequest("late")
	if _, outcome := cache.StoreResponseForRequest("late", []string{state("l")}); outcome != StoreIgnored {
		t.Fatal("response after completion was stored")
	}
}

func TestCacheSnapshotAndClearCarryNoValues(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)
	value := state("s")
	cache.StoreResponseForRequest("request", []string{value})
	cache.CountInjected()

	snapshot := cache.Snapshot()
	if snapshot.Counters.Captured != 1 || snapshot.Counters.Injected != 1 {
		t.Fatalf("counters = %+v", snapshot.Counters)
	}
	if len(snapshot.Buckets) != 1 {
		t.Fatalf("buckets = %+v", snapshot.Buckets)
	}
	bucket := snapshot.Buckets[0]
	if bucket.AuthID != key.AuthID || bucket.Model != key.Model || !bucket.Ready {
		t.Fatalf("bucket = %+v", bucket)
	}
	// The management status route is authenticated, and the dashboard needs
	// the value for its copy button -- so the snapshot deliberately carries it.
	if bucket.Len != len(value) {
		t.Fatalf("bucket length = %d", bucket.Len)
	}
	if revealed, ok := cache.Reveal(key); !ok || revealed != value {
		t.Fatalf("revealed value = (%q, %t)", revealed, ok)
	}

	cache.Clear()
	if snapshot := cache.Snapshot(); len(snapshot.Buckets) != 0 || snapshot.Counters.Captured != 0 {
		t.Fatalf("after clear: %+v", snapshot)
	}
}
