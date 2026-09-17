package turnstate

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func newTestPlugin(now func() time.Time) *Plugin {
	return NewPlugin(NewCache(10, 10, now), nil)
}

func afterAuth(requestID, authID, model string, headers http.Header) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		ToFormat:  "codex",
		Model:     model,
		Headers:   headers,
		Metadata:  map[string]any{selectedAuthIDMetadataKey: authID},
	}
}

func TestPluginReusesOnlyMatchingAuthAndModelAcrossIPs(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	cacheState := state("s")
	callerState := state("c")

	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("capture", "auth-a", "gpt-5.3-codex", http.Header{"X-Forwarded-For": {"198.51.100.10"}})); err != nil {
		t.Fatalf("bind capture: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{TurnStateHeader: {cacheState}},
	}); err != nil {
		t.Fatalf("capture response: %v", err)
	}

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("reuse", "auth-a", "gpt-5.3-codex", http.Header{
		TurnStateHeader:   {callerState},
		"X-Forwarded-For": {"203.0.113.45"},
	}))
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != cacheState {
		t.Fatalf("cross-IP state = %q", got)
	}

	for _, request := range []pluginapi.RequestInterceptRequest{
		afterAuth("other-auth", "auth-b", "gpt-5.3-codex", nil),
		afterAuth("other-model", "auth-a", "gpt-5.3-codex-mini", nil),
	} {
		response, err := plugin.InterceptRequestAfterAuth(context.Background(), request)
		if err != nil {
			t.Fatalf("isolation request: %v", err)
		}
		if got := response.Headers.Get(TurnStateHeader); got != "" {
			t.Fatalf("isolated request received state %q", got)
		}
	}
}

func TestPluginCapturesAgainstAfterAuthModel(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("capture", "auth-a", "gpt-5.3-codex", nil)); err != nil {
		t.Fatalf("bind capture: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		Model:           "normalized-client-alias",
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture response: %v", err)
	}

	selected, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("selected", "auth-a", "gpt-5.3-codex", nil))
	if err != nil {
		t.Fatalf("selected model: %v", err)
	}
	if got := selected.Headers.Get(TurnStateHeader); got != state("s") {
		t.Fatalf("selected model state = %q", got)
	}
	alias, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("alias", "auth-a", "normalized-client-alias", nil))
	if err != nil {
		t.Fatalf("alias model: %v", err)
	}
	if got := alias.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("response model unexpectedly received state %q", got)
	}
}

func TestPluginCapturesOnlyStreamHeaderInitAndDropsCompletedRequests(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("stream", "auth-a", "gpt-stream", nil)); err != nil {
		t.Fatalf("bind stream: %v", err)
	}
	if _, err := plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "stream",
		ChunkIndex:      0,
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture payload chunk: %v", err)
	}
	miss, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("payload-miss", "auth-a", "gpt-stream", nil))
	if err != nil || miss.Headers.Get(TurnStateHeader) != "" {
		t.Fatalf("payload chunk stored state: response=%#v error=%v", miss, err)
	}
	if _, err := plugin.InterceptStreamChunk(context.Background(), pluginapi.StreamChunkInterceptRequest{
		RequestID:       "stream",
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{TurnStateHeader: {state("s")}},
	}); err != nil {
		t.Fatalf("capture header init: %v", err)
	}
	hit, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("stream-hit", "auth-a", "gpt-stream", nil))
	if err != nil || hit.Headers.Get(TurnStateHeader) != state("s") {
		t.Fatalf("header init state: response=%#v error=%v", hit, err)
	}

	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("late", "auth-a", "gpt-late", nil)); err != nil {
		t.Fatalf("bind late response: %v", err)
	}
	if err := plugin.HandleRequestComplete(context.Background(), pluginapi.RequestCompletion{RequestID: "late"}); err != nil {
		t.Fatalf("complete request: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "late",
		ResponseHeaders: http.Header{TurnStateHeader: {state("l")}},
	}); err != nil {
		t.Fatalf("late response: %v", err)
	}
	late, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("late-hit", "auth-a", "gpt-late", nil))
	if err != nil || late.Headers.Get(TurnStateHeader) != "" {
		t.Fatalf("late response stored state: response=%#v error=%v", late, err)
	}
}
