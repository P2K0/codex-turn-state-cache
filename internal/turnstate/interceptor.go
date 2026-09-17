package turnstate

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const selectedAuthIDMetadataKey = "selected_auth_id"

// Plugin implements the CPA interception hooks used by the dynamic ABI adapter.
type Plugin struct {
	cache *Cache
}

// NewPlugin creates a hook implementation around one process-local cache.
func NewPlugin(cache *Cache) *Plugin {
	return &Plugin{cache: cache}
}

// InterceptRequestBeforeAuth intentionally preserves the original request.
func (p *Plugin) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	return pluginapi.RequestInterceptResponse{}, nil
}

// InterceptRequestAfterAuth replaces the state only when a matching local value exists.
func (p *Plugin) InterceptRequestAfterAuth(_ context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if p == nil || p.cache == nil || !strings.EqualFold(req.ToFormat, "codex") {
		return pluginapi.RequestInterceptResponse{}, nil
	}

	key, ok := afterAuthKey(req)
	if !ok {
		p.cache.ForgetRequest(req.RequestID)
		return pluginapi.RequestInterceptResponse{}, nil
	}
	p.cache.BindRequest(req.RequestID, key)
	if state, hit := p.cache.Lookup(key); hit {
		return pluginapi.RequestInterceptResponse{
			Headers: http.Header{TurnStateHeader: []string{state}},
		}, nil
	}
	return pluginapi.RequestInterceptResponse{}, nil
}

// HandleRequestComplete removes request-local correlation data.
func (p *Plugin) HandleRequestComplete(_ context.Context, completion pluginapi.RequestCompletion) error {
	if p != nil && p.cache != nil {
		p.cache.ForgetRequest(completion.RequestID)
	}
	return nil
}

// InterceptResponse records a valid raw upstream header without changing the downstream response.
func (p *Plugin) InterceptResponse(_ context.Context, req pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	if p != nil && p.cache != nil {
		p.cache.StoreResponseForRequest(req.RequestID, turnStateValues(req.ResponseHeaders))
	}
	return pluginapi.ResponseInterceptResponse{}, nil
}

// InterceptStreamChunk records the raw upstream header only during stream initialization.
func (p *Plugin) InterceptStreamChunk(_ context.Context, req pluginapi.StreamChunkInterceptRequest) (pluginapi.StreamChunkInterceptResponse, error) {
	if p != nil && p.cache != nil && req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		p.cache.StoreResponseForRequest(req.RequestID, turnStateValues(req.ResponseHeaders))
	}
	return pluginapi.StreamChunkInterceptResponse{}, nil
}

func afterAuthKey(req pluginapi.RequestInterceptRequest) (CacheKey, bool) {
	key := CacheKey{AuthID: selectedAuthID(req.Metadata), Model: req.Model}
	return key, req.RequestID != "" && validKey(key)
}

func selectedAuthID(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	authID, _ := metadata[selectedAuthIDMetadataKey].(string)
	return authID
}

func turnStateValues(headers http.Header) []string {
	if len(headers) == 0 {
		return nil
	}
	var values []string
	for name, headerValues := range headers {
		if strings.EqualFold(name, TurnStateHeader) {
			values = append(values, headerValues...)
		}
	}
	return values
}
