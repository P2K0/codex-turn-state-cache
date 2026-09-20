//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
)

func TestCollectorProbeUsesDedicatedProxy(t *testing.T) {
	template := validTemplate(time.Now().Add(-time.Minute))
	requestSeen := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.String() != "http://127.0.0.1:44123/responses" {
			t.Errorf("proxied URL = %q", request.URL.String())
		}
		if got := request.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Chatgpt-Account-Id"); got != "account-123" {
			t.Errorf("Chatgpt-Account-Id = %q", got)
		}
		if got := request.Header.Get(turnstate.TurnStateHeader); got != "" {
			t.Errorf("probe sent a stale turn state: %q", got)
		}
		body, errRead := io.ReadAll(request.Body)
		if errRead != nil {
			t.Errorf("read body: %v", errRead)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if payload["model"] != "gpt-test" || payload["stream"] != true || payload["store"] != false {
			t.Errorf("payload = %#v", payload)
		}
		response.Header().Set(turnstate.TurnStateHeader, template)
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, "event: response.completed\ndata: {}\n\n")
		requestSeen <- struct{}{}
	}))
	defer proxy.Close()

	probe, err := newCollectorProbe(collectorProbeOptions{
		ProxyURL: proxy.URL,
		Endpoint: "http://127.0.0.1:44123/responses",
		LoadCredential: func(context.Context, string) (collectorCredential, error) {
			return collectorCredential{AccessToken: "access-token", AccountID: "account-123"}, nil
		},
	})
	if err != nil {
		t.Fatalf("new collector probe: %v", err)
	}
	result := probe.Probe(context.Background(), turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-test"})
	if result.Err != nil || !result.Reached || !result.Harvested || result.State != template {
		t.Fatalf("probe result = %#v", result)
	}
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("dedicated proxy did not receive the collector request")
	}
}

func TestCollectorProbeRequiresSuccessfulCompletedResponse(t *testing.T) {
	template := validTemplate(time.Now().Add(-time.Minute))
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{name: "non-2xx", statusCode: http.StatusUnauthorized, body: "event: response.completed\ndata: {}\n\n"},
		{name: "missing completion", statusCode: http.StatusOK, body: "event: response.output_text.delta\ndata: {}\n\n"},
		{name: "completed", statusCode: http.StatusOK, body: "event: response.completed\ndata: {}\n\n", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set(turnstate.TurnStateHeader, template)
				response.WriteHeader(test.statusCode)
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()

			probe := newTestCollectorProbe(t, server.URL, server.URL+"/responses")
			result := probe.Probe(context.Background(), turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-test"})
			if result.Harvested != test.want {
				t.Fatalf("probe result = %#v, want harvested=%t", result, test.want)
			}
		})
	}
}

func TestCollectorProbeRejectsOversizedResponseBody(t *testing.T) {
	template := validTemplate(time.Now().Add(-time.Minute))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set(turnstate.TurnStateHeader, template)
		response.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(response, strings.Repeat("x", 1<<20)+"\nevent: response.completed\n\n")
	}))
	defer server.Close()

	result := newTestCollectorProbe(t, server.URL, server.URL+"/responses").Probe(
		context.Background(), turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-test"},
	)
	if result.Harvested || result.Err == nil {
		t.Fatalf("oversized response result = %#v", result)
	}
}

func TestCollectorProbeParsesRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		header  func() string
		wantMin time.Duration
		wantMax time.Duration
	}{
		{name: "seconds", header: func() string { return "37" }, wantMin: 37 * time.Second, wantMax: 37 * time.Second},
		{
			name:    "http date",
			header:  func() string { return time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat) },
			wantMin: 28 * time.Second,
			wantMax: 30 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Retry-After", test.header())
				response.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()

			result := newTestCollectorProbe(t, server.URL, server.URL+"/responses").Probe(
				context.Background(), turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-test"},
			)
			got := result.RetryAfter
			if got < test.wantMin || got > test.wantMax {
				t.Fatalf("RetryAfter = %s, want within [%s, %s]", got, test.wantMin, test.wantMax)
			}
		})
	}
}

func TestCollectorProbeRejectsInsecureBearerEndpoints(t *testing.T) {
	proxyRequests := make(chan *http.Request, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		proxyRequests <- request
		response.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	tests := []struct {
		name       string
		endpoint   string
		credential collectorCredential
	}{
		{
			name:       "configured endpoint",
			endpoint:   "http://collector-upstream.invalid/responses",
			credential: collectorCredential{AccessToken: "secret-token"},
		},
		{
			name:       "credential base URL",
			credential: collectorCredential{AccessToken: "secret-token", BaseURL: "http://collector-upstream.invalid/backend-api/codex"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe, err := newCollectorProbe(collectorProbeOptions{
				ProxyURL: proxy.URL,
				Endpoint: test.endpoint,
				LoadCredential: func(context.Context, string) (collectorCredential, error) {
					return test.credential, nil
				},
			})
			if err != nil {
				return
			}
			result := probe.Probe(context.Background(), turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-test"})
			if result.Err == nil {
				t.Fatalf("insecure probe result = %#v", result)
			}
		})
	}
	select {
	case request := <-proxyRequests:
		t.Fatalf("sent bearer request to insecure endpoint %q", request.URL)
	default:
	}
}

func TestCollectorScheduleBlocksUnauthorizedUntilObservedAgain(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	schedule := newCollectorSchedule(8, time.Minute, time.Minute)
	schedule.Observe(key, now)
	if due := schedule.Due(now, nil); len(due) != 1 {
		t.Fatalf("initial due = %#v", due)
	}
	schedule.CompleteResult(key, now, collectorProbeResult{StatusCode: http.StatusUnauthorized})
	if due := schedule.Due(now.Add(24*time.Hour), nil); len(due) != 0 {
		t.Fatalf("blocked target became due = %#v", due)
	}
	schedule.Observe(key, now.Add(24*time.Hour))
	if due := schedule.Due(now.Add(24*time.Hour), nil); len(due) != 1 {
		t.Fatalf("re-observed target due = %#v", due)
	}
}

func TestCollectorScheduleHonorsRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	schedule := newCollectorSchedule(8, time.Minute, time.Minute)
	schedule.Observe(key, now)
	_ = schedule.Due(now, nil)
	schedule.CompleteResult(key, now, collectorProbeResult{
		StatusCode: http.StatusTooManyRequests,
		RetryAfter: 5 * time.Minute,
	})
	if due := schedule.Due(now.Add(4*time.Minute+59*time.Second), nil); len(due) != 0 {
		t.Fatalf("429 target retried before Retry-After = %#v", due)
	}
	if due := schedule.Due(now.Add(5*time.Minute), nil); len(due) != 1 {
		t.Fatalf("429 target not retried after Retry-After = %#v", due)
	}
}

func TestCollectorScheduleBoundsNon429RetryState(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	schedule := newCollectorSchedule(8, time.Minute, time.Minute)
	schedule.Observe(key, now)
	_ = schedule.Due(now, nil)
	schedule.CompleteResult(key, now, collectorProbeResult{
		StatusCode: http.StatusBadGateway,
		RetryAfter: 24 * time.Hour,
	})
	if due := schedule.Due(now.Add(15*time.Minute+time.Second), nil); len(due) != 1 {
		t.Fatalf("non-429 retry state was not bounded = %#v", due)
	}
}

func TestDialCollectorSOCKS5CancelsStalledGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	closed := make(chan struct{})
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		accepted <- connection
		defer connection.Close()
		_, _ = io.ReadFull(connection, make([]byte, 3))
		_, _ = connection.Read(make([]byte, 1))
		close(closed)
	}()
	t.Cleanup(func() {
		select {
		case connection := <-accepted:
			_ = connection.Close()
		default:
		}
	})

	proxyURL, err := url.Parse("socks5://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		connection, errDial := dialCollectorSOCKS5(ctx, proxyURL, "tcp", "example.invalid:443")
		if connection != nil {
			_ = connection.Close()
		}
		result <- errDial
	}()
	select {
	case errDial := <-result:
		if errDial == nil {
			t.Fatal("stalled SOCKS5 greeting unexpectedly succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SOCKS5 greeting ignored context cancellation")
	}
	select {
	case <-closed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancelled SOCKS5 handshake did not close the socket")
	}
}

func newTestCollectorProbe(t *testing.T, proxyURL, endpoint string) *collectorProbe {
	t.Helper()
	probe, err := newCollectorProbe(collectorProbeOptions{
		ProxyURL: proxyURL,
		Endpoint: endpoint,
		LoadCredential: func(context.Context, string) (collectorCredential, error) {
			return collectorCredential{AccessToken: "access-token"}, nil
		},
	})
	if err != nil {
		t.Fatalf("new collector probe: %v", err)
	}
	return probe
}

func TestCollectorCredentialFromAuthJSON(t *testing.T) {
	credential, err := collectorCredentialFromAuthJSON([]byte(`{
		"access_token":"token-a",
		"account_id":"account-a",
		"base_url":"https://example.invalid/backend-api/codex"
	}`))
	if err != nil {
		t.Fatalf("parse credential: %v", err)
	}
	if credential.AccessToken != "token-a" || credential.AccountID != "account-a" || credential.BaseURL != "https://example.invalid/backend-api/codex" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestCollectorScheduleRefreshesMissingAndExpiringTargets(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	missing := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	expiring := turnstate.CacheKey{AuthID: "auth-b", Model: "gpt-b"}
	healthy := turnstate.CacheKey{AuthID: "auth-c", Model: "gpt-c"}
	schedule := newCollectorSchedule(8, 5*time.Minute, time.Minute)
	for _, key := range []turnstate.CacheKey{missing, expiring, healthy} {
		schedule.Observe(key, now)
	}

	due := schedule.Due(now, []turnstate.BucketStatus{
		{AuthID: expiring.AuthID, Model: expiring.Model, Ready: true, ExpiresAt: now.Add(4 * time.Minute)},
		{AuthID: healthy.AuthID, Model: healthy.Model, Ready: true, ExpiresAt: now.Add(6 * time.Minute)},
	})
	if !containsCacheKey(due, missing) || !containsCacheKey(due, expiring) || containsCacheKey(due, healthy) {
		t.Fatalf("due targets = %#v", due)
	}

	schedule.Complete(missing, now)
	if retry := schedule.Due(now.Add(30*time.Second), nil); containsCacheKey(retry, missing) {
		t.Fatalf("missing target retried before backoff: %#v", retry)
	}
	if retry := schedule.Due(now.Add(time.Minute), nil); !containsCacheKey(retry, missing) {
		t.Fatalf("missing target was not retried after backoff: %#v", retry)
	}
}

func TestAutoCollectorStoresMissingObservedBucket(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	template := validTemplate(time.Now().Add(-time.Minute))
	var probeCalls int
	var storedKey turnstate.CacheKey
	var storedState string
	var events []collectorEvent
	collector := newAutoCollector(autoCollectorOptions{
		MaxTargets:    8,
		RefreshBefore: 5 * time.Minute,
		RetryAfter:    time.Minute,
		Now:           func() time.Time { return now },
		Snapshot:      func() []turnstate.BucketStatus { return nil },
		Probe: func(context.Context, turnstate.CacheKey) collectorProbeResult {
			probeCalls++
			return collectorProbeResult{Reached: true, Harvested: true, State: template, Length: len(template)}
		},
		Store: func(gotKey turnstate.CacheKey, state string) turnstate.StoreOutcome {
			storedKey, storedState = gotKey, state
			return turnstate.StoreTemplate
		},
		Event: func(_ context.Context, event collectorEvent) {
			events = append(events, event)
		},
	})
	collector.Observe(key)
	collector.RunOnce(context.Background())
	if probeCalls != 1 || storedKey != key || storedState != template {
		t.Fatalf("probeCalls=%d storedKey=%#v storedStateLen=%d", probeCalls, storedKey, len(storedState))
	}
	if !containsCollectorAction(events, collectorActionStarted) || !containsCollectorAction(events, collectorActionSucceeded) {
		t.Fatalf("collector events = %#v", events)
	}
}

func TestAutoCollectorRecordsFailureAndRetryWithoutSensitiveError(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "secret-account", Model: "gpt-a"}
	var events []collectorEvent
	collector := newAutoCollector(autoCollectorOptions{
		MaxTargets:    8,
		RefreshBefore: 5 * time.Minute,
		RetryAfter:    45 * time.Second,
		Now:           func() time.Time { return now },
		Probe: func(context.Context, turnstate.CacheKey) collectorProbeResult {
			return collectorProbeResult{Err: errors.New("proxy password secret-value")}
		},
		Store: func(turnstate.CacheKey, string) turnstate.StoreOutcome { return turnstate.StoreIgnored },
		Event: func(_ context.Context, event collectorEvent) {
			events = append(events, event)
		},
	})
	collector.Observe(key)
	collector.RunOnce(context.Background())

	var failed collectorEvent
	for _, event := range events {
		if event.Action == collectorActionFailed {
			failed = event
		}
	}
	if failed.Action == "" || failed.RetryInSeconds != 45 || failed.Level != "warn" {
		t.Fatalf("failed event = %#v; all events = %#v", failed, events)
	}
	if failed.AuthID != key.AuthID {
		t.Fatalf("failed event auth_id = %q, want %q", failed.AuthID, key.AuthID)
	}
	raw, errMarshal := json.Marshal(events)
	if errMarshal != nil {
		t.Fatalf("marshal events: %v", errMarshal)
	}
	if !strings.Contains(string(raw), "secret-account") || strings.Contains(string(raw), "secret-value") {
		t.Fatalf("collector events leaked sensitive values: %s", raw)
	}
}

func TestCollectorEventStoreAppendsWithoutTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector-events.jsonl")
	store := newCollectorEventStore(path)
	for _, action := range []string{"first", "second", "third"} {
		if err := store.Append(collectorEvent{Action: action}); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
	}

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read event log: %v", errRead)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 3 {
		t.Fatalf("persisted lines = %d, want 3; raw=%q", lines, raw)
	}
	events, errTail := store.Tail(2)
	if errTail != nil {
		t.Fatalf("tail event log: %v", errTail)
	}
	if len(events) != 2 || events[0].Action != "third" || events[1].Action != "second" {
		t.Fatalf("events = %#v", events)
	}
	page, errPage := store.Page(0, 2)
	if errPage != nil {
		t.Fatalf("first page: %v", errPage)
	}
	if len(page.Events) != 2 || page.NextBefore == 0 {
		t.Fatalf("first page = %#v", page)
	}
	older, errOlder := store.Page(page.NextBefore, 2)
	if errOlder != nil {
		t.Fatalf("older page: %v", errOlder)
	}
	if len(older.Events) != 1 || older.Events[0].Action != "first" || older.NextBefore != 0 {
		t.Fatalf("older page = %#v", older)
	}
}

func TestAutoCollectorSkipsHealthyBucket(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	key := turnstate.CacheKey{AuthID: "auth-a", Model: "gpt-a"}
	var probeCalls int
	collector := newAutoCollector(autoCollectorOptions{
		MaxTargets:    8,
		RefreshBefore: 5 * time.Minute,
		RetryAfter:    time.Minute,
		Now:           func() time.Time { return now },
		Snapshot: func() []turnstate.BucketStatus {
			return []turnstate.BucketStatus{{AuthID: key.AuthID, Model: key.Model, Ready: true, ExpiresAt: now.Add(6 * time.Minute)}}
		},
		Probe: func(context.Context, turnstate.CacheKey) collectorProbeResult {
			probeCalls++
			return collectorProbeResult{}
		},
		Store: func(turnstate.CacheKey, string) turnstate.StoreOutcome { return turnstate.StoreIgnored },
	})
	collector.Observe(key)
	collector.RunOnce(context.Background())
	if probeCalls != 0 {
		t.Fatalf("healthy bucket triggered %d collection calls", probeCalls)
	}
}

func containsCacheKey(keys []turnstate.CacheKey, want turnstate.CacheKey) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func containsCollectorAction(events []collectorEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}
