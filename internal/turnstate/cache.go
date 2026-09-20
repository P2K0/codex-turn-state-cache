package turnstate

import (
	"sort"
	"sync"
	"time"
)

const (
	TurnStateHeader = "X-Codex-Turn-State"
	StateLength     = 292
	DegradedLength  = 312

	stateTTL   = time.Hour
	pendingTTL = 2 * time.Hour
)

// CacheKey scopes state to the exact CPA-selected credential and model.
type CacheKey struct {
	AuthID string
	Model  string
}

// Counters tally what the plugin did, for the management status page. Lengths
// and outcomes only -- never a value.
type Counters struct {
	Captured         int64 `json:"captured"`
	CapturedReplaced int64 `json:"captured_replaced"`
	Injected         int64 `json:"injected"`
	Substituted      int64 `json:"substituted"`
	DegradedObserved int64 `json:"degraded_observed"`
}

// BucketStatus is one value-free (account, model) cell of the readiness matrix.
type BucketStatus struct {
	AuthID    string    `json:"auth_id"`
	Model     string    `json:"model"`
	Ready     bool      `json:"ready"`
	Len       int       `json:"len"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Snapshot struct {
	Counters Counters       `json:"counters"`
	Buckets  []BucketStatus `json:"buckets"`
}

// StoreOutcome classifies what a response header did to the cache.
type StoreOutcome int

const (
	StoreIgnored StoreOutcome = iota
	StoreTemplate
	StoreReplaced
	StoreDegraded
	StoreStale
)

type stateEntry struct {
	value    string
	issuedAt time.Time
	// expiresAt is issuedAt + stateTTL. issuedAt comes from the token's own
	// embedded Fernet timestamp when decodable, so a template harvested late
	// in its life is not mistaken for a fresh one.
	expiresAt time.Time
}

type pendingBinding struct {
	key       CacheKey
	createdAt time.Time
	expiresAt time.Time
}

type Cache struct {
	mu                sync.Mutex
	now               func() time.Time
	maxEntries        int
	maxPendingEntries int
	entries           map[CacheKey]stateEntry
	pending           map[string]pendingBinding
	counters          Counters
}

func NewCache(maxEntries, maxPendingEntries int, now func() time.Time) *Cache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxPendingEntries < 1 {
		maxPendingEntries = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{
		now:               now,
		maxEntries:        maxEntries,
		maxPendingEntries: maxPendingEntries,
		entries:           make(map[CacheKey]stateEntry),
		pending:           make(map[string]pendingBinding),
	}
}

// BindRequest associates a request ID with the trusted after-auth key.
func (c *Cache) BindRequest(requestID string, key CacheKey) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	if !validKey(key) {
		delete(c.pending, requestID)
		return
	}
	if _, exists := c.pending[requestID]; !exists {
		c.evictPendingLocked()
	}
	c.pending[requestID] = pendingBinding{
		key:       key,
		createdAt: now,
		expiresAt: now.Add(pendingTTL),
	}
}

func (c *Cache) ForgetRequest(requestID string) {
	if requestID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, requestID)
}

// Lookup returns a state only while its token-timestamp lifetime is valid.
func (c *Cache) Lookup(key CacheKey) (string, bool) {
	if !validKey(key) {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	return entry.value, true
}

// StoreResponseForRequest writes a valid template for the request's after-auth
// key. A degraded-length value is counted and logged by the caller but never
// stored.
func (c *Cache) StoreResponseForRequest(requestID string, values []string) (key CacheKey, outcome StoreOutcome) {
	if requestID == "" {
		return CacheKey{}, StoreIgnored
	}
	value, ok := validStateValue(values)
	if !ok {
		return CacheKey{}, StoreIgnored
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	binding, bound := c.pending[requestID]
	if !bound {
		return CacheKey{}, StoreIgnored
	}
	if len(value) == DegradedLength {
		c.counters.DegradedObserved++
		return binding.key, StoreDegraded
	}
	return binding.key, c.storeLocked(binding.key, value, now)
}

func (c *Cache) storeLocked(key CacheKey, value string, now time.Time) StoreOutcome {
	issuedAt, _, ok := ParseFernet(value)
	if !ok {
		return StoreIgnored
	}
	// The Fernet timestamp is the upstream's own signing moment, so it
	// necessarily precedes the moment we observe the token. A value in the
	// future means the clock or the token is wrong, and neither is worth
	// trusting: storing it would inject a template that cannot possibly be
	// valid while the decision log shows a clean capture.
	if issuedAt.After(now) {
		return StoreIgnored
	}
	expiresAt := issuedAt.Add(stateTTL)
	if now.After(expiresAt.Add(-30 * time.Second)) {
		return StoreStale
	}
	if current, exists := c.entries[key]; exists {
		if issuedAt.Before(current.issuedAt) {
			return StoreStale
		}
		if issuedAt.Equal(current.issuedAt) && value == current.value {
			return StoreIgnored
		}
	}
	outcome := StoreTemplate
	if _, replaced := c.entries[key]; replaced {
		outcome = StoreReplaced
		c.counters.CapturedReplaced++
	} else {
		c.evictEntriesLocked()
	}
	c.entries[key] = stateEntry{
		value:     value,
		issuedAt:  issuedAt,
		expiresAt: expiresAt,
	}
	c.counters.Captured++
	return outcome
}

func (c *Cache) CountInjected() {
	c.mu.Lock()
	c.counters.Injected++
	c.mu.Unlock()
}

func (c *Cache) CountSubstituted() {
	c.mu.Lock()
	c.counters.Substituted++
	c.mu.Unlock()
}

// Snapshot returns a value-free point-in-time view for status and scheduling.
func (c *Cache) Snapshot() Snapshot {
	c.mu.Lock()
	now := c.now()
	c.purgeExpiredLocked(now)
	snapshot := Snapshot{Counters: c.counters}
	for key, entry := range c.entries {
		ready := now.Before(entry.expiresAt)
		snapshot.Buckets = append(snapshot.Buckets, BucketStatus{
			AuthID:    key.AuthID,
			Model:     key.Model,
			Ready:     ready,
			Len:       len(entry.value),
			IssuedAt:  entry.issuedAt,
			ExpiresAt: entry.expiresAt,
		})
	}
	c.mu.Unlock()
	sortBuckets(snapshot.Buckets)
	return snapshot
}

// Reveal returns a live value for one exact bucket. It is intentionally
// separate from Snapshot so routine status and scheduling cannot copy secrets.
func (c *Cache) Reveal(key CacheKey) (string, bool) {
	return c.Lookup(key)
}

// Delete drops one bucket's cached template. It reports whether a template
// existed.
func (c *Cache) Delete(key CacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		return false
	}
	delete(c.entries, key)
	return true
}

// StoreForBucket stores a template harvested outside the request flow (the
// management probe), which has no request binding. It applies the same rules
// as the passive capture path: template length only, Fernet-based expiry, and
// a future timestamp rejected outright.
func (c *Cache) StoreForBucket(key CacheKey, value string) StoreOutcome {
	if !validKey(key) || len(value) != StateLength {
		return StoreIgnored
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.purgeExpiredLocked(now)
	return c.storeLocked(key, value, now)
}

// Clear drops every cached template and resets the counters, mirroring what
// reconfigure and quiesce already do to the cache itself.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[CacheKey]stateEntry)
	c.pending = make(map[string]pendingBinding)
	c.counters = Counters{}
}

func (c *Cache) purgeExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for requestID, binding := range c.pending {
		if !now.Before(binding.expiresAt) {
			delete(c.pending, requestID)
		}
	}
}

func (c *Cache) evictEntriesLocked() {
	for len(c.entries) >= c.maxEntries {
		var oldestKey CacheKey
		var oldest stateEntry
		first := true
		for key, entry := range c.entries {
			if first || entry.issuedAt.Before(oldest.issuedAt) || (entry.issuedAt.Equal(oldest.issuedAt) && cacheKeyLess(key, oldestKey)) {
				oldestKey = key
				oldest = entry
				first = false
			}
		}
		delete(c.entries, oldestKey)
	}
}

func (c *Cache) evictPendingLocked() {
	for len(c.pending) >= c.maxPendingEntries {
		var oldestRequestID string
		var oldest pendingBinding
		first := true
		for requestID, binding := range c.pending {
			if first || binding.createdAt.Before(oldest.createdAt) || (binding.createdAt.Equal(oldest.createdAt) && requestID < oldestRequestID) {
				oldestRequestID = requestID
				oldest = binding
				first = false
			}
		}
		delete(c.pending, oldestRequestID)
	}
}

func sortBuckets(buckets []BucketStatus) {
	sort.Slice(buckets, func(i, j int) bool { return bucketLess(buckets[i], buckets[j]) })
}

func bucketLess(left, right BucketStatus) bool {
	if left.AuthID != right.AuthID {
		return left.AuthID < right.AuthID
	}
	return left.Model < right.Model
}

func validKey(key CacheKey) bool {
	return key.AuthID != "" && key.Model != ""
}

// validStateValue accepts exactly one value of template or degraded length.
func validStateValue(values []string) (string, bool) {
	if len(values) != 1 {
		return "", false
	}
	if len(values[0]) != StateLength && len(values[0]) != DegradedLength {
		return "", false
	}
	return values[0], true
}

func cacheKeyLess(left, right CacheKey) bool {
	if left.AuthID != right.AuthID {
		return left.AuthID < right.AuthID
	}
	return left.Model < right.Model
}
