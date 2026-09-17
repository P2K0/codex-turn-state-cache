//go:build cgo

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tao/cpa-plugin-codex-turn-state/internal/turnstate"
)

func TestRegisterDeclaresRequiredHooks(t *testing.T) {
	var registered registration
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
	}, &registered)

	if registered.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registered.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registered.Metadata.Name != "codex-turn-state-cache" {
		t.Fatalf("plugin name = %q", registered.Metadata.Name)
	}
	if !registered.Capabilities.RequestInterceptor || !registered.Capabilities.RequestLifecyclePlugin || !registered.Capabilities.ResponseInterceptor || !registered.Capabilities.StreamChunkInterceptor {
		t.Fatalf("capabilities = %#v", registered.Capabilities)
	}
}

func TestRPCFlowCapturesAndReusesState(t *testing.T) {
	configureTestPlugin(t)
	state := strings.Repeat("s", turnstate.StateLength)

	var miss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &miss)
	if len(miss.Headers) != 0 || len(miss.ClearHeaders) != 0 {
		t.Fatalf("cache miss modified headers: %#v", miss)
	}

	var capture pluginapi.ResponseInterceptResponse
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: []string{state}},
	}, &capture)
	if len(capture.Headers) != 0 || len(capture.ClearHeaders) != 0 {
		t.Fatalf("response capture modified downstream response: %#v", capture)
	}

	var hit pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("reuse", "auth-a", "gpt-5"), &hit)
	if got := hit.Headers.Get(turnstate.TurnStateHeader); got != state {
		t.Fatalf("injected state = %q, want cached state", got)
	}
	if len(hit.ClearHeaders) != 0 {
		t.Fatalf("hit unexpectedly clears headers: %#v", hit.ClearHeaders)
	}
}

func TestRPCStreamHeaderInitCapturesAndCompletionStopsLateCapture(t *testing.T) {
	configureTestPlugin(t)
	streamState := strings.Repeat("t", turnstate.StateLength)

	var afterAuth pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("stream-capture", "auth-a", "gpt-stream"), &afterAuth)
	var streamCapture pluginapi.StreamChunkInterceptResponse
	callPluginMethod(t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID:       "stream-capture",
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: []string{streamState}},
	}, &streamCapture)
	if len(streamCapture.Headers) != 0 || len(streamCapture.ClearHeaders) != 0 {
		t.Fatalf("stream capture modified downstream stream: %#v", streamCapture)
	}

	var hit pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("stream-reuse", "auth-a", "gpt-stream"), &hit)
	if got := hit.Headers.Get(turnstate.TurnStateHeader); got != streamState {
		t.Fatalf("stream-captured state = %q, want cached state", got)
	}

	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("complete-before-response", "auth-a", "gpt-complete"), &afterAuth)
	callPluginMethod(t, pluginabi.MethodRequestComplete, pluginapi.RequestCompletion{RequestID: "complete-before-response"}, &struct{}{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "complete-before-response",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: []string{strings.Repeat("c", turnstate.StateLength)}},
	}, &pluginapi.ResponseInterceptResponse{})

	var lateCaptureMiss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("late-capture-miss", "auth-a", "gpt-complete"), &lateCaptureMiss)
	if len(lateCaptureMiss.Headers) != 0 {
		t.Fatalf("late response after completion populated cache: %#v", lateCaptureMiss.Headers)
	}
}

func TestQuiesceClearsCachedState(t *testing.T) {
	configureTestPlugin(t)
	state := strings.Repeat("q", turnstate.StateLength)

	var ignored pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &ignored)
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: []string{state}},
	}, &pluginapi.ResponseInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodPluginQuiesce, struct{}{}, &struct{}{})

	var miss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("after-quiesce", "auth-a", "gpt-5"), &miss)
	if len(miss.Headers) != 0 {
		t.Fatalf("quiesce did not clear cached state: %#v", miss.Headers)
	}
}

func configureTestPlugin(t *testing.T) {
	t.Helper()
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("max_entries: 8\nmax_pending_entries: 8\n"),
	}, &registration{})
}

func afterAuthRequest(requestID, authID, model string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		ToFormat:  "codex",
		Model:     model,
		Metadata:  map[string]any{"selected_auth_id": authID},
	}
}

func callPluginMethod(t *testing.T, method string, request, target any) {
	t.Helper()
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal %s request: %v", method, errMarshal)
	}
	rawResponse, errHandle := handleMethod(method, rawRequest)
	if errHandle != nil {
		t.Fatalf("handle %s: %v", method, errHandle)
	}
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &envelope); errUnmarshal != nil {
		t.Fatalf("decode %s envelope: %v", method, errUnmarshal)
	}
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("%s envelope = %s", method, rawResponse)
	}
	if errUnmarshal := json.Unmarshal(envelope.Result, target); errUnmarshal != nil {
		t.Fatalf("decode %s result: %v", method, errUnmarshal)
	}
}
