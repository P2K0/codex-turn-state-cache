package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static void clear_host_api(void) {
	stored_host = NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	defaultMaxEntries        = 10_000
	defaultMaxPendingEntries = 20_000
	pluginID                 = "codex-turn-state-cache"
	pluginVersion            = "0.1.2"
)

var runtime = pluginRuntime{
	plugin: newPlugin(pluginConfig{
		MaxEntries:        defaultMaxEntries,
		MaxPendingEntries: defaultMaxPendingEntries,
	}),
}

type pluginRuntime struct {
	mu     sync.RWMutex
	plugin *turnstate.Plugin
}

type pluginConfig struct {
	MaxEntries        int `yaml:"max_entries"`
	MaxPendingEntries int `yaml:"max_pending_entries"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type requestInterceptRPC struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type responseInterceptRPC struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type streamChunkInterceptRPC struct {
	pluginapi.StreamChunkInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostLogRequest struct {
	HostCallbackID string        `json:"host_callback_id,omitempty"`
	Level          string        `json:"level,omitempty"`
	Message        string        `json:"message,omitempty"`
	Fields         hostLogFields `json:"fields"`
}

type hostLogFields struct {
	PluginID string `json:"plugin_id"`
	Model    string `json:"model"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}

	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	resetRuntime()
	C.clear_host_api()
}

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(raw); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		resetRuntime()
		return okEnvelope(struct{}{})
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequestBeforeAuth(raw)
	case pluginabi.MethodRequestInterceptAfter:
		return interceptRequestAfterAuth(raw)
	case pluginabi.MethodResponseInterceptAfter:
		return interceptResponse(raw)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return interceptStreamChunk(raw)
	case pluginabi.MethodRequestComplete:
		return completeRequest(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var request lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return errUnmarshal
	}
	if request.SchemaVersion < 2 {
		return fmt.Errorf("request lifecycle plugin requires host schema version 2 or newer")
	}

	cfg := pluginConfig{
		MaxEntries:        defaultMaxEntries,
		MaxPendingEntries: defaultMaxPendingEntries,
	}
	if len(request.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(request.ConfigYAML, &cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if cfg.MaxEntries < 1 || cfg.MaxPendingEntries < 1 {
		return fmt.Errorf("max_entries and max_pending_entries must be greater than zero")
	}

	runtime.mu.Lock()
	runtime.plugin = newPlugin(cfg)
	runtime.mu.Unlock()
	return nil
}

func newPlugin(cfg pluginConfig) *turnstate.Plugin {
	return turnstate.NewPlugin(
		turnstate.NewCache(cfg.MaxEntries, cfg.MaxPendingEntries, nil),
		logTurnState,
	)
}

func resetRuntime() {
	runtime.mu.Lock()
	runtime.plugin = newPlugin(pluginConfig{
		MaxEntries:        defaultMaxEntries,
		MaxPendingEntries: defaultMaxPendingEntries,
	})
	runtime.mu.Unlock()
}

func currentPlugin() *turnstate.Plugin {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.plugin
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "5345asda",
			GitHubRepository: "https://github.com/5345asda/codex-turn-state-cache",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "max_entries",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Maximum in-memory account/model state entries.",
				},
				{
					Name:        "max_pending_entries",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Maximum in-flight request correlations.",
				},
			},
		},
		Capabilities: registrationCapabilities{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

func interceptRequestBeforeAuth(raw []byte) ([]byte, error) {
	var request pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	response, errIntercept := currentPlugin().InterceptRequestBeforeAuth(context.Background(), request)
	if errIntercept != nil {
		return nil, errIntercept
	}
	return okEnvelope(response)
}

func interceptRequestAfterAuth(raw []byte) ([]byte, error) {
	var request requestInterceptRPC
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	response, errIntercept := currentPlugin().InterceptRequestAfterAuth(contextWithHostCallbackID(request.HostCallbackID), request.RequestInterceptRequest)
	if errIntercept != nil {
		return nil, errIntercept
	}
	return okEnvelope(response)
}

func interceptResponse(raw []byte) ([]byte, error) {
	var request responseInterceptRPC
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	response, errIntercept := currentPlugin().InterceptResponse(contextWithHostCallbackID(request.HostCallbackID), request.ResponseInterceptRequest)
	if errIntercept != nil {
		return nil, errIntercept
	}
	return okEnvelope(response)
}

func interceptStreamChunk(raw []byte) ([]byte, error) {
	var request streamChunkInterceptRPC
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	response, errIntercept := currentPlugin().InterceptStreamChunk(contextWithHostCallbackID(request.HostCallbackID), request.StreamChunkInterceptRequest)
	if errIntercept != nil {
		return nil, errIntercept
	}
	return okEnvelope(response)
}

func completeRequest(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if errComplete := currentPlugin().HandleRequestComplete(context.Background(), completion); errComplete != nil {
		return nil, errComplete
	}
	return okEnvelope(struct{}{})
}

func okEnvelope(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: pluginabi.NewError(code, message),
	})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

type hostCallbackIDKey struct{}

func contextWithHostCallbackID(callbackID string) context.Context {
	return context.WithValue(context.Background(), hostCallbackIDKey{}, callbackID)
}

func logTurnState(ctx context.Context, message, model string) {
	payload, _ := json.Marshal(hostLogRequest{
		HostCallbackID: hostCallbackID(ctx),
		Level:          "info",
		Message:        message,
		Fields: hostLogFields{
			PluginID: pluginID,
			Model:    model,
		},
	})
	callHostLog(payload)
}

func hostCallbackID(ctx context.Context) string {
	callbackID, _ := ctx.Value(hostCallbackIDKey{}).(string)
	return callbackID
}

func callHostLog(payload []byte) {
	cMethod := C.CString(pluginabi.MethodHostLog)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		request = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(request))
	}
	var response C.cliproxy_buffer
	if C.call_host_api(cMethod, request, C.size_t(len(payload)), &response) == 0 && response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}
