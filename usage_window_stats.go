package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	usageLogPageSize             = 1000
	maxUsageLogPages             = 10000
	usageZeroConfirmationDelay   = autoResetAccountScanInterval
	usageZeroPendingPayloadField = "utilization_pending_confirmation"
)

type usageWindowDefinition struct {
	usageKey string
	stateKey string
	duration time.Duration
}

var usageWindowDefinitions = []usageWindowDefinition{
	{usageKey: "five_hour", stateKey: "5h", duration: 5 * time.Hour},
	{usageKey: "seven_day", stateKey: "7d", duration: 7 * 24 * time.Hour},
}

type usageWindowState struct {
	StartAt            time.Time  `json:"start_at"`
	ResetAt            time.Time  `json:"reset_at"`
	LastUtilization    *float64   `json:"last_utilization,omitempty"`
	PendingZeroSince   *time.Time `json:"pending_zero_since,omitempty"`
	PendingZeroResetAt *time.Time `json:"pending_zero_reset_at,omitempty"`
}

// usageWindowStateStore 保存 accountinfo 已识别的窗口起点和 0% 确认状态。
// 这里只保存账号 ID、窗口、利用率和时间，不保存用户 token 或 usage log。
type usageWindowStateStore struct {
	mutex   sync.Mutex
	path    string
	entries map[string]usageWindowState
}

func newUsageWindowStateStore(path string) *usageWindowStateStore {
	store := &usageWindowStateStore{
		path:    strings.TrimSpace(path),
		entries: make(map[string]usageWindowState),
	}
	if store.path == "" {
		return store
	}

	data, err := os.ReadFile(store.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("usage window state load failed: %v", err)
		}
		return store
	}
	if err := json.Unmarshal(data, &store.entries); err != nil {
		log.Printf("usage window state parse failed: %v", err)
		store.entries = make(map[string]usageWindowState)
	}
	return store
}

func (store *usageWindowStateStore) resolve(
	accountID int64,
	definition usageWindowDefinition,
	resetAt, suggestedStart, now time.Time,
) (time.Time, bool) {
	startAt, _, ok := store.resolveObserved(
		accountID,
		definition,
		resetAt,
		suggestedStart,
		now,
		0,
		false,
	)
	return startAt, ok
}

func (store *usageWindowStateStore) resolveObserved(
	accountID int64,
	definition usageWindowDefinition,
	resetAt, suggestedStart, now time.Time,
	utilization float64,
	hasUtilization bool,
) (time.Time, bool, bool) {
	if store == nil || accountID <= 0 || resetAt.IsZero() || !resetAt.After(now) {
		return time.Time{}, false, false
	}

	key := strconv.FormatInt(accountID, 10) + ":" + definition.stateKey
	store.mutex.Lock()
	defer store.mutex.Unlock()

	existing, exists := store.entries[key]
	startAt := suggestedStart
	if exists && existing.ResetAt.Equal(resetAt) &&
		existing.StartAt.Before(now) && existing.StartAt.Before(existing.ResetAt) {
		startAt = existing.StartAt.UTC()
	} else if startAt.IsZero() || !startAt.Before(now) || !startAt.Before(resetAt) {
		startAt = resetAt.Add(-definition.duration)
	}
	if !startAt.Before(now) || !startAt.Before(resetAt) {
		return time.Time{}, false, false
	}

	entry := existing
	dirty := !exists || !entry.StartAt.Equal(startAt) || !entry.ResetAt.Equal(resetAt)
	entry.StartAt = startAt.UTC()
	entry.ResetAt = resetAt.UTC()
	pendingZero := false

	if hasUtilization && utilization >= 0 {
		previousWindowStillActive := exists && existing.ResetAt.After(now)
		previousUtilizationWasNonZero := existing.LastUtilization != nil && *existing.LastUtilization > 0
		if utilization == 0 && previousWindowStillActive && previousUtilizationWasNonZero {
			samePendingZero := existing.PendingZeroSince != nil &&
				existing.PendingZeroResetAt != nil &&
				existing.PendingZeroResetAt.Equal(resetAt)
			if samePendingZero && !now.Before(existing.PendingZeroSince.Add(usageZeroConfirmationDelay)) {
				dirty = setUsageWindowUtilization(&entry, 0) || dirty
				dirty = clearPendingUsageZero(&entry) || dirty
			} else {
				pendingZero = true
				if !samePendingZero {
					observedAt := now.UTC()
					observedResetAt := resetAt.UTC()
					entry.PendingZeroSince = &observedAt
					entry.PendingZeroResetAt = &observedResetAt
					dirty = true
				}
			}
		} else {
			dirty = setUsageWindowUtilization(&entry, utilization) || dirty
			dirty = clearPendingUsageZero(&entry) || dirty
		}
	}

	store.entries[key] = entry
	if dirty {
		if err := store.persistLocked(); err != nil {
			log.Printf("usage window state save failed: %v", err)
		}
	}
	return entry.StartAt, pendingZero, true
}

func setUsageWindowUtilization(entry *usageWindowState, utilization float64) bool {
	if entry.LastUtilization != nil && *entry.LastUtilization == utilization {
		return false
	}
	value := utilization
	entry.LastUtilization = &value
	return true
}

func clearPendingUsageZero(entry *usageWindowState) bool {
	if entry.PendingZeroSince == nil && entry.PendingZeroResetAt == nil {
		return false
	}
	entry.PendingZeroSince = nil
	entry.PendingZeroResetAt = nil
	return true
}

func (store *usageWindowStateStore) invalidateAccount(accountID int64) {
	if store == nil || accountID <= 0 {
		return
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()

	dirty := false
	for _, definition := range usageWindowDefinitions {
		key := strconv.FormatInt(accountID, 10) + ":" + definition.stateKey
		if _, exists := store.entries[key]; exists {
			delete(store.entries, key)
			dirty = true
		}
	}
	if dirty {
		if err := store.persistLocked(); err != nil {
			log.Printf("usage window state save failed: %v", err)
		}
	}
}

func (store *usageWindowStateStore) persistLocked() error {
	if store.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(store.path), 0o750); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".usage-window-state-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}

	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(store.entries); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, store.path)
}

type usageLogPage struct {
	Items []usageLogEntry `json:"items"`
	Pages int             `json:"pages"`
}

type usageLogEntry struct {
	UserID              int64   `json:"user_id"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	ActualCost          float64 `json:"actual_cost"`
	CreatedAt           string  `json:"created_at"`
}

type userWindowStats struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

type activeUsageWindow struct {
	progress map[string]any
	startAt  time.Time
	resetAt  time.Time
}

func (a *app) enrichAccountUsageForUser(
	ctx context.Context,
	accountID, userID int64,
	data json.RawMessage,
	now time.Time,
) json.RawMessage {
	if userID <= 0 {
		return data
	}

	payload, activeWindows, ok := a.resolveAccountUsageWindows(accountID, data, now)
	if !ok {
		return data
	}
	if len(activeWindows) == 0 {
		return marshalUsagePayload(payload, data)
	}

	earliestStart := activeWindows[0].startAt
	for _, window := range activeWindows[1:] {
		if window.startAt.Before(earliestStart) {
			earliestStart = window.startAt
		}
	}

	logs, upstreamErr := a.fetchUserUsageLogs(ctx, userID, accountID, earliestStart, now)
	if upstreamErr != nil {
		return marshalUsagePayload(payload, data)
	}

	for _, window := range activeWindows {
		stats := summarizeUserUsageLogs(logs, userID, window.startAt, window.resetAt)
		window.progress["user_window_stats"] = stats
		if pending, _ := window.progress[usageZeroPendingPayloadField].(bool); pending {
			continue
		}

		accountUtilization, hasUtilization := numberValue(window.progress["utilization"])
		accountCost, hasAccountCost := usageWindowAccountCost(window.progress)
		if hasUtilization && hasAccountCost && accountCost > 0 && accountUtilization >= 0 && stats.Cost >= 0 {
			userUtilization := accountUtilization * stats.Cost / accountCost
			if userUtilization < 0 {
				userUtilization = 0
			}
			if userUtilization > 100 {
				userUtilization = 100
			}
			window.progress["user_utilization"] = userUtilization
			window.progress["user_utilization_estimated"] = true
		}
	}

	return marshalUsagePayload(payload, data)
}

func (a *app) resolveAccountUsageWindows(
	accountID int64,
	data json.RawMessage,
	now time.Time,
) (map[string]any, []activeUsageWindow, bool) {
	if len(data) == 0 || accountID <= 0 || a.usageWindowState == nil {
		return nil, nil, false
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil || payload == nil {
		return nil, nil, false
	}

	activeWindows := make([]activeUsageWindow, 0, len(usageWindowDefinitions))
	for _, definition := range usageWindowDefinitions {
		progress, ok := payload[definition.usageKey].(map[string]any)
		if !ok || progress == nil {
			continue
		}
		resetAt, ok := parseUsageTimestamp(progress["resets_at"])
		if !ok || !resetAt.After(now) {
			continue
		}
		suggestedStart, _ := parseUsageTimestamp(progress["window_start_at"])
		utilization, hasUtilization := numberValue(progress["utilization"])
		startAt, pendingZero, ok := a.usageWindowState.resolveObserved(
			accountID,
			definition,
			resetAt,
			suggestedStart,
			now,
			utilization,
			hasUtilization,
		)
		if !ok {
			continue
		}
		if pendingZero {
			progress[usageZeroPendingPayloadField] = true
		} else {
			delete(progress, usageZeroPendingPayloadField)
		}
		progress["window_start_at"] = startAt.Format(time.RFC3339)
		activeWindows = append(activeWindows, activeUsageWindow{
			progress: progress,
			startAt:  startAt,
			resetAt:  resetAt,
		})
	}

	return payload, activeWindows, true
}

func (a *app) fetchUserUsageLogs(
	ctx context.Context,
	userID, accountID int64,
	startAt, endAt time.Time,
) ([]usageLogEntry, *upstreamAPIError) {
	if userID <= 0 || accountID <= 0 || startAt.IsZero() || !endAt.After(startAt) {
		return []usageLogEntry{}, nil
	}

	query := url.Values{
		"page_size":  {strconv.Itoa(usageLogPageSize)},
		"user_id":    {strconv.FormatInt(userID, 10)},
		"account_id": {strconv.FormatInt(accountID, 10)},
		"start_date": {startAt.UTC().Format("2006-01-02")},
		"end_date":   {endAt.UTC().Format("2006-01-02")},
		"timezone":   {"UTC"},
		"sort_by":    {"created_at"},
		"sort_order": {"asc"},
	}

	logs := make([]usageLogEntry, 0)
	for page := 1; page <= maxUsageLogPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var result usageLogPage
		if upstreamErr := a.doAdminRequest(ctx, http.MethodGet, "/admin/usage", query, &result); upstreamErr != nil {
			return nil, upstreamErr
		}
		logs = append(logs, result.Items...)
		if result.Pages <= page || len(result.Items) == 0 {
			break
		}
	}
	return logs, nil
}

func summarizeUserUsageLogs(logs []usageLogEntry, userID int64, startAt, endAt time.Time) userWindowStats {
	stats := userWindowStats{}
	for _, entry := range logs {
		if entry.UserID != userID {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, entry.CreatedAt)
		if err != nil || createdAt.Before(startAt) || !createdAt.Before(endAt) {
			continue
		}
		stats.Requests++
		stats.Tokens += int64(entry.InputTokens + entry.OutputTokens + entry.CacheCreationTokens + entry.CacheReadTokens)
		stats.Cost += entry.ActualCost
	}
	return stats
}

func usageWindowAccountCost(progress map[string]any) (float64, bool) {
	windowStats, ok := progress["window_stats"].(map[string]any)
	if !ok || windowStats == nil {
		return 0, false
	}
	return numberValue(windowStats["cost"])
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func parseUsageTimestamp(value any) (time.Time, bool) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func marshalUsagePayload(payload map[string]any, fallback json.RawMessage) json.RawMessage {
	data, err := json.Marshal(payload)
	if err != nil {
		return fallback
	}
	return data
}
