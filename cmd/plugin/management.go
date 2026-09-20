// Management API for the codex-turn-state-cache plugin.
//
// Two kinds of route are registered, and the difference matters:
//
//   - The HTML shell is a ResourceRoute. Those are served under
//     /v0/resource/plugins/<id>/ and the host does NOT authenticate them. So
//     the shell carries no data whatsoever: it is markup and script, and every
//     byte of state it displays is fetched afterwards by the browser from the
//     authenticated status route, with the operator's management key attached.
//
//   - The data routes are ManagementRoutes with no Menu. Those are served
//     under /v0/management/ behind the host's management middleware. The Menu
//     field is deliberately left empty on all of them: a GET route that
//     declares one is re-registered under the unauthenticated resource prefix
//     instead (routeDeclaresLegacyMenuResource in the host), which would
//     quietly strip the authentication off the very routes that need it.
//
// Status responses never contain a state value. The one exception is the
// explicit authenticated reveal POST used by the copy action; it is marked
// no-store and the dashboard does not persist its response.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed dashboard.html
var dashboardHTML []byte

// logPrefix matches the decision-log prefix used across the plugin.
const logPrefix = "[codex-turn-state-cache] "

// jsonResponse is the shared shape for management JSON responses.
func jsonResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, "could not encode the response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}
}

// Route suffixes. The host hands back a path that may be absolute or relative
// depending on how it resolved the registration, so dispatch matches on the
// suffix rather than on equality.
const (
	routeStatus     = "/" + pluginID + "/status"
	routeCacheClear = "/" + pluginID + "/cache/clear"
	routeProbe      = "/" + pluginID + "/buckets/probe"
	routeDelete     = "/" + pluginID + "/buckets/delete"
	routeReveal     = "/" + pluginID + "/buckets/reveal"
	// routeDashboard is relative to the plugin's own resource prefix, so the
	// browser-facing URL is /v0/resource/plugins/cpa-plugin-codex-turn-state/dashboard.
	routeDashboard = "/dashboard"
)

func managementRegister(raw []byte) ([]byte, error) {
	var request pluginapi.ManagementRegistrationRequest
	if len(raw) > 0 {
		// A malformed registration request is not worth failing over: the
		// paths are fixed, and the host resolves relative ones itself.
		_ = json.Unmarshal(raw, &request)
	}
	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: routeStatus},
			{Method: http.MethodPost, Path: routeCacheClear},
			{Method: http.MethodPost, Path: routeProbe},
			{Method: http.MethodPost, Path: routeDelete},
			{Method: http.MethodPost, Path: routeReveal},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				// Not "/": the host trims trailing slashes and rejects the
				// empty result, so registering the plugin root drops the
				// route with no log line -- the only symptom is a 404 long
				// after the registration that silently discarded it.
				Path:        routeDashboard,
				Menu:        "Codex Turn-State Cache",
				Description: "292 turn-state 防降智助手：缓存就绪度、降级观测、注入模式",
			},
		},
	})
}

func managementHandle(raw []byte) ([]byte, error) {
	var request pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return okEnvelope(managementError(http.StatusBadRequest, "could not decode the management request"))
	}

	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	method := strings.ToUpper(strings.TrimSpace(request.Method))

	switch {
	case hasRouteSuffix(path, routeStatus):
		if method != http.MethodGet && method != "" {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "status is a GET route"))
		}
		return okEnvelope(handleStatus(request.Query.Get("log_before")))
	case hasRouteSuffix(path, routeCacheClear):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "cache/clear is a POST route"))
		}
		return okEnvelope(handleCacheClear())
	case hasRouteSuffix(path, routeProbe):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "buckets/probe is a POST route"))
		}
		return okEnvelope(handleProbe(request.Body))
	case hasRouteSuffix(path, routeDelete):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "buckets/delete is a POST route"))
		}
		return okEnvelope(handleBucketDelete(request.Body))
	case hasRouteSuffix(path, routeReveal):
		if method != http.MethodPost {
			return okEnvelope(managementError(http.StatusMethodNotAllowed, "buckets/reveal is a POST route"))
		}
		return okEnvelope(handleBucketReveal(request.Body))
	case isDashboardPath(path):
		return okEnvelope(handleDashboard())
	default:
		return okEnvelope(managementError(http.StatusNotFound, "no such "+pluginID+" route: "+request.Path))
	}
}

// hasRouteSuffix matches a resolved path against a registered suffix. The host
// may present "/codex-turn-state-cache/status" or the full
// "/v0/management/codex-turn-state-cache/status"; both must land on the same
// handler.
func hasRouteSuffix(path, suffix string) bool {
	return strings.EqualFold(path, suffix) || strings.HasSuffix(strings.ToLower(path), strings.ToLower(suffix))
}

// isDashboardPath recognises the resource root, and only that. Matching on the
// plugin id alone would also catch /v0/management/codex-turn-state-cache,
// which would serve the HTML shell from the authenticated management prefix.
func isDashboardPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/resource/plugins/")
}

// handleDashboard serves the shell. It is deliberately data-free -- see the
// package comment for why that is not an oversight.
func handleDashboard() pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           []string{"text/html; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: dashboardHTML,
	}
}

type statusResponse struct {
	Plugin          string              `json:"plugin"`
	Version         string              `json:"version"`
	InjectMode      string              `json:"inject_mode"`
	Collector       autoCollectorStatus `json:"collector"`
	CollectorLog    collectorLogStatus  `json:"collector_log"`
	CollectorEvents []collectorEvent    `json:"collector_events"`
	GeneratedAt     string              `json:"generated_at"`
	Counters        turnstate.Counters  `json:"counters"`
	Buckets         []statusBucket      `json:"buckets"`
	Auths           []authInfo          `json:"auths"`
}

// statusBucket intentionally excludes the cached token. Values are revealed
// only through the explicit, authenticated copy endpoint below.
type statusBucket struct {
	AuthID    string    `json:"auth_id"`
	Model     string    `json:"model"`
	Ready     bool      `json:"ready"`
	Len       int       `json:"len"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// authInfo is one credential's display metadata for the dashboard. The
// subscription tier comes from the credential file's chatgpt_plan_type (the
// JWT claim CPA stores when the account logs in).
type authInfo struct {
	AuthID string `json:"auth_id"`
	Tier   string `json:"tier,omitempty"`
}

func handleStatus(rawLogBefore string) pluginapi.ManagementResponse {
	snapshot := currentPlugin().Snapshot()
	if snapshot.Buckets == nil {
		snapshot.Buckets = []turnstate.BucketStatus{}
	}
	auths := authInfos(snapshot.Buckets)
	cfg := currentConfig()
	collectorStatus := autoCollectorStatus{
		RefreshBeforeSeconds: cfg.CollectorRefreshBeforeSeconds,
		RetrySeconds:         cfg.CollectorRetrySeconds,
	}
	if collector := currentCollector(); collector != nil {
		collectorStatus = collector.Status(
			time.Duration(cfg.CollectorRefreshBeforeSeconds)*time.Second,
			time.Duration(cfg.CollectorRetrySeconds)*time.Second,
		)
	}
	collectorLog := currentCollectorEventStore()
	collectorEvents := []collectorEvent{}
	collectorLogStatus := collectorLogStatus{}
	if collectorLog != nil {
		logBefore, _ := strconv.ParseInt(strings.TrimSpace(rawLogBefore), 10, 64)
		if page, errEvents := collectorLog.Page(logBefore, collectorEventTailLimit); errEvents == nil {
			collectorEvents = page.Events
			collectorLogStatus = collectorLog.Status()
			collectorLogStatus.NextBefore = page.NextBefore
		} else {
			collectorLogStatus = collectorLog.Status()
		}
	}
	body, errMarshal := marshalStatus(snapshot, auths, cfg, collectorStatus, collectorLogStatus, collectorEvents)
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, "could not encode the status document")
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}
}

func marshalStatus(snapshot turnstate.Snapshot, auths []authInfo, cfg pluginConfig, collectorStatus autoCollectorStatus, collectorLog collectorLogStatus, collectorEvents []collectorEvent) ([]byte, error) {
	buckets := make([]statusBucket, 0, len(snapshot.Buckets))
	for _, bucket := range snapshot.Buckets {
		buckets = append(buckets, statusBucket{
			AuthID: bucket.AuthID, Model: bucket.Model, Ready: bucket.Ready,
			Len: bucket.Len, IssuedAt: bucket.IssuedAt, ExpiresAt: bucket.ExpiresAt,
		})
	}
	return json.Marshal(statusResponse{
		Plugin:          pluginID,
		Version:         pluginVersion,
		InjectMode:      string(currentPlugin().InjectMode()),
		Collector:       collectorStatus,
		CollectorLog:    collectorLog,
		CollectorEvents: collectorEvents,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
		Counters:        snapshot.Counters,
		Buckets:         buckets,
		Auths:           auths,
	})
}

func handleBucketReveal(body []byte) pluginapi.ManagementResponse {
	var req bucketRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return managementError(http.StatusBadRequest, "could not decode the request body as JSON")
	}
	key, errKey := req.key()
	if errKey != nil {
		return managementError(http.StatusBadRequest, errKey.Error())
	}
	if value, ok := currentPlugin().RevealForBucket(key); ok {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"application/json; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: mustJSON(map[string]any{"auth_id": key.AuthID, "model": key.Model, "value": value, "len": len(value)}),
		}
	}
	return managementError(http.StatusNotFound, "no ready cached state for this bucket")
}

func mustJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":"could not encode the response"}`)
	}
	return body
}

// handleCacheClear drops every cached template and resets the counters. The
// next successful response rebuilds the cache automatically, so clearing is
// reversible by traffic, not by an operator.
func handleCacheClear() pluginapi.ManagementResponse {
	currentPlugin().Clear()
	body, errMarshal := json.Marshal(map[string]string{"status": "cleared"})
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, "could not encode the response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func managementError(statusCode int, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(map[string]string{"error": message})
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// bucketRequest names one (account, model) pair. auth_id is caller-supplied
// input that reaches credential lookup and cache keys, so it goes through the
// same sanitiser as every other bucket-key path.
type bucketRequest struct {
	AuthID string `json:"auth_id"`
	Model  string `json:"model"`
}

func (r bucketRequest) key() (turnstate.CacheKey, error) {
	auth := strings.TrimSpace(r.AuthID)
	model := strings.TrimSpace(r.Model)
	if auth == "" || model == "" {
		return turnstate.CacheKey{}, fmt.Errorf("auth_id and model are both required")
	}
	for _, part := range []string{auth, model} {
		if strings.ContainsAny(part, `/\`) || strings.Contains(part, "..") || part == "." {
			return turnstate.CacheKey{}, fmt.Errorf("unsafe bucket component %q", part)
		}
	}
	return turnstate.CacheKey{AuthID: auth, Model: model}, nil
}

// handleBucketDelete drops one bucket's cached template. Traffic rebuilds it
// on the next successful response, so this is reversible by design.
func handleBucketDelete(body []byte) pluginapi.ManagementResponse {
	var req bucketRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return managementError(http.StatusBadRequest, "could not decode the request body as JSON")
		}
	}
	key, errKey := req.key()
	if errKey != nil {
		return managementError(http.StatusBadRequest, errKey.Error())
	}
	deleted := currentPlugin().DeleteBucket(key)
	log.Printf("%sdeleted bucket auth=%s model=%s existed=%t", logPrefix, key.AuthID, key.Model, deleted)
	return jsonResponse(http.StatusOK, map[string]any{
		"status":  "deleted",
		"deleted": deleted,
		"auth_id": key.AuthID,
		"model":   key.Model,
	})
}

// handleProbe issues one minimal upstream request locked to the exact
// (account, model) via host.model.execute and harvests the fresh 292 straight
// from the response headers the host hands back. Host-executed requests do not
// re-enter the plugin's interceptors (the host skips the calling plugin to
// prevent recursion), but the raw response -- headers included -- still comes
// back to us, which is what makes an on-demand harvest possible at all.
func handleProbe(body []byte) pluginapi.ManagementResponse {
	var req bucketRequest
	if len(strings.TrimSpace(string(body))) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return managementError(http.StatusBadRequest, "could not decode the request body as JSON")
		}
	}
	key, errKey := req.key()
	if errKey != nil {
		return managementError(http.StatusBadRequest, errKey.Error())
	}
	if collector := currentCollector(); collector != nil {
		return collectorProbeResponse(key, collector.CollectNow(context.Background(), key))
	}
	if !hostAPIAvailable() {
		return managementError(http.StatusServiceUnavailable,
			"this plugin holds no host callback table, so it could not issue a probe")
	}
	recordCollectorEvent(context.Background(), collectorEvent{
		Action:  collectorActionStarted,
		Source:  "manual-host",
		AuthID:  key.AuthID,
		Model:   key.Model,
		Message: "开始通过 CPA 主机执行手动探测",
	})

	// A deliberately minimal turn, with no X-Codex-Turn-State attached: sending
	// a stale value is what stops the upstream minting a fresh one.
	payload := map[string]any{
		"model": key.Model,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "ping"}},
		}},
		"store": false,
	}
	rawBody, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return managementError(http.StatusInternalServerError, errMarshal.Error())
	}

	exec := pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-responses",
		ExitProtocol:  "openai-responses",
		Model:         key.Model,
		Stream:        false,
		Body:          rawBody,
		Headers:       http.Header{"Content-Type": []string{"application/json"}},
		AuthID:        key.AuthID,
	}

	var execResp pluginapi.HostModelExecutionResponse
	out := map[string]any{
		"auth_id": key.AuthID,
		"model":   key.Model,
	}
	errCall := hostCallJSON(pluginabi.MethodHostModelExecute, exec, &execResp)
	if errCall != nil {
		out["reached"] = false
		out["harvested"] = false
		out["error"] = errCall.Error()
		log.Printf("%sprobe failed auth=%s model=%s: %v", logPrefix, key.AuthID, key.Model, errCall)
		recordCollectorEvent(context.Background(), collectorResultEvent(
			collectorProbeResult{Err: errCall}, "manual-host", key, 0,
		))
		return jsonResponse(http.StatusOK, out)
	}
	out["reached"] = true
	out["status_code"] = execResp.StatusCode

	value := headerValue(execResp.Headers, turnstate.TurnStateHeader)
	out["len"] = len(value)
	probeResult := collectorProbeResult{
		Reached:    true,
		StatusCode: execResp.StatusCode,
		Length:     len(value),
	}
	switch {
	case len(value) == turnstate.StateLength:
		outcome := currentPlugin().StoreForBucket(key, value)
		harvested := outcome == turnstate.StoreTemplate || outcome == turnstate.StoreReplaced
		out["harvested"] = harvested
		probeResult.Harvested = harvested
		if harvested {
			log.Printf("%sprobe harvested fresh template auth=%s model=%s len=%d", logPrefix, key.AuthID, key.Model, len(value))
		} else {
			out["note"] = "upstream state was invalid, stale, or not newer than the cached template"
			probeResult.Note = out["note"].(string)
		}
	case len(value) == turnstate.DegradedLength:
		out["harvested"] = false
		out["note"] = "upstream issued a degraded 312 state; nothing to harvest"
		probeResult.Note = "upstream issued a degraded 312 state; nothing to harvest"
		log.Printf("%sprobe observed degraded state auth=%s model=%s", logPrefix, key.AuthID, key.Model)
	default:
		out["harvested"] = false
		if value != "" {
			out["note"] = "unexpected state length"
			probeResult.Note = "unexpected state length"
		}
	}
	recordCollectorEvent(context.Background(), collectorResultEvent(probeResult, "manual-host", key, 0))
	return jsonResponse(http.StatusOK, out)
}

func collectorProbeResponse(key turnstate.CacheKey, result collectorProbeResult) pluginapi.ManagementResponse {
	out := map[string]any{
		"auth_id":     key.AuthID,
		"model":       key.Model,
		"reached":     result.Reached,
		"harvested":   result.Harvested,
		"status_code": result.StatusCode,
		"len":         result.Length,
	}
	if result.Note != "" {
		out["note"] = result.Note
	}
	if result.Err != nil {
		out["error"] = result.Err.Error()
	}
	return jsonResponse(http.StatusOK, out)
}

// ── subscription tier lookup ──────────────────────────────────────────────

// tierCache memoises the subscription tier per credential for ten minutes, so
// polling the dashboard does not pull every auth file through host.auth.get
// on every refresh.
var (
	tierMu       sync.Mutex
	tierCache    = map[string]tierEntry{}
	tierCacheTTL = 10 * time.Minute
)

type tierEntry struct {
	tier    string
	fetched time.Time
}

// authInfos reports display metadata for every account that owns a bucket.
// Tier lookup failures degrade to an empty tier -- a missing tier must never
// hide the bucket itself.
func authInfos(buckets []turnstate.BucketStatus) []authInfo {
	seen := map[string]bool{}
	ids := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if !seen[b.AuthID] {
			seen[b.AuthID] = true
			ids = append(ids, b.AuthID)
		}
	}
	infos := make([]authInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, authInfo{AuthID: id, Tier: lookupTier(id)})
	}
	return infos
}

func lookupTier(authID string) string {
	tierMu.Lock()
	cached, ok := tierCache[authID]
	tierMu.Unlock()
	if ok && time.Since(cached.fetched) < tierCacheTTL {
		return cached.tier
	}
	tier := fetchTier(authID)
	tierMu.Lock()
	tierCache[authID] = tierEntry{tier: tier, fetched: time.Now()}
	tierMu.Unlock()
	return tier
}

func clearTierCache() {
	tierMu.Lock()
	tierCache = map[string]tierEntry{}
	tierMu.Unlock()
}

// fetchTier resolves the credential's chatgpt_plan_type. host.auth.list names
// the credentials; host.auth.get returns the physical auth file JSON, whose
// access_token JWT carries the plan claim. Both steps degrade to "" on any
// failure -- tiers are decoration, buckets are the product.
func fetchTier(authID string) string {
	raw, errLoad := fetchAuthJSON(authID)
	if errLoad != nil {
		return ""
	}
	return planTypeFromAuthJSON(raw)
}

func fetchAuthJSON(authID string) ([]byte, error) {
	if !hostAPIAvailable() {
		return nil, fmt.Errorf("host API unavailable")
	}
	var listResp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := hostCallJSON(pluginabi.MethodHostAuthList, struct{}{}, &listResp); err != nil {
		return nil, err
	}
	authIndex := ""
	for _, file := range listResp.Files {
		if file.ID == authID || file.Name == authID || file.AuthIndex == authID {
			authIndex = file.AuthIndex
			break
		}
	}
	if authIndex == "" {
		return nil, fmt.Errorf("credential %q was not found", authID)
	}
	var getResp struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := hostCallJSON(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": authIndex}, &getResp); err != nil {
		return nil, err
	}
	if len(getResp.JSON) == 0 {
		return nil, fmt.Errorf("credential %q returned empty JSON", authID)
	}
	return getResp.JSON, nil
}

// planTypeFromAuthJSON reads chatgpt_plan_type the way CPA itself stores it:
// either as a literal field in the auth file, or inside the access_token JWT
// payload. Signature verification is irrelevant here -- the file is the
// operator's own credential, and the claim is plain data.
func planTypeFromAuthJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if tier := stringField(doc, "chatgpt_plan_type"); tier != "" {
		return tier
	}
	if nested, ok := doc["account"].(map[string]any); ok {
		if tier := stringField(nested, "chatgpt_plan_type"); tier != "" {
			return tier
		}
	}
	token, _ := doc["access_token"].(string)
	if token == "" {
		if tokens, ok := doc["tokens"].(map[string]any); ok {
			token, _ = tokens["access_token"].(string)
		}
	}
	return planTypeFromJWT(token)
}

func planTypeFromJWT(token string) string {
	if token == "" {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return stringField(claims, "chatgpt_plan_type")
}

func stringField(doc map[string]any, key string) string {
	if doc == nil {
		return ""
	}
	if v, ok := doc[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// headerValue returns the first value of the named header, case-insensitively.
func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
