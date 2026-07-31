package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageWindowStateStorePersistsWindowStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-window-state.json")
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	definition := usageWindowDefinitions[0]

	store := newUsageWindowStateStore(path)
	got, ok := store.resolve(14, definition, resetAt, time.Time{}, now)
	if !ok {
		t.Fatal("resolve() returned false")
	}
	want := resetAt.Add(-5 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("start = %v, want %v", got, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file was not persisted: %v", err)
	}

	reloaded := newUsageWindowStateStore(path)
	got, ok = reloaded.resolve(14, definition, resetAt, resetAt.Add(-time.Hour), now)
	if !ok || !got.Equal(want) {
		t.Fatalf("reloaded start = %v, %v; want %v, true", got, ok, want)
	}
}

func TestSummarizeUserUsageLogsUsesHalfOpenWindow(t *testing.T) {
	start := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	logs := []usageLogEntry{
		{UserID: 2, InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 3, CacheReadTokens: 4, ActualCost: 0.5, CreatedAt: start.Format(time.RFC3339)},
		{UserID: 2, InputTokens: 1, OutputTokens: 2, ActualCost: 0.25, CreatedAt: end.Add(-time.Second).Format(time.RFC3339)},
		{UserID: 2, InputTokens: 100, OutputTokens: 100, ActualCost: 5, CreatedAt: end.Format(time.RFC3339)},
		{UserID: 3, InputTokens: 100, OutputTokens: 100, ActualCost: 5, CreatedAt: start.Add(time.Minute).Format(time.RFC3339)},
	}

	stats := summarizeUserUsageLogs(logs, 2, start, end)
	if stats.Requests != 2 {
		t.Fatalf("requests = %d, want 2", stats.Requests)
	}
	if stats.Tokens != 40 {
		t.Fatalf("tokens = %d, want 40", stats.Tokens)
	}
	if stats.Cost != 0.75 {
		t.Fatalf("cost = %v, want 0.75", stats.Cost)
	}
}

func TestEnrichAccountUsageKeepsProviderUsageWhenLogRequestFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/usage" {
			t.Fatalf("unexpected upstream path: %s", request.URL.Path)
		}
		writeUpstream(response, http.StatusBadGateway, `{"code":502,"message":"temporary usage log failure"}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage(`{"five_hour":{"utilization":25,"resets_at":"2026-07-31T14:00:00Z","window_stats":{"cost":10}}}`)
	enriched := application.enrichAccountUsageForUser(context.Background(), 14, 2, raw, now)

	var payload map[string]any
	if err := json.Unmarshal(enriched, &payload); err != nil {
		t.Fatal(err)
	}
	progress, ok := payload["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour payload = %#v", payload["five_hour"])
	}
	if progress["utilization"] != float64(25) {
		t.Fatalf("provider utilization = %v, want 25", progress["utilization"])
	}
	if _, ok := progress["user_window_stats"]; ok {
		t.Fatal("user window stats must be omitted when usage logs fail")
	}
}

func TestUsageZeroRequiresSeparatedConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-window-state.json")
	store := newUsageWindowStateStore(path)
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resetAt := start.Add(2 * time.Hour)
	definition := usageWindowDefinitions[0]

	observe := func(at time.Time, utilization float64) bool {
		t.Helper()
		_, pending, ok := store.resolveObserved(14, definition, resetAt, time.Time{}, at, utilization, true)
		if !ok {
			t.Fatal("resolveObserved() returned false")
		}
		return pending
	}

	if observe(start, 25) {
		t.Fatal("initial non-zero utilization must not be pending")
	}
	firstZeroAt := start.Add(time.Minute)
	if !observe(firstZeroAt, 0) {
		t.Fatal("first unexpected zero must be pending")
	}

	store = newUsageWindowStateStore(path)
	if !observe(firstZeroAt.Add(usageZeroConfirmationDelay-time.Second), 0) {
		t.Fatal("zero before the confirmation delay must remain pending after reload")
	}
	if observe(firstZeroAt.Add(usageZeroConfirmationDelay), 0) {
		t.Fatal("separated zero confirmation must be accepted")
	}

	state := store.entries["14:5h"]
	if state.LastUtilization == nil || *state.LastUtilization != 0 {
		t.Fatalf("confirmed utilization = %v, want 0", state.LastUtilization)
	}
	if state.PendingZeroSince != nil || state.PendingZeroResetAt != nil {
		t.Fatalf("pending zero was not cleared: %#v", state)
	}
}

func TestUsageZeroRecoveryAndKnownResetClearPendingState(t *testing.T) {
	store := newUsageWindowStateStore(filepath.Join(t.TempDir(), "usage-window-state.json"))
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resetAt := start.Add(2 * time.Hour)
	definition := usageWindowDefinitions[0]

	if _, _, ok := store.resolveObserved(14, definition, resetAt, time.Time{}, start, 25, true); !ok {
		t.Fatal("initial observation failed")
	}
	if _, pending, ok := store.resolveObserved(14, definition, resetAt, time.Time{}, start.Add(time.Minute), 0, true); !ok || !pending {
		t.Fatalf("first zero pending = %t, ok = %t; want true, true", pending, ok)
	}
	if _, pending, ok := store.resolveObserved(14, definition, resetAt, time.Time{}, start.Add(2*time.Minute), 25, true); !ok || pending {
		t.Fatalf("recovered value pending = %t, ok = %t; want false, true", pending, ok)
	}

	store.invalidateAccount(14)
	if _, pending, ok := store.resolveObserved(14, definition, resetAt, time.Time{}, start.Add(3*time.Minute), 0, true); !ok || pending {
		t.Fatalf("known reset zero pending = %t, ok = %t; want false, true", pending, ok)
	}
}

func TestResolveAccountUsageWindowsMarksPendingZero(t *testing.T) {
	application := &app{usageWindowState: newUsageWindowStateStore(filepath.Join(t.TempDir(), "usage-window-state.json"))}
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	resetAt := "2026-07-31T14:00:00Z"

	application.resolveAccountUsageWindows(14, json.RawMessage(`{"five_hour":{"utilization":25,"resets_at":"`+resetAt+`"}}`), start)
	payload, _, ok := application.resolveAccountUsageWindows(
		14,
		json.RawMessage(`{"five_hour":{"utilization":0,"resets_at":"`+resetAt+`"}}`),
		start.Add(time.Minute),
	)
	if !ok {
		t.Fatal("zero payload was not resolved")
	}
	progress, ok := payload["five_hour"].(map[string]any)
	if !ok || progress[usageZeroPendingPayloadField] != true {
		t.Fatalf("pending zero marker missing: %#v", payload["five_hour"])
	}
}
