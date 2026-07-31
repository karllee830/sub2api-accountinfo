package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestUserUsageAggregateRequiresAllFields(t *testing.T) {
	requests := int64(1)
	tokens := int64(2)
	cost := 0.5
	_, upstreamErr := (userUsageAggregate{
		TotalRequests:   &requests,
		TotalTokens:     &tokens,
		TotalActualCost: &cost,
	}).toUserWindowStats()
	if upstreamErr == nil {
		t.Fatal("missing total_account_cost must fail closed")
	}
}

func TestEnrichAccountUsageUsesAccountCostForUserPercentage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/usage" {
			t.Fatalf("unexpected upstream path: %s", request.URL.Path)
		}
		writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[`+
			`{"user_id":2,"input_tokens":10,"output_tokens":10,"total_cost":2,"actual_cost":4,"account_rate_multiplier":1,"created_at":"2026-07-31T10:00:00Z"}`+
			`],"page":1,"page_size":200,"pages":1}}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage(`{"five_hour":{"utilization":25,"resets_at":"2026-07-31T14:00:00Z","window_stats":{"cost":10}}}`)
	enriched := application.enrichAccountUsageForUser(context.Background(), 14, 2, raw, now, false, false)

	var payload map[string]any
	if err := json.Unmarshal(enriched, &payload); err != nil {
		t.Fatal(err)
	}
	progress := payload["five_hour"].(map[string]any)
	stats := progress["user_window_stats"].(map[string]any)
	if stats["cost"] != float64(4) {
		t.Fatalf("displayed user cost = %v, want 4", stats["cost"])
	}
	if progress["user_utilization"] != float64(5) {
		t.Fatalf("user utilization = %v, want 5", progress["user_utilization"])
	}
}

func TestEnrichAccountUsageShowsProvisionalCostWithoutResetTime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/usage" {
			t.Fatalf("unexpected upstream path: %s", request.URL.Path)
		}
		writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[`+
			`{"user_id":2,"input_tokens":10,"output_tokens":10,"total_cost":2,"actual_cost":3,"account_rate_multiplier":1,"created_at":"2026-07-31T10:00:00Z"}`+
			`],"page":1,"page_size":200,"pages":1}}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage(`{"five_hour":{"utilization":20,"window_stats":{"cost":10}}}`)
	enriched := application.enrichAccountUsageForUser(context.Background(), 14, 2, raw, now, true, false)

	var payload map[string]any
	if err := json.Unmarshal(enriched, &payload); err != nil {
		t.Fatal(err)
	}
	progress := payload["five_hour"].(map[string]any)
	if progress[resetTimePendingField] != true {
		t.Fatalf("reset pending marker missing: %#v", progress)
	}
	stats := progress["user_window_stats"].(map[string]any)
	if stats["cost"] != float64(3) {
		t.Fatalf("provisional user cost = %v, want 3", stats["cost"])
	}
	if _, exists := progress["user_utilization"]; exists {
		t.Fatalf("provisional window must not estimate user utilization: %#v", progress)
	}

	_, activeWindows, ok := application.resolveAccountUsageWindowsWithOptions(14, raw, now, false)
	if !ok || len(activeWindows) != 0 {
		t.Fatalf("non-eligible account active windows = %d, want 0", len(activeWindows))
	}
}

func TestProvisionalUsageWindowsOnlyAllowOpenAIOAuth(t *testing.T) {
	tests := []struct {
		name    string
		account accountView
		want    bool
	}{
		{name: "openai oauth", account: accountView{Platform: "openai", Type: "oauth"}, want: true},
		{name: "normalized values", account: accountView{Platform: " OpenAI ", Type: " OAuth "}, want: true},
		{name: "openai api key", account: accountView{Platform: "openai", Type: "apikey"}, want: false},
		{name: "anthropic oauth", account: accountView{Platform: "anthropic", Type: "oauth"}, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allowsProvisionalUsageWindows(test.account); got != test.want {
				t.Fatalf("allowsProvisionalUsageWindows() = %t, want %t", got, test.want)
			}
		})
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
	enriched := application.enrichAccountUsageForUser(context.Background(), 14, 2, raw, now, false, false)

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
	if progress[userStatsUnavailableField] != true {
		t.Fatalf("missing unavailable marker: %#v", progress)
	}
}

func TestFetchUserWindowStatsUsesAggregateAndOnlyStartDayBoundary(t *testing.T) {
	var statsCalls, detailCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/usage/stats":
			statsCalls++
			query := request.URL.Query()
			if query.Get("start_date") != "2026-07-30" || query.Get("end_date") != "2026-07-31" || query.Get("nocache") != "true" {
				t.Fatalf("aggregate query = %q", request.URL.RawQuery)
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"data":{"total_requests":10,"total_tokens":100,"total_actual_cost":5,"total_account_cost":5}}`)
		case "/api/v1/admin/usage":
			detailCalls++
			query := request.URL.Query()
			if query.Get("start_date") != "2026-07-29" || query.Get("end_date") != "2026-07-29" || query.Get("sort_order") != "desc" {
				t.Fatalf("boundary query = %q", request.URL.RawQuery)
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[`+
				`{"user_id":2,"input_tokens":10,"output_tokens":5,"cache_creation_tokens":2,"cache_read_tokens":3,"total_cost":1.5,"actual_cost":1.5,"account_rate_multiplier":1,"created_at":"2026-07-29T19:00:00Z"},`+
				`{"user_id":2,"input_tokens":100,"output_tokens":100,"total_cost":9,"actual_cost":9,"account_rate_multiplier":1,"created_at":"2026-07-29T17:59:59Z"}`+
				`],"page":1,"page_size":200,"pages":1}}`)
		default:
			t.Fatalf("unexpected upstream path: %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	stats, upstreamErr := application.fetchUserWindowStats(
		context.Background(),
		2,
		14,
		time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		true,
	)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if stats.Requests != 11 || stats.Tokens != 120 || stats.Cost != 6.5 || stats.QuotaCost != 6.5 {
		t.Fatalf("stats = %+v, want requests=11 tokens=120 cost=6.5 quota_cost=6.5", stats)
	}
	if statsCalls != 1 || detailCalls != 1 {
		t.Fatalf("calls stats=%d detail=%d, want 1 and 1", statsCalls, detailCalls)
	}
}

func TestFetchUserWindowStatsSubtractsShortPrefix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/usage/stats":
			query := request.URL.Query()
			if query.Get("start_date") != "2026-07-29" || query.Get("end_date") != "2026-07-31" {
				t.Fatalf("aggregate query = %q", request.URL.RawQuery)
			}
			if request.URL.Query().Get("nocache") != "" {
				t.Fatalf("normal aggregate query bypassed cache: %q", request.URL.RawQuery)
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"data":{"total_requests":20,"total_tokens":200,"total_actual_cost":10,"total_account_cost":10}}`)
		case "/api/v1/admin/usage":
			if request.URL.Query().Get("sort_order") != "asc" {
				t.Fatalf("boundary query = %q", request.URL.RawQuery)
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[`+
				`{"user_id":2,"input_tokens":10,"output_tokens":10,"total_cost":1,"actual_cost":1,"account_rate_multiplier":1,"created_at":"2026-07-29T02:00:00Z"},`+
				`{"user_id":2,"input_tokens":90,"output_tokens":90,"total_cost":8,"actual_cost":8,"account_rate_multiplier":1,"created_at":"2026-07-29T04:00:00Z"}`+
				`],"page":1,"page_size":200,"pages":1}}`)
		default:
			t.Fatalf("unexpected upstream path: %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	stats, upstreamErr := application.fetchUserWindowStats(
		context.Background(),
		2,
		14,
		time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		false,
	)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if stats.Requests != 19 || stats.Tokens != 180 || stats.Cost != 9 || stats.QuotaCost != 9 {
		t.Fatalf("stats = %+v, want requests=19 tokens=180 cost=9 quota_cost=9", stats)
	}
}

func TestUsageBoundaryRetriesWithSmallerPageWhenResponseIsTooLarge(t *testing.T) {
	var pageSizes []string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		pageSize := request.URL.Query().Get("page_size")
		pageSizes = append(pageSizes, pageSize)
		if pageSize == "200" {
			writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[]}}`+strings.Repeat(" ", maxUpstreamBody))
			return
		}
		writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[{"user_id":2,"input_tokens":1,"output_tokens":2,"total_cost":0.5,"actual_cost":0.5,"account_rate_multiplier":1,"created_at":"2026-07-31T10:30:00Z"}],"page":1,"page_size":100,"pages":1}}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	stats, upstreamErr := application.fetchUserUsageBoundary(
		context.Background(),
		2,
		14,
		time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		"desc",
	)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if stats.Requests != 1 || stats.Tokens != 3 || stats.Cost != 0.5 {
		t.Fatalf("stats = %+v", stats)
	}
	if strings.Join(pageSizes, ",") != "200,100" {
		t.Fatalf("page sizes = %v, want [200 100]", pageSizes)
	}
}

func TestUsageBoundaryRejectsMalformedTimestamp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeUpstream(response, http.StatusOK, `{"code":0,"data":{"items":[`+
			`{"user_id":2,"input_tokens":1,"output_tokens":2,"total_cost":0.5,"actual_cost":0.5,"created_at":"invalid"}`+
			`],"page":1,"page_size":200,"pages":1}}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	_, upstreamErr := application.fetchUserUsageBoundary(
		context.Background(),
		2,
		14,
		time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
		"desc",
	)
	if upstreamErr == nil {
		t.Fatal("malformed timestamp must fail closed")
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

func TestResolveAccountUsageWindowsPreservesActiveWindowForExpiredZeroReset(t *testing.T) {
	application := &app{usageWindowState: newUsageWindowStateStore(filepath.Join(t.TempDir(), "usage-window-state.json"))}
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	realResetAt := start.Add(2 * time.Hour)

	application.resolveAccountUsageWindows(
		14,
		json.RawMessage(`{"five_hour":{"utilization":25,"resets_at":"`+realResetAt.Format(time.RFC3339)+`"}}`),
		start,
	)
	payload, activeWindows, ok := application.resolveAccountUsageWindows(
		14,
		json.RawMessage(`{"five_hour":{"utilization":0,"remaining_seconds":0,"resets_at":"`+start.Format(time.RFC3339)+`"}}`),
		start.Add(time.Minute),
	)
	if !ok || len(activeWindows) != 1 {
		t.Fatalf("resolved=%t active_windows=%d, want true and 1", ok, len(activeWindows))
	}
	progress, ok := payload["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("five_hour payload = %#v", payload["five_hour"])
	}
	if progress[usageZeroPendingPayloadField] != true {
		t.Fatalf("pending zero marker missing: %#v", progress)
	}
	if progress["resets_at"] != realResetAt.Format(time.RFC3339) {
		t.Fatalf("resets_at = %v, want %s", progress["resets_at"], realResetAt.Format(time.RFC3339))
	}
}
