package turnstate

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const selectedAuthIDMetadataKey = "selected_auth_id"

type Plugin struct {
	cache *Cache
	log   func(context.Context, string, string)
}

func NewPlugin(cache *Cache, log func(context.Context, string, string)) *Plugin {
	return &Plugin{cache: cache, log: log}
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
	if p.log != nil {
		p.log(ctx, "codex turn-state cache injected", key.Model)
	}
	return pluginapi.RequestInterceptResponse{
		Headers: http.Header{TurnStateHeader: {state}},
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

func (p *Plugin) capture(ctx context.Context, requestID string, headers http.Header, source string) {
	key, stored, replaced := p.cache.StoreResponseForRequest(requestID, turnStateValues(headers))
	if !stored || p.log == nil {
		return
	}
	message := "codex turn-state cache captured source=" + source
	if replaced {
		message += " replaced=true"
	}
	p.log(ctx, message, key.Model)
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
