package turnstate

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func newTestPlugin(now func() time.Time) *Plugin {
	return NewPlugin(NewCache(10, 10, now), nil)
}

func newTestPluginWithMode(now func() time.Time, mode InjectMode) *Plugin {
	return NewPluginWithMode(NewCache(10, 10, now), nil, mode)
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

// captureTemplate binds a request and stores a template on its key, returning
// the stored value.
func captureTemplate(t *testing.T, plugin *Plugin, now func() time.Time, requestID, authID, model string) string {
	t.Helper()
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth(requestID, authID, model, nil)); err != nil {
		t.Fatalf("bind %s: %v", requestID, err)
	}
	value := templateAt(now(), "x")
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       requestID,
		ResponseHeaders: http.Header{TurnStateHeader: {value}},
	}); err != nil {
		t.Fatalf("capture response: %v", err)
	}
	return value
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
	if len(response.ClearHeaders) == 0 {
		t.Fatal("replacement did not clear the original header first")
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

func TestPluginReplaceOnlyModeSubstitutesDegradedStateAndLeavesOthersAlone(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPluginWithMode(func() time.Time { return now }, ModeReplaceOnly)
	template := captureTemplate(t, plugin, func() time.Time { return now }, "capture", "auth-a", "gpt-5.3-codex")

	// A degraded 312-length state on the request is swapped for the template.
	response, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth(
		"degraded", "auth-a", "gpt-5.3-codex",
		http.Header{TurnStateHeader: {degradedState("d")}},
	))
	if err != nil {
		t.Fatalf("degraded request: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != template {
		t.Fatalf("degraded state was not substituted: %q", got)
	}
	if plugin.Snapshot().Counters.Substituted != 1 {
		t.Fatalf("substituted counter = %d", plugin.Snapshot().Counters.Substituted)
	}

	// A headerless request stays untouched: forcing a template onto a request
	// that carried no state is the one behavior the mode exists to prevent.
	response, err = plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("bare", "auth-a", "gpt-5.3-codex", nil))
	if err != nil {
		t.Fatalf("bare request: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("headerless request was force-injected: %q", got)
	}

	// A request already carrying a normal 292-length state is also left alone.
	response, err = plugin.InterceptRequestAfterAuth(context.Background(), afterAuth(
		"normal", "auth-a", "gpt-5.3-codex",
		http.Header{TurnStateHeader: {state("n")}},
	))
	if err != nil {
		t.Fatalf("normal request: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != "" {
		t.Fatalf("normal state was replaced: %q", got)
	}
}

func TestPluginAlwaysModeInjectsWithoutIncomingHeader(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPluginWithMode(func() time.Time { return now }, ModeAlways)
	template := captureTemplate(t, plugin, func() time.Time { return now }, "capture", "auth-a", "gpt-5.3-codex")

	response, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("bare", "auth-a", "gpt-5.3-codex", nil))
	if err != nil {
		t.Fatalf("bare request: %v", err)
	}
	if got := response.Headers.Get(TurnStateHeader); got != template {
		t.Fatalf("always mode did not inject: %q", got)
	}
	if plugin.Snapshot().Counters.Injected != 1 {
		t.Fatalf("injected counter = %d", plugin.Snapshot().Counters.Injected)
	}
}

func TestPluginObservesDegradedResponsesWithoutStoringThem(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	if _, err := plugin.InterceptRequestAfterAuth(context.Background(), afterAuth("capture", "auth-a", "gpt-5.3-codex", nil)); err != nil {
		t.Fatalf("bind capture: %v", err)
	}
	if _, err := plugin.InterceptResponse(context.Background(), pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{TurnStateHeader: {degradedState("d")}},
	}); err != nil {
		t.Fatalf("capture degraded response: %v", err)
	}

	snapshot := plugin.Snapshot()
	if snapshot.Counters.DegradedObserved != 1 {
		t.Fatalf("degraded counter = %d", snapshot.Counters.DegradedObserved)
	}
	if len(snapshot.Buckets) != 0 {
		t.Fatalf("degraded state created a bucket: %+v", snapshot.Buckets)
	}
}

func TestPluginClearDropsEverything(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	plugin := newTestPlugin(func() time.Time { return now })
	captureTemplate(t, plugin, func() time.Time { return now }, "capture", "auth-a", "gpt-5.3-codex")

	plugin.Clear()
	snapshot := plugin.Snapshot()
	if len(snapshot.Buckets) != 0 || snapshot.Counters.Captured != 0 {
		t.Fatalf("after clear: %+v", snapshot)
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

func TestParseInjectMode(t *testing.T) {
	for input, expected := range map[string]InjectMode{
		"":              ModeAlways,
		"always":        ModeAlways,
		" Always ":      ModeAlways,
		"replace-only":  ModeReplaceOnly,
		"Replace-Only ": ModeReplaceOnly,
	} {
		if mode, ok := ParseInjectMode(input); !ok || mode != expected {
			t.Fatalf("ParseInjectMode(%q) = (%q, %t), want %q", input, mode, ok, expected)
		}
	}
	for _, invalid := range []string{"replace_only", "alwaysx", "replaceonly"} {
		if _, ok := ParseInjectMode(invalid); ok {
			t.Fatalf("ParseInjectMode(%q) accepted a typo -- it must fail loudly so a typo cannot silently degrade into the forcing mode", invalid)
		}
	}
}

func templateAt(now time.Time, seed string) string {
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Unix()))
	for index := 9; index < len(raw); index++ {
		raw[index] = byte(index) ^ seed[0]
	}
	return base64.URLEncoding.EncodeToString(raw)
}
