package turnstate

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAfterAuthReplacesCallerStateWithOnlyMatchingCachedState(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	if !plugin.cache.StoreResponse(key, []string{validState()}) {
		t.Fatal("StoreResponse() = false, want true")
	}

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestID: "request-1",
		ToFormat:  "codex",
		Model:     key.Model,
		Headers: http.Header{
			"X-Codex-Turn-State": {strings.Repeat("caller", 60)},
			"X-Other":            {"preserve"},
		},
		Metadata: map[string]any{"selected_auth_id": key.AuthID},
	})
	if err != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
	}
	if got := response.Headers.Values(TurnStateHeader); len(got) != 1 || got[0] != validState() {
		t.Fatalf("injected state = %#v, want cached state", got)
	}
	if got := response.Headers.Get("X-Other"); got != "" {
		t.Fatalf("response modified unrelated header = %q, want no value", got)
	}
}

func TestAfterAuthMissDoesNotInjectAndNeverUsesIP(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestID: "request-1",
		ToFormat:  "codex",
		Model:     "gpt-5.3-codex",
		Headers: http.Header{
			"x-codex-turn-state": {validState()},
			"X-Forwarded-For":    {"198.51.100.1"},
		},
		Metadata: map[string]any{"selected_auth_id": "auth-a"},
	})
	if err != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
	}
	if got := response.Headers.Values(TurnStateHeader); len(got) != 0 {
		t.Fatalf("miss injected caller state = %#v, want no state", got)
	}
	if _, ok := plugin.cache.Pending("request-1"); !ok {
		t.Fatal("Pending() = miss, want binding independent of request IP headers")
	}
}

func TestAfterAuthReusesSameKeyAcrossClientIPs(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	key := CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}
	state := validState()
	if !plugin.cache.StoreResponse(key, []string{state}) {
		t.Fatal("StoreResponse() = false, want true")
	}

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestID: "request-2",
		ToFormat:  "codex",
		Model:     key.Model,
		Headers: http.Header{
			"X-Forwarded-For": {"203.0.113.45"},
		},
		Metadata: map[string]any{"selected_auth_id": key.AuthID},
	})
	if err != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != state {
		t.Fatalf("cross-IP state = %q, want cached state", got)
	}
}

func TestAfterAuthDoesNotReplaceForDifferentAuthOrModel(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	state := validState()
	if !plugin.cache.StoreResponse(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}, []string{state}) {
		t.Fatal("StoreResponse() = false, want true")
	}

	for _, tc := range []struct {
		name string
		key  CacheKey
	}{
		{name: "different auth", key: CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex"}},
		{name: "different model", key: CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex-mini"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
				RequestID: "request-" + tc.name,
				ToFormat:  "codex",
				Model:     tc.key.Model,
				Metadata:  map[string]any{"selected_auth_id": tc.key.AuthID},
			})
			if err != nil {
				t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
			}
			if got := response.Headers.Get(TurnStateHeader); got != "" {
				t.Fatalf("state = %q, want no replacement", got)
			}
		})
	}
}

func TestCaptureResponseUsesPendingAfterAuthModelNotNormalizedResponseModel(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	state := validState()
	_, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestID: "request-1",
		ToFormat:  "codex",
		Model:     "gpt-5.3-codex-mini",
		Metadata:  map[string]any{"selected_auth_id": "auth-a"},
	})
	if err != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
	}

	_, err = plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID: "request-1",
		Model:     "client-alias", // CPA response hooks can receive a normalized outer model.
		ResponseHeaders: http.Header{
			"X-Codex-Turn-State": {state},
		},
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if got, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex-mini"}); !ok || got != state {
		t.Fatalf("Lookup(after-auth key) = (%q, %t), want captured state", got, ok)
	}
	if got, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "client-alias"}); ok || got != "" {
		t.Fatalf("Lookup(response model) = (%q, %t), want no state", got, ok)
	}
}

func TestRetryCaptureUsesLatestAfterAuthBinding(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	state := validState()

	for _, key := range []CacheKey{
		{AuthID: "auth-a", Model: "gpt-5.3-codex"},
		{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"},
	} {
		_, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
			RequestID: "request-retry",
			ToFormat:  "codex",
			Model:     key.Model,
			Metadata:  map[string]any{"selected_auth_id": key.AuthID},
		})
		if err != nil {
			t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
		}
	}

	_, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "request-retry",
		ResponseHeaders: http.Header{TurnStateHeader: {state}},
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if _, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}); ok {
		t.Fatal("first retry binding received the final response state")
	}
	if got, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-b", Model: "gpt-5.3-codex-mini"}); !ok || got != state {
		t.Fatalf("latest retry binding = (%q, %t), want final state", got, ok)
	}
}

func TestStreamCaptureOnlyRunsAtHeaderInitialization(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))
	state := validState()
	_, err := plugin.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{
		RequestID: "request-1",
		ToFormat:  "codex",
		Model:     "gpt-5.3-codex",
		Metadata:  map[string]any{"selected_auth_id": "auth-a"},
	})
	if err != nil {
		t.Fatalf("InterceptRequestAfterAuth() error = %v", err)
	}

	_, err = plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "request-1",
		ChunkIndex:      0,
		ResponseHeaders: http.Header{"X-Codex-Turn-State": {state}},
	})
	if err != nil {
		t.Fatalf("InterceptStreamChunk(payload) error = %v", err)
	}
	if _, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}); ok {
		t.Fatal("payload chunk stored state, want header-init only")
	}

	_, err = plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "request-1",
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{"X-Codex-Turn-State": {state}},
	})
	if err != nil {
		t.Fatalf("InterceptStreamChunk(header-init) error = %v", err)
	}
	if got, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}); !ok || got != state {
		t.Fatalf("Lookup() = (%q, %t), want captured state", got, ok)
	}
}

func TestCaptureResponseRequiresEarlierAfterAuthBinding(t *testing.T) {
	now := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
	plugin := NewPlugin(NewCache(10, 10, func() time.Time { return now }))

	_, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "unknown-request",
		ResponseHeaders: http.Header{TurnStateHeader: {validState()}},
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if _, ok := plugin.cache.Lookup(CacheKey{AuthID: "auth-a", Model: "gpt-5.3-codex"}); ok {
		t.Fatal("response without after-auth context stored a state")
	}
}
