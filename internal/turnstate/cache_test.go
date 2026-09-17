package turnstate

import (
	"strings"
	"testing"
	"time"
)

func state(value string) string {
	return strings.Repeat(value, StateLength/len(value))
}

func TestCacheAcceptsOnlyOne292ByteState(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	valid := strings.Repeat("\xC3\xA9", StateLength/2)
	cache.BindRequest("request", key)

	if gotKey, stored, replaced := cache.StoreResponseForRequest("request", []string{valid}); !stored || replaced || gotKey != key {
		t.Fatalf("first write = (%+v, %t, %t)", gotKey, stored, replaced)
	}
	for _, values := range [][]string{
		nil,
		{""},
		{strings.Repeat("x", StateLength-1)},
		{strings.Repeat("x", StateLength+1)},
		{valid, valid},
	} {
		if _, stored, _ := cache.StoreResponseForRequest("request", values); stored {
			t.Fatalf("stored invalid header values: %#v", values)
		}
		if got, ok := cache.Lookup(key); !ok || got != valid {
			t.Fatalf("invalid values replaced cache: (%q, %t)", got, ok)
		}
	}
}

func TestCacheReplacementHasFixedOneHourLifetime(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	cache := NewCache(10, 10, func() time.Time { return now })
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	cache.BindRequest("request", key)
	if _, stored, replaced := cache.StoreResponseForRequest("request", []string{state("a")}); !stored || replaced {
		t.Fatalf("first write stored=%t replaced=%t", stored, replaced)
	}

	now = now.Add(59 * time.Minute)
	if _, stored, replaced := cache.StoreResponseForRequest("request", []string{state("b")}); !stored || !replaced {
		t.Fatalf("replacement stored=%t replaced=%t", stored, replaced)
	}
	now = now.Add(2 * time.Minute)
	if got, ok := cache.Lookup(key); !ok || got != state("b") {
		t.Fatalf("replacement after old expiry = (%q, %t)", got, ok)
	}
	now = now.Add(58 * time.Minute)
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

	if gotKey, stored, _ := cache.StoreResponseForRequest("retry", []string{state("s")}); !stored || gotKey != last {
		t.Fatalf("retry write = (%+v, %t)", gotKey, stored)
	}
	if _, ok := cache.Lookup(first); ok {
		t.Fatal("response was stored for the superseded binding")
	}
	if got, ok := cache.Lookup(last); !ok || got != state("s") {
		t.Fatalf("latest binding state = (%q, %t)", got, ok)
	}
	if _, stored, _ := cache.StoreResponseForRequest("unknown", []string{state("u")}); stored {
		t.Fatal("unbound response was stored")
	}

	late := CacheKey{AuthID: "auth-a", Model: "gpt-late"}
	cache.BindRequest("late", late)
	cache.ForgetRequest("late")
	if _, stored, _ := cache.StoreResponseForRequest("late", []string{state("l")}); stored {
		t.Fatal("response after completion was stored")
	}
}
