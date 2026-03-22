package store

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// PersistedUsageAccount represents a single account's usage data stored on disk.
type PersistedUsageAccount struct {
	Records []PersistedUsageRecord `json:"records"`
	Last429 string                 `json:"last429,omitempty"` // RFC3339 or empty
}

// PersistedUsageRecord is a JSON-serializable version of instance.usageRecord.
type PersistedUsageRecord struct {
	At     string `json:"at"`     // RFC3339
	Failed bool   `json:"failed"`
	Is429  bool   `json:"is429"`
}

// persistedUsageStore is the top-level object stored in usage-stats.json.
type persistedUsageStore struct {
	Accounts map[string]PersistedUsageAccount `json:"accounts"`
	SavedAt  string                          `json:"savedAt"` // RFC3339
}

var (
	saveReq   chan map[string]PersistedUsageAccount
	saveReqMu atomic.Bool // ensures channel is created only once
)

// getSaveReqChan returns the save request channel, creating it exactly once.
func getSaveReqChan() chan map[string]PersistedUsageAccount {
	if !saveReqMu.Load() {
		if saveReqMu.CompareAndSwap(false, true) {
			saveReq = make(chan map[string]PersistedUsageAccount, 1)
			go saveLoop()
		}
	}
	return saveReq
}

// saveLoop runs in a single background goroutine, debouncing saves every 5 seconds.
func saveLoop() {
	var pending map[string]PersistedUsageAccount
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case data, ok := <-saveReq:
			if !ok {
				// Channel closed — flush final save
				if pending != nil {
					saveToFile(pending)
				}
				return
			}
			pending = data
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(5 * time.Second)

		case <-timer.C:
			if pending != nil {
				saveToFile(pending)
				pending = nil
			}
		}
	}
}

// saveToFile writes usage stats to disk (called only from the saveLoop goroutine).
func saveToFile(accounts map[string]PersistedUsageAccount) {
	store := persistedUsageStore{
		Accounts: accounts,
		SavedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(UsageStatsFile(), data, 0644)
}

// LoadUsageStats loads persisted usage stats from disk and returns them.
// Called once at startup from instance.LoadUsage (outside the saveLoop goroutine).
func LoadUsageStats() map[string]PersistedUsageAccount {
	data, err := os.ReadFile(UsageStatsFile())
	if err != nil || len(data) == 0 || string(data) == "{}" {
		return make(map[string]PersistedUsageAccount)
	}

	var s persistedUsageStore
	if err := json.Unmarshal(data, &s); err != nil {
		return make(map[string]PersistedUsageAccount)
	}
	if s.Accounts == nil {
		s.Accounts = make(map[string]PersistedUsageAccount)
	}
	return s.Accounts
}

// RequestSaveUsageStats sends a save request to the background goroutine.
// Safe to call from multiple goroutines.
func RequestSaveUsageStats(accounts map[string]PersistedUsageAccount) {
	// Clone the data so the caller retains ownership.
	clone := make(map[string]PersistedUsageAccount, len(accounts))
	for k, v := range accounts {
		clone[k] = v
	}

	// Non-blocking send. If the channel is full, skip this save — the next
	// one will carry the latest data anyway.
	select {
	case getSaveReqChan() <- clone:
	default:
	}
}
