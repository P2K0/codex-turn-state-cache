//go:build cgo

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegisterDeclaresPluginContract(t *testing.T) {
	var registered registration
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion}, &registered)
	if registered.SchemaVersion != pluginabi.SchemaVersion || registered.Metadata.Name != pluginID || registered.Metadata.Version != pluginVersion {
		t.Fatalf("registration = %#v", registered)
	}
	if registered.Metadata.Author != "5345asda" || registered.Metadata.GitHubRepository != "https://github.com/5345asda/codex-turn-state-cache" {
		t.Fatalf("metadata = %#v", registered.Metadata)
	}
	if !registered.Capabilities.RequestInterceptor || !registered.Capabilities.RequestLifecyclePlugin || !registered.Capabilities.ResponseInterceptor || !registered.Capabilities.StreamChunkInterceptor {
		t.Fatalf("capabilities = %#v", registered.Capabilities)
	}
	if !registered.Capabilities.ManagementAPI {
		t.Fatalf("management capability missing: %#v", registered.Capabilities)
	}
	foundCollectorProxy := false
	for _, field := range registered.Metadata.ConfigFields {
		if field.Name == "collector_proxy_url" {
			foundCollectorProxy = field.Type == pluginapi.ConfigFieldTypeString
		}
	}
	if !foundCollectorProxy {
		t.Fatal("registration does not expose collector_proxy_url")
	}
}

func TestConfigureRejectsInvalidCollectorProxy(t *testing.T) {
	raw, errMarshal := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("collector_proxy_url: ftp://proxy.invalid:21\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, raw); err == nil {
		t.Fatal("unsupported collector proxy scheme must fail configuration")
	}
}

func TestConfiguredCollectorObservesAfterAuthBucket(t *testing.T) {
	configureTestPlugin(t, "collector_proxy_url: http://127.0.0.1:1\n")
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("observe", "auth-a", "gpt-a"), &pluginapi.RequestInterceptResponse{})

	var status struct {
		Collector struct {
			Enabled bool `json:"enabled"`
			Targets int  `json:"targets"`
		} `json:"collector"`
	}
	callManagement(t, "GET", "/v0/management/cpa-plugin-codex-turn-state/status", nil, &status)
	if !status.Collector.Enabled || status.Collector.Targets != 1 {
		t.Fatalf("collector status = %#v", status.Collector)
	}
}

func TestConfigureRejectsUnknownInjectMode(t *testing.T) {
	raw, errMarshal := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("inject_mode: replace_only\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, raw); err == nil {
		t.Fatal("a typo in inject_mode must fail loudly, not silently degrade into the forcing mode")
	}
}

func TestConfigureRejectsUnknownFields(t *testing.T) {
	raw, errMarshal := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("max_entries: 8\nmax_pending_entries: 8\ncollector_rety_seconds: 30\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, raw); err == nil || !strings.Contains(err.Error(), "collector_rety_seconds") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestConfigureAcceptsHostManagedFields(t *testing.T) {
	raw, errMarshal := json.Marshal(lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte("enabled: true\npriority: 0\nstore:\n  version: 0.3.0\n  release-tag: v0.3.0\nmax_entries: 8\nmax_pending_entries: 8\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if _, err := handleMethod(pluginabi.MethodPluginRegister, raw); err != nil {
		t.Fatalf("host-managed fields must be accepted: %v", err)
	}
}

func TestCollectorOnlyReconfigurePreservesCache(t *testing.T) {
	configureTestPlugin(t, "collector_retry_seconds: 60\n")
	template := validTemplate(time.Now().Add(-time.Minute))
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture-reconfigure", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture-reconfigure",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {template}},
	}, &pluginapi.ResponseInterceptResponse{})

	var registered registration
	callPluginMethod(t, pluginabi.MethodPluginReconfigure, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML: []byte("max_entries: 8\nmax_pending_entries: 8\n" +
			"collector_retry_seconds: 120\n"),
	}, &registered)

	var response pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("after-reconfigure", "auth-a", "gpt-5"), &response)
	if got := response.Headers.Get(turnstate.TurnStateHeader); got != template {
		t.Fatalf("collector-only reconfigure lost cached template: got len=%d", len(got))
	}
}

func TestQuiesceClearsCachedState(t *testing.T) {
	configureTestPlugin(t, "")
	state := validTemplate(time.Now().Add(-time.Minute))
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {state}},
	}, &pluginapi.ResponseInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodPluginQuiesce, struct{}{}, &struct{}{})

	var miss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("after-quiesce", "auth-a", "gpt-5"), &miss)
	if len(miss.Headers) != 0 {
		t.Fatalf("quiesce did not clear cached state: %#v", miss.Headers)
	}
}

func TestReplaceOnlySubstitutesDegradedStateOverRPC(t *testing.T) {
	configureTestPlugin(t, "inject_mode: replace-only\n")
	template := validTemplate(time.Now().Add(-time.Minute))
	degraded := strings.Repeat("d", turnstate.DegradedLength)

	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {template}},
	}, &pluginapi.ResponseInterceptResponse{})

	var substituted pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequestHeaders("degraded", "auth-a", "gpt-5", http.Header{
		turnstate.TurnStateHeader: {degraded},
	}), &substituted)
	if substituted.Headers.Get(turnstate.TurnStateHeader) != template {
		t.Fatalf("degraded state was not substituted: %#v", substituted.Headers)
	}

	var untouched pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("bare", "auth-a", "gpt-5"), &untouched)
	if untouched.Headers.Get(turnstate.TurnStateHeader) != "" {
		t.Fatalf("headerless request was force-injected: %#v", untouched.Headers)
	}
}

func TestDegradedResponseIsCountedNotStored(t *testing.T) {
	configureTestPlugin(t, "")
	degraded := strings.Repeat("d", turnstate.DegradedLength)
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {degraded}},
	}, &pluginapi.ResponseInterceptResponse{})

	var miss pluginapi.RequestInterceptResponse
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("after", "auth-a", "gpt-5"), &miss)
	if miss.Headers.Get(turnstate.TurnStateHeader) != "" {
		t.Fatalf("degraded state was stored: %#v", miss.Headers)
	}

	var status struct {
		Counters struct {
			DegradedObserved int64 `json:"degraded_observed"`
		} `json:"counters"`
	}
	callManagement(t, "GET", "/v0/management/cpa-plugin-codex-turn-state/status", nil, &status)
	if status.Counters.DegradedObserved != 1 {
		t.Fatalf("degraded_observed = %d", status.Counters.DegradedObserved)
	}
}

func TestManagementRoutesServeStatusAndClear(t *testing.T) {
	configureTestPlugin(t, "")
	template := validTemplate(time.Now().Add(-time.Minute))
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:       "capture",
		ResponseHeaders: http.Header{turnstate.TurnStateHeader: {template}},
	}, &pluginapi.ResponseInterceptResponse{})

	var status struct {
		Plugin  string `json:"plugin"`
		Version string `json:"version"`
		Buckets []struct {
			AuthID string `json:"auth_id"`
			Model  string `json:"model"`
			Ready  bool   `json:"ready"`
			Len    int    `json:"len"`
		} `json:"buckets"`
	}
	statusRaw := callManagementRaw(t, "GET", "/v0/management/cpa-plugin-codex-turn-state/status", nil)
	if bytes.Contains(statusRaw.Body, []byte(template)) || bytes.Contains(statusRaw.Body, []byte(`"value"`)) {
		t.Fatalf("status leaked cached state: %s", statusRaw.Body)
	}
	if err := json.Unmarshal(statusRaw.Body, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Plugin != pluginID || status.Version != pluginVersion {
		t.Fatalf("status = %#v", status)
	}
	if len(status.Buckets) != 1 || status.Buckets[0].AuthID != "auth-a" || !status.Buckets[0].Ready {
		t.Fatalf("buckets = %#v", status.Buckets)
	}
	if status.Buckets[0].Len != turnstate.StateLength {
		t.Fatalf("bucket length field wrong: len=%d", status.Buckets[0].Len)
	}
	var revealed struct {
		Value string `json:"value"`
	}
	revealResponse := callManagementRaw(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/reveal", map[string]any{"auth_id": "auth-a", "model": "gpt-5"})
	if revealResponse.StatusCode != http.StatusOK || !strings.Contains(string(revealResponse.Body), template) {
		t.Fatalf("reveal response = %#v", revealResponse)
	}
	if err := json.Unmarshal(revealResponse.Body, &revealed); err != nil || revealed.Value != template {
		t.Fatalf("revealed value = %q, err=%v", revealed.Value, err)
	}
	if revealResponse.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal cache policy = %q", revealResponse.Headers.Get("Cache-Control"))
	}

	var cleared map[string]string
	callManagement(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/cache/clear", nil, &cleared)
	if cleared["status"] != "cleared" {
		t.Fatalf("clear response = %#v", cleared)
	}

	callManagement(t, "GET", "/v0/management/cpa-plugin-codex-turn-state/status", nil, &status)
	if len(status.Buckets) != 0 {
		t.Fatalf("buckets after clear = %#v", status.Buckets)
	}
}

func TestBucketDeleteDropsOneBucket(t *testing.T) {
	configureTestPlugin(t, "")
	template := validTemplate(time.Now().Add(-time.Minute))
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture-a", "auth-a", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	callPluginMethod(t, pluginabi.MethodRequestInterceptAfter, afterAuthRequest("capture-b", "auth-b", "gpt-5"), &pluginapi.RequestInterceptResponse{})
	for _, id := range []string{"capture-a", "capture-b"} {
		callPluginMethod(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
			RequestID:       id,
			ResponseHeaders: http.Header{turnstate.TurnStateHeader: {template}},
		}, &pluginapi.ResponseInterceptResponse{})
	}

	var deleted map[string]any
	callManagement(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/delete",
		map[string]any{"auth_id": "auth-a", "model": "gpt-5"}, &deleted)
	if deleted["deleted"] != true {
		t.Fatalf("delete response = %#v", deleted)
	}

	var status struct {
		Buckets []struct {
			AuthID string `json:"auth_id"`
		} `json:"buckets"`
	}
	callManagement(t, "GET", "/v0/management/cpa-plugin-codex-turn-state/status", nil, &status)
	if len(status.Buckets) != 1 || status.Buckets[0].AuthID != "auth-b" {
		t.Fatalf("buckets after delete = %#v", status.Buckets)
	}

	// Deleting again reports deleted=false rather than erroring.
	callManagement(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/delete",
		map[string]any{"auth_id": "auth-a", "model": "gpt-5"}, &deleted)
	if deleted["deleted"] != false {
		t.Fatalf("second delete response = %#v", deleted)
	}

	// Unsafe bucket components are rejected.
	if msg := callManagementErr(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/delete",
		map[string]any{"auth_id": "../escape", "model": "gpt-5"}); msg == "" {
		t.Fatal("unsafe bucket component was accepted")
	}
}

func TestBucketProbeWithoutHostAPIFailsLoudly(t *testing.T) {
	configureTestPlugin(t, "")
	// No host callback table in tests: the probe must say it did not run,
	// never answer 200 with reached=false.
	callManagementErr(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/probe",
		map[string]any{"auth_id": "auth-a", "model": "gpt-5"})
}

func TestBucketProbeUsesBuiltInCollectorWhenConfigured(t *testing.T) {
	configureTestPlugin(t, "")
	template := validTemplate(time.Now().Add(-time.Minute))
	collector := newAutoCollector(autoCollectorOptions{
		MaxTargets:    8,
		RefreshBefore: 5 * time.Minute,
		RetryAfter:    time.Minute,
		Probe: func(context.Context, turnstate.CacheKey) collectorProbeResult {
			return collectorProbeResult{
				Reached: true, Harvested: true, StatusCode: http.StatusOK,
				Length: len(template), State: template,
			}
		},
		Snapshot: func() []turnstate.BucketStatus { return currentPlugin().Snapshot().Buckets },
		Store:    currentPlugin().StoreForBucket,
	})
	runtime.mu.Lock()
	runtime.collector = collector
	runtime.config.CollectorProxyURL = "http://collector.invalid:8080"
	runtime.mu.Unlock()

	var result struct {
		Reached   bool `json:"reached"`
		Harvested bool `json:"harvested"`
		Length    int  `json:"len"`
	}
	callManagement(t, "POST", "/v0/management/cpa-plugin-codex-turn-state/buckets/probe",
		map[string]any{"auth_id": "auth-a", "model": "gpt-a"}, &result)
	if !result.Reached || !result.Harvested || result.Length != turnstate.StateLength {
		t.Fatalf("probe result = %#v", result)
	}
	snapshot := currentPlugin().Snapshot()
	revealed, ok := currentPlugin().RevealForBucket(turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"})
	if len(snapshot.Buckets) != 1 || !ok || revealed != template {
		t.Fatalf("built-in probe did not populate cache")
	}
}

func TestManagementRegisterKeepsDataRoutesOffTheMenu(t *testing.T) {
	var registration pluginapi.ManagementRegistrationResponse
	callPluginMethod(t, pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{}, &registration)

	for _, resource := range registration.Resources {
		if resource.Path == routeDashboard {
			if resource.Menu == "" {
				t.Fatal("dashboard resource lost its menu entry")
			}
		}
		if strings.Contains(resource.Path, "status") || strings.Contains(resource.Path, "clear") {
			t.Fatalf("data route registered as a resource: %+v", resource)
		}
	}
	if dashboard := countPaths(registration.Resources, routeDashboard); dashboard != 1 {
		t.Fatalf("dashboard resource count = %d", dashboard)
	}
	for _, route := range registration.Routes {
		if route.Menu != "" {
			// A GET route with a Menu is re-registered under the unauthenticated
			// resource prefix by the host, which would strip the authentication
			// off the very routes that need it.
			t.Fatalf("management route declared a Menu: %+v", route)
		}
	}
	if len(registration.Routes) != 5 {
		t.Fatalf("routes = %+v", registration.Routes)
	}
}

func TestDashboardShellIsServedWithoutData(t *testing.T) {
	configureTestPlugin(t, "")
	var response pluginapi.ManagementResponse
	callPluginMethod(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/codex-turn-state-cache/dashboard",
	}, &response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", response.StatusCode)
	}
	body := string(response.Body)
	if !strings.Contains(body, pluginID) {
		t.Fatal("dashboard shell is not the expected page")
	}
	template := validTemplate(time.Now().Add(-time.Minute))
	if strings.Contains(body, template) {
		t.Fatal("dashboard shell leaked a state value")
	}
}

func TestDashboardKeepsAccountAndModelRowsAligned(t *testing.T) {
	body := string(dashboardHTML)
	modelRowStart := strings.Index(body, `<tr class="model`)
	if modelRowStart < 0 {
		t.Fatal("dashboard model row template is missing")
	}
	modelRowEnd := strings.Index(body[modelRowStart:], `</tr>`)
	if modelRowEnd < 0 {
		t.Fatal("dashboard model row template is incomplete")
	}
	modelRow := body[modelRowStart : modelRowStart+modelRowEnd]
	if cells := strings.Count(modelRow, `<td`); cells != 7 {
		t.Fatalf("model row cell count = %d, want 7 so it aligns with the table header", cells)
	}
	if !strings.Contains(modelRow, `class="cell tier-spacer"`) {
		t.Fatal("model row must leave the account-only subscription column empty")
	}
	accountButtonStart := strings.Index(body, `.acct-btn {`)
	if accountButtonStart < 0 {
		t.Fatal("account button styles are missing")
	}
	accountButtonEnd := strings.Index(body[accountButtonStart:], `}`)
	if accountButtonEnd < 0 {
		t.Fatal("account button styles are incomplete")
	}
	accountButtonStyles := body[accountButtonStart : accountButtonStart+accountButtonEnd]
	if !strings.Contains(accountButtonStyles, `white-space: nowrap;`) {
		t.Fatal("account row must explicitly prevent wrapping")
	}
}

func TestDashboardReservesSpaceForLifeActionsAndCopy(t *testing.T) {
	body := string(dashboardHTML)
	for _, rule := range []string{
		`col.c-name { width: 22%; }`,
		`col.c-life { width: 14%; }`,
		`col.c-actions { width: 16%; }`,
		`max-width: 26ch;`,
		`class="acct-name" title="${esc(auth)}"`,
		`class="cell life-cell"`,
		`class="cell turn-cell"`,
		`width: 100%; justify-content: flex-start;`,
	} {
		if !strings.Contains(body, rule) {
			t.Fatalf("dashboard is missing layout/copy rule %q", rule)
		}
	}
	if !strings.Contains(body, `const chip = ev.target.closest(".val-chip")`) ||
		!strings.Contains(body, `await navigator.clipboard.writeText(value)`) {
		t.Fatal("turn-state click must copy the complete value")
	}
}

func TestDashboardFitsCompactViewportWithoutHidingActions(t *testing.T) {
	body := string(dashboardHTML)
	for _, rule := range []string{
		`@media (max-width: 1000px)`,
		`table { min-width: 100%; }`,
		`col.c-name { width: 21%; }`,
		`col.c-life { width: 15%; }`,
		`col.c-time { width: 18%; }`,
		`col.c-actions { width: 16%; }`,
		`.next-life-label { display: none; }`,
	} {
		if !strings.Contains(body, rule) {
			t.Fatalf("dashboard is missing compact viewport rule %q", rule)
		}
	}
}

func TestDashboardOffersPersistentCollectorSettings(t *testing.T) {
	body := string(dashboardHTML)
	for _, contract := range []string{
		`id="btnCollectorSettings"`,
		`id="collectorSettings"`,
		`type="password" id="collectorProxyInput"`,
		`id="collectorRefreshInput"`,
		`id="collectorRetryInput"`,
		`collector_refresh_before_seconds`,
		`collector_retry_seconds`,
		`collector_proxy_url: null`,
		`"http:", "https:", "socks5:", "socks5h:"`,
		`/v0/management/plugins/${PLUGIN_ID}/config`,
		`"PATCH"`,
	} {
		if !strings.Contains(body, contract) {
			t.Fatalf("dashboard collector settings are missing contract %q", contract)
		}
	}
	if strings.Contains(body, `call(PLUGIN_CONFIG_PATH, "GET")`) {
		t.Fatal("dashboard must not fetch and expose the saved proxy URL")
	}
}

func TestDashboardShowsRedactedCollectorProcessLog(t *testing.T) {
	body := string(dashboardHTML)
	for _, contract := range []string{
		`id="collectorLog"`,
		`最近采集日志`,
		`collector_events`,
		`collector_log`,
		`renderCollectorLog`,
		`event.auth_id`,
		`event.status_code`,
		`event.retry_in_seconds`,
		`日志文件持续追加，不自动轮转或删除`,
		`id="btnCollectorLogMore"`,
		`log_before`,
	} {
		if !strings.Contains(body, contract) {
			t.Fatalf("dashboard collector log is missing contract %q", contract)
		}
	}
}

func TestDashboardUsesAccessibleIconToolbar(t *testing.T) {
	body := string(dashboardHTML)
	for _, contract := range []string{
		`class="icon-btn" id="btnExpandAll" aria-label="全部展开"`,
		`class="icon-btn" id="btnCollapseAll" aria-label="全部折叠"`,
		`class="icon-btn" id="btnCollectorSettings" aria-label="采集设置"`,
		`class="icon-btn" id="btnRefresh" aria-label="刷新"`,
		`class="icon-btn danger" id="btnClear" aria-label="清空缓存"`,
		`.toolbar-actions`,
		`.icon-btn svg`,
	} {
		if !strings.Contains(body, contract) {
			t.Fatalf("dashboard icon toolbar is missing contract %q", contract)
		}
	}
}

func configureTestPlugin(t *testing.T, extraYAML string) {
	t.Helper()
	t.Cleanup(resetRuntime)
	configYAML := "max_entries: 8\nmax_pending_entries: 8\n"
	if extraYAML != "" {
		configYAML = configYAML + "\n" + extraYAML
	}
	callPluginMethod(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
		ConfigYAML:    []byte(configYAML),
	}, &registration{})
}

func afterAuthRequest(requestID, authID, model string) pluginapi.RequestInterceptRequest {
	return afterAuthRequestHeaders(requestID, authID, model, nil)
}

func validTemplate(issued time.Time) string {
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	for index := 9; index < len(raw); index++ {
		raw[index] = byte(index)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func afterAuthRequestHeaders(requestID, authID, model string, headers http.Header) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		RequestID: requestID,
		ToFormat:  "codex",
		Model:     model,
		Headers:   headers,
		Metadata:  map[string]any{"selected_auth_id": authID},
	}
}

func callManagement(t *testing.T, method, path string, body any, target any) {
	t.Helper()
	response := callManagementRaw(t, method, path, body)
	if response.StatusCode >= 400 {
		t.Fatalf("%s %s status = %d body = %s", method, path, response.StatusCode, response.Body)
	}
	if err := json.Unmarshal(response.Body, target); err != nil {
		t.Fatalf("decode %s %s body %q: %v", method, path, response.Body, err)
	}
}

func callManagementRaw(t *testing.T, method, path string, body any) pluginapi.ManagementResponse {
	t.Helper()
	var rawBody []byte
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, errMarshal)
		}
		rawBody = raw
	}
	// ManagementResponse has no JSON tags, so its Body arrives base64-encoded
	// inside the RPC envelope. Decode the wrapper, then parse the actual JSON
	// body the route produced.
	var response struct {
		StatusCode int         `json:"StatusCode"`
		Headers    http.Header `json:"Headers"`
		Body       []byte      `json:"Body"`
	}
	callPluginMethod(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: method,
		Path:   path,
		Body:   rawBody,
	}, &response)
	return pluginapi.ManagementResponse{StatusCode: response.StatusCode, Headers: response.Headers, Body: response.Body}
}

// callManagementErr is callManagement for routes that are expected to answer
// with a 4xx/5xx error body.
func callManagementErr(t *testing.T, method, path string, body any) string {
	t.Helper()
	rawBody, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatalf("marshal %s %s body: %v", method, path, errMarshal)
	}
	var response struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	callPluginMethod(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: method,
		Path:   path,
		Body:   rawBody,
	}, &response)
	if response.StatusCode < 400 {
		t.Fatalf("%s %s should have failed, got status %d body %s", method, path, response.StatusCode, response.Body)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &errResp); err != nil || errResp.Error == "" {
		t.Fatalf("%s %s error body = %s", method, path, response.Body)
	}
	return errResp.Error
}

func countPaths(routes []pluginapi.ResourceRoute, path string) int {
	count := 0
	for _, route := range routes {
		if route.Path == path {
			count++
		}
	}
	return count
}

func callPluginMethod(t *testing.T, method string, request, target any) {
	t.Helper()
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	rawResponse, err := handleMethod(method, rawRequest)
	if err != nil {
		t.Fatalf("handle %s: %v", method, err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		t.Fatalf("decode %s envelope: %v", method, err)
	}
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("%s envelope = %s", method, rawResponse)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatalf("decode %s result: %v", method, err)
	}
}
