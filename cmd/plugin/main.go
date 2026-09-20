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
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	defaultMaxEntries                    = 10_000
	defaultMaxPendingEntries             = 20_000
	defaultCollectorRefreshBeforeSeconds = 300
	defaultCollectorRetrySeconds         = 60
	pluginID                             = "cpa-plugin-codex-turn-state"
)

// pluginVersion is overridden by the release build script so the artifact
// name and the version reported to CPA cannot drift apart.
var pluginVersion = "0.3.0"

var runtime = pluginRuntime{
	plugin: newPlugin(pluginConfig{
		MaxEntries:        defaultMaxEntries,
		MaxPendingEntries: defaultMaxPendingEntries,
	}),
	config: pluginConfig{
		MaxEntries:                    defaultMaxEntries,
		MaxPendingEntries:             defaultMaxPendingEntries,
		CollectorRefreshBeforeSeconds: defaultCollectorRefreshBeforeSeconds,
		CollectorRetrySeconds:         defaultCollectorRetrySeconds,
	},
	collectorEvents: newCollectorEventStore(defaultCollectorEventLogPath),
}

type pluginRuntime struct {
	mu              sync.RWMutex
	lifecycleMu     sync.Mutex
	plugin          *turnstate.Plugin
	collector       *autoCollector
	config          pluginConfig
	collectorEvents *collectorEventStore
}

type pluginConfig struct {
	// enabled and priority are injected by CPA into every plugin config before
	// registration. They are host-owned and intentionally ignored here, but
	// must be declared so the strict decoder does not reject a valid runtime
	// config.
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`
	// store is metadata written by CPA's plugin store when this plugin is
	// installed from a registry. It is not consumed by the plugin, but must be
	// accepted as an opaque map for the same reason as the host-owned fields.
	Store                         map[string]any `yaml:"store"`
	MaxEntries                    int            `yaml:"max_entries"`
	MaxPendingEntries             int            `yaml:"max_pending_entries"`
	InjectMode                    string         `yaml:"inject_mode"`
	CollectorProxyURL             string         `yaml:"collector_proxy_url"`
	CollectorRefreshBeforeSeconds int            `yaml:"collector_refresh_before_seconds"`
	CollectorRetrySeconds         int            `yaml:"collector_retry_seconds"`
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
	AuthID   string `json:"auth_id,omitempty"`
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
	ManagementAPI          bool `json:"management_api"`
}

func main() {}

// hostAPI is the *C.cliproxy_host_api the host passes to cliproxy_plugin_init,
// kept so the management handlers can call back into the host (model execute
// for the probe, auth list/get for subscription tiers). It is written once
// during init and read from handler goroutines, hence the atomic.
var hostAPI unsafe.Pointer

func hostAPIAvailable() bool {
	return atomic.LoadPointer(&hostAPI) != nil
}

// hostCall invokes a host callback and returns its raw RPC envelope. The host
// owns the response buffer, so every non-nil ptr it hands back must be
// released through the host.
func hostCall(method string, request []byte) ([]byte, error) {
	raw := atomic.LoadPointer(&hostAPI)
	if raw == nil {
		return nil, fmt.Errorf("host API unavailable: this plugin was initialised without one")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}

	var response C.cliproxy_buffer
	rc := C.call_host_api(cMethod, requestPtr, C.size_t(len(request)), &response)
	// request is Go memory handed to C for the duration of the call; the host
	// copies it out before returning, but it must not be collected mid-call.
	goruntime.KeepAlive(request)

	if response.ptr != nil {
		defer C.free_host_buffer(response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed with code %d", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, nil
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

// hostCallJSON marshals the request, calls the host, and decodes the RPC
// envelope's result into result.
func hostCallJSON(method string, request, result any) error {
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return errMarshal
	}
	response, errCall := hostCall(method, raw)
	if errCall != nil {
		return errCall
	}
	if len(response) == 0 {
		return nil
	}
	var envelope pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(response, &envelope); errUnmarshal != nil {
		return errUnmarshal
	}
	if !envelope.OK {
		code, message := "host_error", "unknown host error"
		if envelope.Error != nil {
			code, message = envelope.Error.Code, envelope.Error.Message
		}
		return fmt.Errorf("%s: %s", code, message)
	}
	if len(envelope.Result) > 0 && result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	if host != nil {
		atomic.StorePointer(&hostAPI, unsafe.Pointer(host))
	}
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
	atomic.StorePointer(&hostAPI, nil)
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
	case pluginabi.MethodManagementRegister:
		return managementRegister(raw)
	case pluginabi.MethodManagementHandle:
		return managementHandle(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	runtime.lifecycleMu.Lock()
	defer runtime.lifecycleMu.Unlock()
	return configureLocked(raw)
}

func configureLocked(raw []byte) error {
	var request lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return errUnmarshal
	}
	if request.SchemaVersion < 2 {
		return fmt.Errorf("request lifecycle plugin requires host schema version 2 or newer")
	}

	cfg := pluginConfig{
		MaxEntries:                    defaultMaxEntries,
		MaxPendingEntries:             defaultMaxPendingEntries,
		CollectorRefreshBeforeSeconds: defaultCollectorRefreshBeforeSeconds,
		CollectorRetrySeconds:         defaultCollectorRetrySeconds,
	}
	if len(request.ConfigYAML) > 0 {
		decoder := yaml.NewDecoder(strings.NewReader(string(request.ConfigYAML)))
		decoder.KnownFields(true)
		if errUnmarshal := decoder.Decode(&cfg); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	if cfg.MaxEntries < 1 || cfg.MaxPendingEntries < 1 {
		return fmt.Errorf("max_entries and max_pending_entries must be greater than zero")
	}
	if cfg.CollectorRefreshBeforeSeconds < 1 || cfg.CollectorRefreshBeforeSeconds >= 3600 || cfg.CollectorRetrySeconds < 1 {
		return fmt.Errorf("collector_refresh_before_seconds must be between 1 and 3599 and collector_retry_seconds must be greater than zero")
	}
	if strings.TrimSpace(cfg.CollectorProxyURL) != "" {
		if errProxy := validateCollectorProxyURL(cfg.CollectorProxyURL); errProxy != nil {
			return fmt.Errorf("invalid collector_proxy_url: %w", errProxy)
		}
	}
	mode, valid := turnstate.ParseInjectMode(cfg.InjectMode)
	if !valid {
		return fmt.Errorf("inject_mode must be %q or %q, got %q", turnstate.ModeAlways, turnstate.ModeReplaceOnly, cfg.InjectMode)
	}

	runtime.mu.RLock()
	oldPlugin := runtime.plugin
	oldCollector := runtime.collector
	oldConfig := runtime.config
	runtime.mu.RUnlock()
	plugin := newPluginWithMode(cfg, mode)
	if oldPlugin != nil && oldConfig.MaxEntries == cfg.MaxEntries &&
		oldConfig.MaxPendingEntries == cfg.MaxPendingEntries {
		plugin = oldPlugin.WithInjectMode(mode)
	}
	collector, errCollector := buildAutoCollector(cfg, plugin)
	if errCollector != nil {
		return errCollector
	}
	if oldCollector != nil {
		oldCollector.Stop()
	}
	runtime.mu.Lock()
	runtime.plugin = plugin
	runtime.collector = collector
	runtime.config = cfg
	runtime.mu.Unlock()
	if collector != nil {
		collector.Start()
	}
	recordCollectorEvent(context.Background(), collectorEvent{
		Action:  collectorActionConfigured,
		Source:  "config",
		Message: map[bool]string{true: "内置采集器已启用", false: "内置采集器未配置代理"}[collector != nil],
	})
	clearTierCache()
	return nil
}

func buildAutoCollector(cfg pluginConfig, plugin *turnstate.Plugin) (*autoCollector, error) {
	if strings.TrimSpace(cfg.CollectorProxyURL) == "" {
		return nil, nil
	}
	probe, errProbe := newCollectorProbe(collectorProbeOptions{
		ProxyURL:       cfg.CollectorProxyURL,
		LoadCredential: loadCollectorCredential,
	})
	if errProbe != nil {
		return nil, fmt.Errorf("create built-in collector: %w", errProbe)
	}
	return newAutoCollector(autoCollectorOptions{
		MaxTargets:    cfg.MaxEntries,
		RefreshBefore: time.Duration(cfg.CollectorRefreshBeforeSeconds) * time.Second,
		RetryAfter:    time.Duration(cfg.CollectorRetrySeconds) * time.Second,
		Snapshot: func() []turnstate.BucketStatus {
			return plugin.Snapshot().Buckets
		},
		Probe: probe.Probe,
		Store: plugin.StoreForBucket,
		Event: recordCollectorEvent,
	}), nil
}

func newPlugin(cfg pluginConfig) *turnstate.Plugin {
	return newPluginWithMode(cfg, turnstate.ModeAlways)
}

func newPluginWithMode(cfg pluginConfig, mode turnstate.InjectMode) *turnstate.Plugin {
	return turnstate.NewPluginWithMode(
		turnstate.NewCache(cfg.MaxEntries, cfg.MaxPendingEntries, nil),
		logTurnState,
		mode,
	)
}

func resetRuntime() {
	runtime.lifecycleMu.Lock()
	defer runtime.lifecycleMu.Unlock()
	resetRuntimeLocked()
}

func resetRuntimeLocked() {
	runtime.mu.RLock()
	oldCollector := runtime.collector
	runtime.mu.RUnlock()
	if oldCollector != nil {
		oldCollector.Stop()
	}
	runtime.mu.Lock()
	runtime.plugin = newPlugin(pluginConfig{
		MaxEntries:        defaultMaxEntries,
		MaxPendingEntries: defaultMaxPendingEntries,
	})
	runtime.collector = nil
	runtime.collectorEvents = newCollectorEventStore(defaultCollectorEventLogPath)
	runtime.config = pluginConfig{
		MaxEntries:                    defaultMaxEntries,
		MaxPendingEntries:             defaultMaxPendingEntries,
		CollectorRefreshBeforeSeconds: defaultCollectorRefreshBeforeSeconds,
		CollectorRetrySeconds:         defaultCollectorRetrySeconds,
	}
	runtime.mu.Unlock()
}

func currentPlugin() *turnstate.Plugin {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.plugin
}

func currentCollector() *autoCollector {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.collector
}

func currentConfig() pluginConfig {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.config
}

func currentCollectorEventStore() *collectorEventStore {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.collectorEvents
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "5345asda",
			GitHubRepository: "https://github.com/5345asda/codex-turn-state-cache", ConfigFields: []pluginapi.ConfigField{
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
				{
					Name:        "inject_mode",
					Type:        pluginapi.ConfigFieldTypeEnum,
					EnumValues:  []string{string(turnstate.ModeAlways), string(turnstate.ModeReplaceOnly)},
					Description: "\"always\" (default) injects a live template on every request; \"replace-only\" rewrites only requests already carrying a degraded 312-length state. Any other value is rejected at startup.",
				},
				{
					Name:        "collector_proxy_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Dedicated http/https/socks5 proxy used only by automatic and manual turn-state collection. Empty disables built-in automatic collection.",
				},
				{
					Name:        "collector_refresh_before_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Automatically refresh a discovered account/model bucket this many seconds before expiry (default 300).",
				},
				{
					Name:        "collector_retry_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Minimum delay before retrying a failed or empty automatic collection (default 60).",
				},
			},
		},
		Capabilities: registrationCapabilities{
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
			ManagementAPI:          true,
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
	if collector := currentCollector(); collector != nil && strings.EqualFold(request.ToFormat, "codex") {
		authID, _ := request.Metadata["selected_auth_id"].(string)
		collector.Observe(turnstate.CacheKey{AuthID: authID, Model: request.Model})
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

func logCollectorEvent(ctx context.Context, event collectorEvent) {
	message := fmt.Sprintf("codex turn-state collector action=%s source=%s status=%d len=%d retry_in=%ds message=%s",
		event.Action, event.Source, event.StatusCode, event.StateLength, event.RetryInSeconds, event.Message)
	payload, _ := json.Marshal(hostLogRequest{
		HostCallbackID: hostCallbackID(ctx),
		Level:          event.Level,
		Message:        message,
		Fields: hostLogFields{
			PluginID: pluginID,
			AuthID:   event.AuthID,
			Model:    event.Model,
		},
	})
	callHostLog(payload)
}

func hostCallbackID(ctx context.Context) string {
	callbackID, _ := ctx.Value(hostCallbackIDKey{}).(string)
	return callbackID
}

func callHostLog(payload []byte) {
	_, _ = hostCall(pluginabi.MethodHostLog, payload)
}
