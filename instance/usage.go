package instance

import (
	"sync"
	"time"

	"copilot-go/store"
)

const usageWindowDuration = 1 * time.Hour

// UsageRecord holds a single timestamped request event.
type usageRecord struct {
	At     time.Time
	Failed bool
	Is429  bool
}

// AccountUsage tracks per-account request statistics within a sliding window.
type AccountUsage struct {
	mu      sync.Mutex
	records []usageRecord
	last429 time.Time
}

// AccountUsageSnapshot is a point-in-time view of an account's usage stats.
type AccountUsageSnapshot struct {
	TotalRequests  int64  `json:"totalRequests"`
	FailedRequests int64  `json:"failedRequests"`
	Last429At      string `json:"last429At,omitempty"` // RFC3339 or empty
	WindowSeconds  int    `json:"windowSeconds"`
}

var (
	usageMap   = make(map[string]*AccountUsage)
	usageMapMu sync.RWMutex
)

// LoadUsage restores persisted usage stats from disk into usageMap.
// Called once at startup from main.go.
func LoadUsage() {
	persisted := store.LoadUsageStats()
	now := time.Now()

	usageMapMu.Lock()
	defer usageMapMu.Unlock()

	for accountID, accData := range persisted {
		au := &AccountUsage{}
		for _, rec := range accData.Records {
			t, err := time.Parse(time.RFC3339, rec.At)
			if err != nil {
				continue
			}
			au.records = append(au.records, usageRecord{At: t, Failed: rec.Failed, Is429: rec.Is429})
		}
		// Trim stale records so GetUsageSnapshot returns correct counts on startup.
		au.mu.Lock()
		au.trimLocked(now)
		au.mu.Unlock()

		if accData.Last429 != "" && accData.Last429 != "0001-01-01T00:00:00Z" {
			if t, err := time.Parse(time.RFC3339, accData.Last429); err == nil {
				au.last429 = t
			}
		}
		usageMap[accountID] = au
	}
}

func getOrCreateUsage(accountID string) *AccountUsage {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if ok {
		return u
	}

	usageMapMu.Lock()
	defer usageMapMu.Unlock()
	// Double-check after acquiring write lock.
	if u, ok = usageMap[accountID]; ok {
		return u
	}
	u = &AccountUsage{}
	usageMap[accountID] = u
	return u
}

// RecordRequest records a request for the given account.
func RecordRequest(accountID string, failed bool, is429 bool) {
	u := getOrCreateUsage(accountID)
	now := time.Now()

	u.mu.Lock()
	u.records = append(u.records, usageRecord{At: now, Failed: failed, Is429: is429})
	if is429 {
		u.last429 = now
	}
	u.trimLocked(now)
	u.mu.Unlock()

	// Persist to disk asynchronously (debounced in store layer).
	persistUsage()
}

// persistUsage serialises the current usageMap and triggers an async save.
func persistUsage() {
	usageMapMu.RLock()
	accounts := make(map[string]store.PersistedUsageAccount, len(usageMap))
	for id, au := range usageMap {
		au.mu.Lock()
		recs := make([]store.PersistedUsageRecord, len(au.records))
		for i, r := range au.records {
			recs[i] = store.PersistedUsageRecord{
				At:     r.At.Format(time.RFC3339),
				Failed: r.Failed,
				Is429:  r.Is429,
			}
		}
		last429 := ""
		if !au.last429.IsZero() {
			last429 = au.last429.Format(time.RFC3339)
		}
		accounts[id] = store.PersistedUsageAccount{Records: recs, Last429: last429}
		au.mu.Unlock()
	}
	usageMapMu.RUnlock()
	store.RequestSaveUsageStats(accounts)
}

// GetUsageSnapshot returns current usage stats for a single account.
func GetUsageSnapshot(accountID string) AccountUsageSnapshot {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return AccountUsageSnapshot{WindowSeconds: int(usageWindowDuration.Seconds())}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.trimLocked(time.Now())

	var total, failed int64
	for _, r := range u.records {
		total++
		if r.Failed {
			failed++
		}
	}

	snap := AccountUsageSnapshot{
		TotalRequests:  total,
		FailedRequests: failed,
		WindowSeconds:  int(usageWindowDuration.Seconds()),
	}
	if !u.last429.IsZero() {
		snap.Last429At = u.last429.Format(time.RFC3339)
	}
	return snap
}

// GetAllUsageSnapshots returns usage stats for all tracked accounts.
func GetAllUsageSnapshots() map[string]AccountUsageSnapshot {
	usageMapMu.RLock()
	defer usageMapMu.RUnlock()

	result := make(map[string]AccountUsageSnapshot, len(usageMap))
	now := time.Now()
	for id, u := range usageMap {
		u.mu.Lock()
		u.trimLocked(now)
		var total, failed int64
		for _, r := range u.records {
			total++
			if r.Failed {
				failed++
			}
		}
		snap := AccountUsageSnapshot{
			TotalRequests:  total,
			FailedRequests: failed,
			WindowSeconds:  int(usageWindowDuration.Seconds()),
		}
		if !u.last429.IsZero() {
			snap.Last429At = u.last429.Format(time.RFC3339)
		}
		u.mu.Unlock()
		result[id] = snap
	}
	return result
}

// GetWindowRequestCount returns the number of requests in the current window for an account.
// Used by load balancer strategies.
func GetWindowRequestCount(accountID string) int64 {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return 0
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	u.trimLocked(time.Now())
	return int64(len(u.records))
}

// GetLast429Time returns the last time a 429 was recorded for the account.
func GetLast429Time(accountID string) time.Time {
	usageMapMu.RLock()
	u, ok := usageMap[accountID]
	usageMapMu.RUnlock()
	if !ok {
		return time.Time{}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last429
}

// trimLocked removes records outside the sliding window. Must be called with u.mu held.
func (u *AccountUsage) trimLocked(now time.Time) {
	cutoff := now.Add(-usageWindowDuration)
	i := 0
	for i < len(u.records) && u.records[i].At.Before(cutoff) {
		i++
	}
	if i > 0 {
		u.records = u.records[i:]
	}
}
