package turnstate

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const selectedAuthIDMetadataKey = "selected_auth_id"

// InjectMode selects what the plugin does when it holds a live template for a
// bucket. ModeAlways injects on every request (the v0.1 behavior, kept as the
// default for compatibility); ModeReplaceOnly rewrites only requests already
// carrying a degraded-length state, leaving headerless requests untouched.
type InjectMode string

const (
	ModeAlways      InjectMode = "always"
	ModeReplaceOnly InjectMode = "replace-only"
)

func ParseInjectMode(value string) (InjectMode, bool) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", string(ModeAlways):
		return ModeAlways, true
	case string(ModeReplaceOnly):
		return ModeReplaceOnly, true
	default:
		return "", false
	}
}

type Plugin struct {
	cache      *Cache
	log        func(context.Context, string, string)
	injectMode InjectMode
}

func NewPlugin(cache *Cache, log func(context.Context, string, string)) *Plugin {
	return NewPluginWithMode(cache, log, ModeAlways)
}

func NewPluginWithMode(cache *Cache, log func(context.Context, string, string), mode InjectMode) *Plugin {
	if mode == "" {
		mode = ModeAlways
	}
	return &Plugin{cache: cache, log: log, injectMode: mode}
}

func (p *Plugin) InjectMode() InjectMode {
	return p.injectMode
}

// WithInjectMode returns a plugin view with the requested injection policy
// while retaining the cache and its in-flight request bindings.
func (p *Plugin) WithInjectMode(mode InjectMode) *Plugin {
	if mode == "" {
		mode = ModeAlways
	}
	return &Plugin{cache: p.cache, log: p.log, injectMode: mode}
}

func (p *Plugin) InterceptRequestBeforeAuth(_ context.Context, _ pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	return pluginapi.RequestInterceptResponse{}, nil
}

// InterceptRequestAfterAuth only reuses state for CPA's selected credential and exact model.
func (p *Plugin) InterceptRequestAfterAuth(ctx context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if !strings.EqualFold(req.ToFormat, "codex") {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	key := CacheKey{AuthID: selectedAuthID(req.Metadata), Model: req.Model}
	if req.RequestID == "" || !validKey(key) {
		p.cache.ForgetRequest(req.RequestID)
		return pluginapi.RequestInterceptResponse{}, nil
	}
	p.cache.BindRequest(req.RequestID, key)
	state, hit := p.cache.Lookup(key)
	if !hit {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	incoming := headerValue(req.Headers, TurnStateHeader)
	if p.injectMode == ModeReplaceOnly && (incoming == "" || len(incoming) != DegradedLength) {
		// Headerless or non-degraded requests stay untouched in replace-only
		// mode: forcing a template onto a request that carried no state is
		// the one behavior the deployment spec prohibits.
		return pluginapi.RequestInterceptResponse{}, nil
	}
	message := "codex turn-state cache injected"
	if p.injectMode == ModeReplaceOnly {
		message = "codex turn-state cache substituted degraded state"
		p.cache.CountSubstituted()
	} else {
		p.cache.CountInjected()
	}
	if p.log != nil {
		p.log(ctx, message, key.Model)
	}
	// ClearHeaders is applied before Headers and sweeps case-insensitively,
	// while the Headers merge is a canonicalizing Del+Add. Clearing first is
	// what guarantees a replacement rather than a second copy alongside a
	// non-canonically spelled original.
	return pluginapi.RequestInterceptResponse{
		ClearHeaders: []string{TurnStateHeader},
		Headers:      http.Header{TurnStateHeader: {state}},
	}, nil
}

func (p *Plugin) HandleRequestComplete(_ context.Context, completion pluginapi.RequestCompletion) error {
	p.cache.ForgetRequest(completion.RequestID)
	return nil
}

func (p *Plugin) InterceptResponse(ctx context.Context, req pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	p.capture(ctx, req.RequestID, req.ResponseHeaders, "http")
	return pluginapi.ResponseInterceptResponse{}, nil
}

func (p *Plugin) InterceptStreamChunk(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) (pluginapi.StreamChunkInterceptResponse, error) {
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		p.capture(ctx, req.RequestID, req.ResponseHeaders, "stream")
	}
	return pluginapi.StreamChunkInterceptResponse{}, nil
}

func (p *Plugin) Snapshot() Snapshot {
	return p.cache.Snapshot()
}

func (p *Plugin) Clear() {
	p.cache.Clear()
}

// DeleteBucket drops one (account, model) bucket's cached template.
func (p *Plugin) DeleteBucket(key CacheKey) bool {
	return p.cache.Delete(key)
}

// StoreForBucket stores a template harvested outside the request flow (the
// management probe path).
func (p *Plugin) StoreForBucket(key CacheKey, value string) StoreOutcome {
	return p.cache.StoreForBucket(key, value)
}

func (p *Plugin) RevealForBucket(key CacheKey) (string, bool) {
	return p.cache.Reveal(key)
}

func (p *Plugin) capture(ctx context.Context, requestID string, headers http.Header, source string) {
	key, outcome := p.cache.StoreResponseForRequest(requestID, turnStateValues(headers))
	if p.log == nil {
		return
	}
	switch outcome {
	case StoreDegraded:
		// A degraded-length state is the upstream's throttle signal. It is
		// never stored; this line is the monitoring wire that makes the
		// degradation visible in the decision log.
		p.log(ctx, "codex turn-state cache degraded state observed source="+source, key.Model)
	case StoreTemplate:
		p.log(ctx, "codex turn-state cache captured source="+source, key.Model)
	case StoreReplaced:
		p.log(ctx, "codex turn-state cache captured source="+source+" replaced=true", key.Model)
	}
}

func selectedAuthID(metadata map[string]any) string {
	authID, _ := metadata[selectedAuthIDMetadataKey].(string)
	return authID
}

func turnStateValues(headers http.Header) []string {
	var values []string
	for name, headerValues := range headers {
		if strings.EqualFold(name, TurnStateHeader) {
			values = append(values, headerValues...)
		}
	}
	return values
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
