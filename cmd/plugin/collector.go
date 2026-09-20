package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/5345asda/codex-turn-state-cache/internal/turnstate"
)

const (
	defaultCollectorEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	collectorUserAgent       = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	collectorMaxResponseBody = 1 << 20
)

type collectorCredential struct {
	AccessToken string
	AccountID   string
	BaseURL     string
}

type collectorProbeOptions struct {
	ProxyURL       string
	Endpoint       string
	Timeout        time.Duration
	LoadCredential func(context.Context, string) (collectorCredential, error)
}

type collectorProbe struct {
	client         *http.Client
	endpoint       string
	loadCredential func(context.Context, string) (collectorCredential, error)
}

type collectorProbeResult struct {
	Reached    bool
	Harvested  bool
	StatusCode int
	Length     int
	State      string
	Note       string
	RetryAfter time.Duration
	Err        error
}

func validateCollectorEndpoint(raw string) error {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Host == "" {
		return fmt.Errorf("collector endpoint is missing a valid scheme and host")
	}
	switch strings.ToLower(endpoint.Scheme) {
	case "https":
		return nil
	case "http":
		host := strings.ToLower(endpoint.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
	}
	return fmt.Errorf("collector endpoint must use HTTPS (HTTP is permitted only for loopback tests)")
}

func validateCollectorProxyURL(raw string) error {
	proxyURL, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || proxyURL.Host == "" {
		return fmt.Errorf("collector proxy URL is missing a valid scheme and host")
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("collector_proxy_url must be a concrete http, https, socks5, or socks5h URL")
	}
	return nil
}

func newCollectorProbe(options collectorProbeOptions) (*collectorProbe, error) {
	if err := validateCollectorProxyURL(options.ProxyURL); err != nil {
		return nil, err
	}
	transport, err := buildCollectorTransport(options.ProxyURL)
	if err != nil {
		return nil, err
	}
	if options.LoadCredential == nil {
		return nil, fmt.Errorf("collector credential loader is required")
	}
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		endpoint = defaultCollectorEndpoint
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &collectorProbe{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(request *http.Request, _ []*http.Request) error {
				if err := validateCollectorEndpoint(request.URL.String()); err != nil {
					return err
				}
				return nil
			},
		},
		endpoint:       endpoint,
		loadCredential: options.LoadCredential,
	}, nil
}

func buildCollectorTransport(raw string) (*http.Transport, error) {
	if err := validateCollectorProxyURL(raw); err != nil {
		return nil, err
	}
	proxyURL, _ := url.Parse(strings.TrimSpace(raw))
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		transport = &http.Transport{}
	} else {
		transport = transport.Clone()
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5", "socks5h":
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialCollectorSOCKS5(ctx, proxyURL, network, address)
		}
	}
	return transport, nil
}

func dialCollectorSOCKS5(ctx context.Context, proxyURL *url.URL, network, address string) (net.Conn, error) {
	proxyAddress := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddress = net.JoinHostPort(proxyURL.Hostname(), "1080")
	}
	connection, errDial := (&net.Dialer{}).DialContext(ctx, network, proxyAddress)
	if errDial != nil {
		return nil, fmt.Errorf("dial collector SOCKS5 proxy: %w", errDial)
	}
	stopCancellation := make(chan struct{})
	defer close(stopCancellation)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopCancellation:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	fail := func(err error) (net.Conn, error) {
		_ = connection.Close()
		return nil, err
	}
	methods := []byte{0x00}
	if proxyURL.User != nil {
		methods = append(methods, 0x02)
	}
	if _, errWrite := connection.Write(append([]byte{0x05, byte(len(methods))}, methods...)); errWrite != nil {
		return fail(fmt.Errorf("write SOCKS5 greeting: %w", errWrite))
	}
	reply := make([]byte, 2)
	if _, errRead := io.ReadFull(connection, reply); errRead != nil {
		return fail(fmt.Errorf("read SOCKS5 greeting: %w", errRead))
	}
	if reply[0] != 0x05 || reply[1] == 0xff {
		return fail(fmt.Errorf("SOCKS5 proxy rejected authentication methods"))
	}
	if reply[1] == 0x02 {
		if proxyURL.User == nil {
			return fail(fmt.Errorf("SOCKS5 proxy requires credentials"))
		}
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		if len(username) > 255 || len(password) > 255 {
			return fail(fmt.Errorf("SOCKS5 credentials are too long"))
		}
		authRequest := []byte{0x01, byte(len(username))}
		authRequest = append(authRequest, username...)
		authRequest = append(authRequest, byte(len(password)))
		authRequest = append(authRequest, password...)
		if _, errWrite := connection.Write(authRequest); errWrite != nil {
			return fail(fmt.Errorf("write SOCKS5 authentication: %w", errWrite))
		}
		if _, errRead := io.ReadFull(connection, reply); errRead != nil || reply[1] != 0x00 {
			return fail(fmt.Errorf("SOCKS5 authentication failed"))
		}
	} else if reply[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 proxy selected unsupported authentication method"))
	}
	host, portText, errSplit := net.SplitHostPort(address)
	if errSplit != nil {
		return fail(fmt.Errorf("split SOCKS5 target: %w", errSplit))
	}
	port, errPort := strconv.ParseUint(portText, 10, 16)
	if errPort != nil {
		return fail(fmt.Errorf("parse SOCKS5 target port: %w", errPort))
	}
	connectRequest := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			connectRequest = append(connectRequest, 0x01)
			connectRequest = append(connectRequest, ipv4...)
		} else {
			connectRequest = append(connectRequest, 0x04)
			connectRequest = append(connectRequest, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fail(fmt.Errorf("SOCKS5 target hostname is too long"))
		}
		connectRequest = append(connectRequest, 0x03, byte(len(host)))
		connectRequest = append(connectRequest, host...)
	}
	connectRequest = append(connectRequest, byte(port>>8), byte(port))
	if _, errWrite := connection.Write(connectRequest); errWrite != nil {
		return fail(fmt.Errorf("write SOCKS5 connect request: %w", errWrite))
	}
	header := make([]byte, 4)
	if _, errRead := io.ReadFull(connection, header); errRead != nil {
		return fail(fmt.Errorf("read SOCKS5 connect response: %w", errRead))
	}
	if header[0] != 0x05 || header[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 connect failed with code %d", header[1]))
	}
	addressLength := 0
	switch header[3] {
	case 0x01:
		addressLength = 4
	case 0x04:
		addressLength = 16
	case 0x03:
		length := []byte{0}
		if _, errRead := io.ReadFull(connection, length); errRead != nil {
			return fail(fmt.Errorf("read SOCKS5 response hostname length: %w", errRead))
		}
		addressLength = int(length[0])
	default:
		return fail(fmt.Errorf("SOCKS5 response used an unknown address type"))
	}
	if _, errRead := io.ReadFull(connection, make([]byte, addressLength+2)); errRead != nil {
		return fail(fmt.Errorf("read SOCKS5 response address: %w", errRead))
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, nil
}

func (probe *collectorProbe) Probe(ctx context.Context, key turnstate.CacheKey) collectorProbeResult {
	credential, errCredential := probe.loadCredential(ctx, key.AuthID)
	if errCredential != nil {
		return collectorProbeResult{Err: errCredential}
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return collectorProbeResult{Err: fmt.Errorf("credential %q has no access_token", key.AuthID)}
	}
	payload := map[string]any{
		"model":        key.Model,
		"instructions": "Reply briefly.",
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": "ping"}},
		}},
		"store":  false,
		"stream": true,
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return collectorProbeResult{Err: errMarshal}
	}
	endpoint := probe.endpoint
	if baseURL := strings.TrimSpace(credential.BaseURL); baseURL != "" && endpoint == defaultCollectorEndpoint {
		if errEndpoint := validateCollectorEndpoint(baseURL); errEndpoint != nil {
			return collectorProbeResult{Err: errEndpoint}
		}
		endpoint = strings.TrimRight(baseURL, "/") + "/responses"
	}
	if errEndpoint := validateCollectorEndpoint(endpoint); errEndpoint != nil {
		return collectorProbeResult{Err: errEndpoint}
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return collectorProbeResult{Err: errRequest}
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Connection", "Keep-Alive")
	request.Header.Set("Originator", "codex-tui")
	request.Header.Set("User-Agent", collectorUserAgent)
	if accountID := strings.TrimSpace(credential.AccountID); accountID != "" {
		request.Header.Set("Chatgpt-Account-Id", accountID)
	}

	response, errDo := probe.client.Do(request)
	if errDo != nil {
		return collectorProbeResult{Err: errDo}
	}
	defer response.Body.Close()
	body, errRead := io.ReadAll(io.LimitReader(response.Body, collectorMaxResponseBody+1))
	if errRead != nil {
		return collectorProbeResult{Reached: true, StatusCode: response.StatusCode, RetryAfter: parseCollectorRetryAfter(response.Header.Get("Retry-After"), time.Now()), Err: fmt.Errorf("read collector response: %w", errRead)}
	}
	retryAfter := parseCollectorRetryAfter(response.Header.Get("Retry-After"), time.Now())
	value := headerValue(response.Header, turnstate.TurnStateHeader)
	result := collectorProbeResult{
		Reached:    true,
		StatusCode: response.StatusCode,
		Length:     len(value),
		State:      value,
		RetryAfter: retryAfter,
	}
	if len(body) > collectorMaxResponseBody {
		result.Err = fmt.Errorf("collector response body exceeds %d bytes", collectorMaxResponseBody)
		return result
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Note = "upstream response was not successful"
		return result
	}
	if !collectorResponseCompleted(body) {
		result.Note = "upstream response did not complete"
		return result
	}
	switch len(value) {
	case turnstate.StateLength:
		result.Harvested = true
	case turnstate.DegradedLength:
		result.Note = "upstream issued a degraded 312 state; nothing to harvest"
	case 0:
		result.Note = "upstream response did not include a turn state"
	default:
		result.Note = "unexpected state length"
	}
	return result
}

func collectorResponseCompleted(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "event: response.completed") {
			return true
		}
	}
	return false
}

func parseCollectorRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(raw)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func collectorCredentialFromAuthJSON(raw []byte) (collectorCredential, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return collectorCredential{}, fmt.Errorf("decode credential JSON: %w", err)
	}
	credential := collectorCredential{
		AccessToken: stringField(document, "access_token"),
		AccountID:   stringField(document, "account_id"),
		BaseURL:     stringField(document, "base_url"),
	}
	if tokens, ok := document["tokens"].(map[string]any); ok {
		if credential.AccessToken == "" {
			credential.AccessToken = stringField(tokens, "access_token")
		}
		if credential.AccountID == "" {
			credential.AccountID = stringField(tokens, "account_id")
		}
	}
	if credential.AccessToken == "" {
		return collectorCredential{}, fmt.Errorf("credential JSON has no access_token")
	}
	return credential, nil
}

func loadCollectorCredential(_ context.Context, authID string) (collectorCredential, error) {
	raw, errLoad := fetchAuthJSON(authID)
	if errLoad != nil {
		return collectorCredential{}, errLoad
	}
	return collectorCredentialFromAuthJSON(raw)
}

type collectorTargetState struct {
	observedAt  time.Time
	lastAttempt time.Time
	nextRetry   time.Time
	failures    int
	blocked     bool
	inFlight    bool
}

type collectorSchedule struct {
	mu            sync.Mutex
	maxTargets    int
	refreshBefore time.Duration
	retryAfter    time.Duration
	targets       map[turnstate.CacheKey]collectorTargetState
}

func newCollectorSchedule(maxTargets int, refreshBefore, retryAfter time.Duration) *collectorSchedule {
	if maxTargets < 1 {
		maxTargets = 1
	}
	return &collectorSchedule{
		maxTargets:    maxTargets,
		refreshBefore: refreshBefore,
		retryAfter:    retryAfter,
		targets:       make(map[turnstate.CacheKey]collectorTargetState),
	}
}

func (schedule *collectorSchedule) Observe(key turnstate.CacheKey, now time.Time) bool {
	if key.AuthID == "" || key.Model == "" {
		return false
	}
	schedule.mu.Lock()
	defer schedule.mu.Unlock()
	if state, exists := schedule.targets[key]; exists {
		state.observedAt = now
		state.blocked = false
		state.failures = 0
		state.nextRetry = time.Time{}
		schedule.targets[key] = state
		return false
	}
	for len(schedule.targets) >= schedule.maxTargets {
		var oldestKey turnstate.CacheKey
		var oldest time.Time
		first := true
		for candidate, state := range schedule.targets {
			if state.inFlight {
				continue
			}
			if first || state.observedAt.Before(oldest) {
				oldestKey, oldest, first = candidate, state.observedAt, false
			}
		}
		if first {
			return false
		}
		delete(schedule.targets, oldestKey)
	}
	schedule.targets[key] = collectorTargetState{observedAt: now}
	return true
}

func (schedule *collectorSchedule) Due(now time.Time, buckets []turnstate.BucketStatus) []turnstate.CacheKey {
	expires := make(map[turnstate.CacheKey]time.Time, len(buckets))
	for _, bucket := range buckets {
		if bucket.Ready {
			expires[turnstate.CacheKey{AuthID: bucket.AuthID, Model: bucket.Model}] = bucket.ExpiresAt
		}
	}
	schedule.mu.Lock()
	defer schedule.mu.Unlock()
	due := make([]turnstate.CacheKey, 0)
	for key, state := range schedule.targets {
		if state.inFlight || state.blocked || (!state.nextRetry.IsZero() && now.Before(state.nextRetry)) || (state.nextRetry.IsZero() && !state.lastAttempt.IsZero() && now.Before(state.lastAttempt.Add(schedule.retryAfter))) {
			continue
		}
		expiresAt, ready := expires[key]
		if ready && expiresAt.Sub(now) > schedule.refreshBefore {
			continue
		}
		state.inFlight = true
		schedule.targets[key] = state
		due = append(due, key)
	}
	return due
}

func (schedule *collectorSchedule) Complete(key turnstate.CacheKey, now time.Time) {
	schedule.complete(key, now, collectorProbeResult{})
}

func (schedule *collectorSchedule) CompleteResult(key turnstate.CacheKey, now time.Time, result collectorProbeResult) {
	schedule.complete(key, now, result)
}

func (schedule *collectorSchedule) complete(key turnstate.CacheKey, now time.Time, result collectorProbeResult) {
	schedule.mu.Lock()
	defer schedule.mu.Unlock()
	state, exists := schedule.targets[key]
	if !exists {
		return
	}
	state.inFlight = false
	state.lastAttempt = now
	if result.StatusCode == http.StatusUnauthorized || result.StatusCode == http.StatusForbidden {
		state.blocked = true
		state.nextRetry = time.Time{}
	} else if result.Harvested {
		state.failures = 0
		state.nextRetry = time.Time{}
	} else {
		state.failures++
		if state.failures > 5 {
			state.failures = 5
		}
		backoff := schedule.retryAfter
		for attempt := 1; attempt < state.failures; attempt++ {
			if backoff >= 15*time.Minute {
				break
			}
			backoff *= 2
		}
		if backoff > 15*time.Minute {
			backoff = 15 * time.Minute
		}
		if result.StatusCode == http.StatusTooManyRequests && result.RetryAfter > backoff {
			backoff = result.RetryAfter
		}
		state.nextRetry = now.Add(backoff)
	}
	schedule.targets[key] = state
}

func (schedule *collectorSchedule) Size() int {
	schedule.mu.Lock()
	defer schedule.mu.Unlock()
	return len(schedule.targets)
}

type autoCollectorOptions struct {
	MaxTargets    int
	RefreshBefore time.Duration
	RetryAfter    time.Duration
	PollInterval  time.Duration
	Concurrency   int
	Now           func() time.Time
	Snapshot      func() []turnstate.BucketStatus
	Probe         func(context.Context, turnstate.CacheKey) collectorProbeResult
	Store         func(turnstate.CacheKey, string) turnstate.StoreOutcome
	Event         func(context.Context, collectorEvent)
}

type autoCollector struct {
	schedule     *collectorSchedule
	pollInterval time.Duration
	concurrency  int
	now          func() time.Time
	snapshot     func() []turnstate.BucketStatus
	probe        func(context.Context, turnstate.CacheKey) collectorProbeResult
	store        func(turnstate.CacheKey, string) turnstate.StoreOutcome
	event        func(context.Context, collectorEvent)
	wake         chan struct{}
	runMu        sync.Mutex
	lifecycleMu  sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
}

type autoCollectorStatus struct {
	Enabled              bool `json:"enabled"`
	ProxyConfigured      bool `json:"proxy_configured"`
	Targets              int  `json:"targets"`
	RefreshBeforeSeconds int  `json:"refresh_before_seconds"`
	RetrySeconds         int  `json:"retry_seconds"`
}

func newAutoCollector(options autoCollectorOptions) *autoCollector {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Snapshot == nil {
		options.Snapshot = func() []turnstate.BucketStatus { return nil }
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 15 * time.Second
	}
	if options.Concurrency < 1 {
		options.Concurrency = 4
	}
	return &autoCollector{
		schedule:     newCollectorSchedule(options.MaxTargets, options.RefreshBefore, options.RetryAfter),
		pollInterval: options.PollInterval,
		concurrency:  options.Concurrency,
		now:          options.Now,
		snapshot:     options.Snapshot,
		probe:        options.Probe,
		store:        options.Store,
		event:        options.Event,
		wake:         make(chan struct{}, 1),
	}
}

func (collector *autoCollector) Start() {
	collector.lifecycleMu.Lock()
	defer collector.lifecycleMu.Unlock()
	if collector.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	collector.cancel = cancel
	collector.done = make(chan struct{})
	go collector.loop(ctx, collector.done)
}

func (collector *autoCollector) Stop() {
	collector.lifecycleMu.Lock()
	cancel, done := collector.cancel, collector.done
	collector.cancel, collector.done = nil, nil
	collector.lifecycleMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (collector *autoCollector) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(collector.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-collector.wake:
			collector.RunOnce(ctx)
		case <-ticker.C:
			collector.RunOnce(ctx)
		}
	}
}

func (collector *autoCollector) Observe(key turnstate.CacheKey) {
	if collector.schedule.Observe(key, collector.now()) {
		collector.emit(context.Background(), collectorEvent{
			Action:  collectorActionDiscovered,
			Source:  "traffic",
			AuthID:  key.AuthID,
			Model:   key.Model,
			Message: "已发现新的账号模型采集目标",
		})
	}
	select {
	case collector.wake <- struct{}{}:
	default:
	}
}

func (collector *autoCollector) RunOnce(ctx context.Context) {
	collector.runMu.Lock()
	defer collector.runMu.Unlock()
	if collector.probe == nil || collector.store == nil {
		return
	}
	due := collector.schedule.Due(collector.now(), collector.snapshot())
	semaphore := make(chan struct{}, collector.concurrency)
	var wait sync.WaitGroup
	for _, key := range due {
		if ctx.Err() != nil {
			collector.schedule.Complete(key, collector.now())
			continue
		}
		semaphore <- struct{}{}
		wait.Add(1)
		go func(target turnstate.CacheKey) {
			defer wait.Done()
			defer func() { <-semaphore }()
			collector.emit(ctx, collectorEvent{
				Action:  collectorActionStarted,
				Source:  "automatic",
				AuthID:  target.AuthID,
				Model:   target.Model,
				Message: "开始自动采集",
			})
			result := collector.collect(ctx, target)
			collector.schedule.CompleteResult(target, collector.now(), result)
			collector.emit(ctx, collectorResultEvent(result, "automatic", target, collector.schedule.retryAfter))
		}(key)
	}
	wait.Wait()
}

func (collector *autoCollector) CollectNow(ctx context.Context, key turnstate.CacheKey) collectorProbeResult {
	collector.runMu.Lock()
	defer collector.runMu.Unlock()
	collector.schedule.Observe(key, collector.now())
	collector.emit(ctx, collectorEvent{
		Action:  collectorActionStarted,
		Source:  "manual",
		AuthID:  key.AuthID,
		Model:   key.Model,
		Message: "开始手动探测采集",
	})
	result := collector.collect(ctx, key)
	collector.schedule.CompleteResult(key, collector.now(), result)
	collector.emit(ctx, collectorResultEvent(result, "manual", key, 0))
	return result
}

func (collector *autoCollector) emit(ctx context.Context, event collectorEvent) {
	if collector.event == nil {
		return
	}
	event.At = collector.now().UTC().Format(time.RFC3339Nano)
	collector.event(ctx, event)
}

func (collector *autoCollector) collect(ctx context.Context, key turnstate.CacheKey) collectorProbeResult {
	result := collector.probe(ctx, key)
	if !result.Harvested {
		return result
	}
	if outcome := collector.store(key, result.State); outcome == turnstate.StoreIgnored {
		result.Harvested = false
		result.Note = "collected turn state was rejected by cache validation"
	}
	return result
}

func (collector *autoCollector) Status(refreshBefore, retryAfter time.Duration) autoCollectorStatus {
	return autoCollectorStatus{
		Enabled:              true,
		ProxyConfigured:      true,
		Targets:              collector.schedule.Size(),
		RefreshBeforeSeconds: int(refreshBefore.Seconds()),
		RetrySeconds:         int(retryAfter.Seconds()),
	}
}
