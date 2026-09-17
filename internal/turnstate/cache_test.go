package turnstate

import (
	"strings"
	"testing"
	"time"
)

func validState() string {
	return strings.Repeat("s", StateLength)
}

func validStateWith(value string) string {
	return strings.Repeat(value, StateLength)
}

func TestCacheAcceptsOnlyOneExactLengthState(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}

	if cache.StoreResponse(key, []string{validState()}) != true {
		t.Fatal("StoreResponse() = false, want true for a single 292-byte state")
	}
	if got, ok := cache.Lookup(key); !ok || got != validState() {
		t.Fatalf("Lookup() = (%q, %t), want valid cached state", got, ok)
	}

	for _, values := range [][]string{
		nil,
		{""},
		{strings.Repeat("x", StateLength-1)},
		{strings.Repeat("x", StateLength+1)},
		{validState(), validState()},
	} {
		if cache.StoreResponse(key, values) {
			t.Fatalf("StoreResponse(%d values) = true, want false", len(values))
		}
		if got, ok := cache.Lookup(key); !ok || got != validState() {
			t.Fatalf("invalid input replaced valid entry: Lookup() = (%q, %t)", got, ok)
		}
	}
}

func TestCacheScopesStateByAuthAndExactModel(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	state := validState()
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}

	if !cache.StoreResponse(key, []string{state}) {
		t.Fatal("StoreResponse() = false, want true")
	}

	for _, otherKey := range []CacheKey{
		{AuthID: "auth-b", Model: "gpt-5.3-codex"},
		{AuthID: "auth-a", Model: "gpt-5.3-codex-mini"},
		{AuthID: "auth-a", Model: "GPT-5.3-CODEX"},
	} {
		if got, ok := cache.Lookup(otherKey); ok || got != "" {
			t.Fatalf("Lookup(%+v) = (%q, %t), want cache miss", otherKey, got, ok)
		}
	}
}

func TestCacheExpiresExactlyOneHourWithoutSlidingRead(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}

	if !cache.StoreResponse(key, []string{validState()}) {
		t.Fatal("StoreResponse() = false, want true")
	}
	now = now.Add(59*time.Minute + 59*time.Second)
	if _, ok := cache.Lookup(key); !ok {
		t.Fatal("Lookup() before fixed expiry = miss, want hit")
	}
	now = now.Add(time.Second)
	if got, ok := cache.Lookup(key); ok || got != "" {
		t.Fatalf("Lookup() at fixed expiry = (%q, %t), want miss", got, ok)
	}
}

func TestCacheReplacesValidStateAndRestartsExpiryFromNewCapture(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	oldState := validStateWith("a")
	newState := validStateWith("b")

	if !cache.StoreResponse(key, []string{oldState}) {
		t.Fatal("StoreResponse(old state) = false, want true")
	}
	now = now.Add(59 * time.Minute)
	if !cache.StoreResponse(key, []string{newState}) {
		t.Fatal("StoreResponse(new state) = false, want true")
	}
	if got, ok := cache.Lookup(key); !ok || got != newState {
		t.Fatalf("Lookup() = (%q, %t), want replacement state", got, ok)
	}

	// This is after the old entry's fixed expiry but before the replacement expires.
	now = now.Add(2 * time.Minute)
	if got, ok := cache.Lookup(key); !ok || got != newState {
		t.Fatalf("Lookup() after old expiry = (%q, %t), want fresh replacement", got, ok)
	}
	now = now.Add(58 * time.Minute)
	if got, ok := cache.Lookup(key); ok || got != "" {
		t.Fatalf("Lookup() at replacement expiry = (%q, %t), want miss", got, ok)
	}
}

func TestCacheOverwritesPendingBindingForRetry(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })

	cache.BindRequest("request-1", CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"})
	cache.BindRequest("request-1", CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"})

	if got, ok := cache.Pending("request-1"); !ok || got != (CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"}) {
		t.Fatalf("Pending() = (%+v, %t), want latest retry binding", got, ok)
	}
	if !cache.StoreResponseForRequest("request-1", []string{validState()}) {
		t.Fatal("StoreResponseForRequest() = false, want valid final retry capture")
	}
	if got, ok := cache.Lookup(CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"}); !ok || got != validState() {
		t.Fatalf("Lookup(final retry key) = (%q, %t), want valid state", got, ok)
	}
}

func TestCacheRejectsResponseWithoutAfterAuthBinding(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })

	if cache.StoreResponseForRequest("unknown-request", []string{validState()}) {
		t.Fatal("StoreResponseForRequest() = true without an after-auth binding")
	}
}
