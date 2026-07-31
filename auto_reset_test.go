package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoResetWorkerUsesCreditAtLeadTimeWithoutFrequentQuotaQueries(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(50 * time.Minute)
	var accountScans atomic.Int32
	var quotaQueries atomic.Int32
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts":
			accountScans.Add(1)
			for key, want := range map[string]string{
				"platform": "openai",
				"type":     "oauth",
				"status":   "active",
				"lite":     "true",
			} {
				if got := request.URL.Query().Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			writeUpstream(response, http.StatusOK, `{
				"code":0,
				"message":"success",
				"data":{
					"items":[
						{"id":14,"name":"parent","platform":"openai","type":"oauth","status":"active"},
						{"id":15,"name":"shadow","platform":"openai","type":"oauth","status":"active","parent_account_id":14}
					],
					"total":2,
					"page":1,
					"page_size":100,
					"pages":1
				}
			}`)
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"active"}}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			quotaQueries.Add(1)
			writeUpstream(
				response,
				http.StatusOK,
				fmt.Sprintf(
					`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
					expiresAt.Format(time.RFC3339),
				),
			)
		case "/api/v1/admin/openai/accounts/14/reset-quota":
			if request.Method != http.MethodPost {
				t.Errorf("reset method = %s, want POST", request.Method)
			}
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	currentTime := start
	application.quotaCache.now = func() time.Time { return currentTime }
	application.resetCoordinator.now = func() time.Time { return currentTime }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return currentTime }

	worker.step(context.Background(), currentTime)
	if accountScans.Load() != 1 || quotaQueries.Load() != 1 || resets.Load() != 0 {
		t.Fatalf(
			"after discovery: scans=%d quota=%d resets=%d",
			accountScans.Load(),
			quotaQueries.Load(),
			resets.Load(),
		)
	}
	if len(worker.accounts) != 1 {
		t.Fatalf("tracked accounts = %d, want only the non-shadow account", len(worker.accounts))
	}
	state := worker.accounts[14]
	if want := expiresAt.Add(-autoResetLeadTime); !state.resetAt.Equal(want) {
		t.Fatalf("resetAt = %s, want %s", state.resetAt, want)
	}
	schedules := application.autoResetPlans.forAccounts(map[int64]struct{}{14: {}})
	if len(schedules) != 1 || schedules[0].AccountID != 14 || !schedules[0].ResetAt.Equal(state.resetAt) || !schedules[0].ExpiresAt.Equal(state.expiresAt) {
		t.Fatalf("published schedules = %#v, want account 14 at %s", schedules, state.resetAt)
	}

	currentTime = start.Add(20 * time.Minute)
	worker.step(context.Background(), currentTime)
	if quotaQueries.Load() != 1 || resets.Load() != 0 {
		t.Fatalf("before lead time: quota=%d resets=%d", quotaQueries.Load(), resets.Load())
	}

	currentTime = expiresAt.Add(-autoResetLeadTime)
	worker.step(context.Background(), currentTime)
	if quotaQueries.Load() != 2 {
		t.Fatalf("verification quota queries = %d, want 2 total", quotaQueries.Load())
	}
	if resets.Load() != 1 {
		t.Fatalf("resets = %d, want 1", resets.Load())
	}

	currentTime = currentTime.Add(time.Minute)
	worker.step(context.Background(), currentTime)
	if quotaQueries.Load() != 2 || resets.Load() != 1 {
		t.Fatalf("immediate repeat: quota=%d resets=%d", quotaQueries.Load(), resets.Load())
	}

	currentTime = currentTime.Add(autoResetPostResetCheck - time.Minute)
	worker.step(context.Background(), currentTime)
	if quotaQueries.Load() != 4 || resets.Load() != 1 {
		t.Fatalf("unchanged post-reset snapshot: quota=%d resets=%d", quotaQueries.Load(), resets.Load())
	}
}

func TestAutoResetScheduleStoreFiltersAccounts(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := newAutoResetScheduleStore()
	store.replace([]autoResetSchedule{
		{AccountID: 14, ResetAt: now.Add(20 * time.Minute), ExpiresAt: now.Add(30 * time.Minute)},
		{AccountID: 16, ResetAt: now.Add(10 * time.Minute), ExpiresAt: now.Add(20 * time.Minute)},
	})

	schedules := store.forAccounts(map[int64]struct{}{16: {}})
	if len(schedules) != 1 || schedules[0].AccountID != 16 {
		t.Fatalf("visible schedules = %#v, want only account 16", schedules)
	}
}

func TestAutoResetWorkerSkipsCreditConsumedDirectlyInSub2API(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(20 * time.Minute)
	var quotaQueries atomic.Int32
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts":
			writeUpstream(response, http.StatusOK, `{
				"code":0,
				"message":"success",
				"data":{
					"items":[{"id":14,"platform":"openai","type":"oauth","status":"active"}],
					"total":1,
					"page":1,
					"page_size":100,
					"pages":1
				}
			}`)
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"active"}}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			if quotaQueries.Add(1) == 1 {
				writeUpstream(
					response,
					http.StatusOK,
					fmt.Sprintf(
						`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
						expiresAt.Format(time.RFC3339),
					),
				)
				return
			}
			writeUpstream(
				response,
				http.StatusOK,
				`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":0,"credits":[]}}}`,
			)
		case "/api/v1/admin/openai/accounts/14/reset-quota":
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	currentTime := start
	application.quotaCache.now = func() time.Time { return currentTime }
	application.resetCoordinator.now = func() time.Time { return currentTime }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return currentTime }

	worker.step(context.Background(), currentTime)
	currentTime = expiresAt.Add(-autoResetLeadTime)
	worker.step(context.Background(), currentTime)

	if quotaQueries.Load() != 2 {
		t.Fatalf("quota queries = %d, want initial query plus fresh trigger check", quotaQueries.Load())
	}
	if resets.Load() != 0 {
		t.Fatalf("resets = %d, want none after the credit was consumed directly in Sub2API", resets.Load())
	}
	if state := worker.accounts[14]; !state.resetAt.IsZero() || !state.expiresAt.IsZero() {
		t.Fatalf("consumed credit remained scheduled: resetAt=%s expiresAt=%s", state.resetAt, state.expiresAt)
	}
}

func TestAutoResetWorkerSkipsAccountDisabledBeforeTrigger(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(20 * time.Minute)
	var quotaQueries atomic.Int32
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts":
			writeUpstream(response, http.StatusOK, `{
				"code":0,
				"message":"success",
				"data":{
					"items":[{"id":14,"platform":"openai","type":"oauth","status":"active"}],
					"total":1,
					"page":1,
					"page_size":100,
					"pages":1
				}
			}`)
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"inactive"}}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			quotaQueries.Add(1)
			writeUpstream(
				response,
				http.StatusOK,
				fmt.Sprintf(
					`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
					expiresAt.Format(time.RFC3339),
				),
			)
		case "/api/v1/admin/openai/accounts/14/reset-quota":
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	currentTime := start
	application.quotaCache.now = func() time.Time { return currentTime }
	application.resetCoordinator.now = func() time.Time { return currentTime }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return currentTime }

	worker.step(context.Background(), currentTime)
	currentTime = expiresAt.Add(-autoResetLeadTime)
	worker.step(context.Background(), currentTime)

	if quotaQueries.Load() != 1 {
		t.Fatalf("quota queries = %d, want only the initial query before the account was disabled", quotaQueries.Load())
	}
	if resets.Load() != 0 {
		t.Fatalf("resets = %d, want none for an inactive account", resets.Load())
	}
	if state := worker.accounts[14]; !state.resetAt.IsZero() || !state.expiresAt.IsZero() {
		t.Fatalf("inactive account remained scheduled: resetAt=%s expiresAt=%s", state.resetAt, state.expiresAt)
	}
}

func TestAutoResetAndManualResetShareUnderlyingAccountGuard(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(5 * time.Minute)
	quotaStarted := make(chan struct{})
	releaseQuota := make(chan struct{})
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-shared"}}}`)
		case "/api/v1/admin/accounts/16":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":16,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-shared"}}}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			close(quotaStarted)
			<-releaseQuota
			writeUpstream(
				response,
				http.StatusOK,
				fmt.Sprintf(
					`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
					expiresAt.Format(time.RFC3339),
				),
			)
		case "/api/v1/admin/openai/accounts/14/reset-quota",
			"/api/v1/admin/openai/accounts/16/reset-quota":
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	application.quotaCache.now = func() time.Time { return start }
	application.resetCoordinator.now = func() time.Time { return start }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return start }
	state := &autoResetAccountState{resetAt: start, expiresAt: expiresAt}

	autoDone := make(chan struct{})
	go func() {
		defer close(autoDone)
		worker.consumeExpiringCredit(context.Background(), 14, state)
	}()
	<-quotaStarted

	manualResult := make(chan *upstreamAPIError, 1)
	go func() {
		var result autoResetResult
		manualResult <- application.resetOpenAIQuota(context.Background(), 16, &result)
	}()

	close(releaseQuota)
	<-autoDone
	manualErr := <-manualResult
	if manualErr == nil || manualErr.Status != http.StatusConflict {
		t.Fatalf("manual reset error = %#v, want recent-attempt conflict", manualErr)
	}
	if resets.Load() != 1 {
		t.Fatalf("upstream resets = %d, want exactly one", resets.Load())
	}
}

func TestAutoResetWorkerRetriesFailedVerificationBeforeExpiry(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(20 * time.Minute)
	currentTime := expiresAt.Add(-autoResetLeadTime)
	var quotaQueries atomic.Int32
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"active"}}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			if quotaQueries.Add(1) == 1 {
				writeUpstream(
					response,
					http.StatusBadGateway,
					`{"code":"UPSTREAM_ERROR","message":"quota temporarily unavailable"}`,
				)
				return
			}
			writeUpstream(
				response,
				http.StatusOK,
				fmt.Sprintf(
					`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
					expiresAt.Format(time.RFC3339),
				),
			)
		case "/api/v1/admin/openai/accounts/14/reset-quota":
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	application.quotaCache.now = func() time.Time { return currentTime }
	application.resetCoordinator.now = func() time.Time { return currentTime }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return currentTime }
	state := &autoResetAccountState{resetAt: currentTime, expiresAt: expiresAt}

	worker.consumeExpiringCredit(context.Background(), 14, state)
	if want := currentTime.Add(autoResetVerificationRetry); !state.resetAt.Equal(want) {
		t.Fatalf("verification retry = %s, want %s", state.resetAt, want)
	}
	if resets.Load() != 0 {
		t.Fatalf("resets after failed verification = %d, want 0", resets.Load())
	}

	currentTime = state.resetAt
	worker.consumeExpiringCredit(context.Background(), 14, state)
	if quotaQueries.Load() != 2 || resets.Load() != 1 {
		t.Fatalf("after verification retry: quota=%d resets=%d", quotaQueries.Load(), resets.Load())
	}
}

func TestAutoResetWorkerProcessesDueAccountsConcurrently(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expiresAt := start.Add(5 * time.Minute)
	quotaStarted := make(chan int64, 2)
	releaseQuota := make(chan struct{})
	var resets atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var accountID int64
		switch request.URL.Path {
		case "/api/v1/admin/accounts/14":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":14,"platform":"openai","type":"oauth","status":"active"}}`)
			return
		case "/api/v1/admin/accounts/16":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":16,"platform":"openai","type":"oauth","status":"active"}}`)
			return
		case "/api/v1/admin/openai/accounts/14/quota":
			accountID = 14
		case "/api/v1/admin/openai/accounts/16/quota":
			accountID = 16
		case "/api/v1/admin/openai/accounts/14/reset-quota",
			"/api/v1/admin/openai/accounts/16/reset-quota":
			resets.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
			return
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}

		quotaStarted <- accountID
		<-releaseQuota
		writeUpstream(
			response,
			http.StatusOK,
			fmt.Sprintf(
				`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1,"credits":[{"expires_at":%q}]}}}`,
				expiresAt.Format(time.RFC3339),
			),
		)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	application.quotaCache.now = func() time.Time { return start }
	application.resetCoordinator.now = func() time.Time { return start }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return start }
	tasks := []autoResetTask{
		{accountID: 14, state: &autoResetAccountState{resetAt: start, expiresAt: expiresAt}},
		{accountID: 16, state: &autoResetAccountState{resetAt: start, expiresAt: expiresAt}},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.consumeDueCredits(context.Background(), tasks)
	}()

	started := map[int64]bool{}
	for range tasks {
		select {
		case accountID := <-quotaStarted:
			started[accountID] = true
		case <-time.After(time.Second):
			t.Fatal("due quota confirmations did not start concurrently")
		}
	}
	close(releaseQuota)
	<-done

	if !started[14] || !started[16] {
		t.Fatalf("started accounts = %#v, want both due accounts", started)
	}
	if resets.Load() != 2 {
		t.Fatalf("upstream resets = %d, want 2", resets.Load())
	}
}

func TestAutoResetWorkerStaggersInitialQuotaQueries(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/accounts" {
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
		writeUpstream(response, http.StatusOK, `{
			"code":0,
			"message":"success",
			"data":{
				"items":[
					{"id":14,"platform":"openai","type":"oauth","status":"active"},
					{"id":15,"platform":"openai","type":"oauth","status":"active"},
					{"id":16,"platform":"openai","type":"oauth","status":"active"},
					{"id":17,"platform":"openai","type":"oauth","status":"active"},
					{"id":18,"platform":"openai","type":"oauth","status":"active"},
					{"id":19,"platform":"openai","type":"oauth","status":"active"},
					{"id":20,"platform":"openai","type":"oauth","status":"active"},
					{"id":21,"platform":"openai","type":"oauth","status":"active"},
					{"id":22,"platform":"openai","type":"oauth","status":"active"}
				],
				"total":9,
				"page":1,
				"page_size":100,
				"pages":1
			}
		}`)
	}))
	defer upstream.Close()

	worker := newAutoResetWorker(newApp(testConfig(t, upstream.URL)))
	worker.discoverAccounts(context.Background(), start)
	if got := worker.accounts[14].nextQuotaCheck; !got.Equal(start) {
		t.Fatalf("first account query = %s, want %s", got, start)
	}
	if got := worker.accounts[21].nextQuotaCheck; !got.Equal(start) {
		t.Fatalf("eighth account query = %s, want %s", got, start)
	}
	if got, want := worker.accounts[22].nextQuotaCheck, start.Add(autoResetInitialQuerySpacing); !got.Equal(want) {
		t.Fatalf("ninth account query = %s, want %s", got, want)
	}
}

func TestAutoResetWorkerDiscoversNewAccountBeforeQuotaRefresh(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	var accountScans atomic.Int32
	var account14Queries atomic.Int32
	var account16Queries atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/admin/accounts":
			if accountScans.Add(1) == 1 {
				writeUpstream(response, http.StatusOK, `{
					"code":0,
					"message":"success",
					"data":{
						"items":[{"id":14,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-14"}}],
						"total":1,
						"page":1,
						"page_size":100,
						"pages":1
					}
				}`)
				return
			}
			writeUpstream(response, http.StatusOK, `{
				"code":0,
				"message":"success",
				"data":{
					"items":[
						{"id":14,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-14"}},
						{"id":16,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-16"}}
					],
					"total":2,
					"page":1,
					"page_size":100,
					"pages":1
				}
			}`)
		case "/api/v1/admin/openai/accounts/14/quota":
			account14Queries.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":0,"credits":[]}}}`)
		case "/api/v1/admin/openai/accounts/16/quota":
			account16Queries.Add(1)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":0,"credits":[]}}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.autoResetCredits = true
	application := newApp(cfg)
	currentTime := start
	application.quotaCache.now = func() time.Time { return currentTime }
	worker := newAutoResetWorker(application)
	worker.now = func() time.Time { return currentTime }

	worker.step(context.Background(), currentTime)
	if account14Queries.Load() != 1 || account16Queries.Load() != 0 {
		t.Fatalf("initial quota queries: account14=%d account16=%d", account14Queries.Load(), account16Queries.Load())
	}

	currentTime = start.Add(autoResetAccountScanInterval)
	worker.step(context.Background(), currentTime)
	if accountScans.Load() != 2 {
		t.Fatalf("account scans = %d, want 2", accountScans.Load())
	}
	if account14Queries.Load() != 1 {
		t.Fatalf("existing account quota queries = %d, want no 6-hour refresh yet", account14Queries.Load())
	}
	if account16Queries.Load() != 1 {
		t.Fatalf("new account quota queries = %d, want immediate initial query after discovery", account16Queries.Load())
	}
}

func TestAutoResetWorkerDeduplicatesShadowAndSharedOpenAIAccount(t *testing.T) {
	start := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/accounts" {
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
		writeUpstream(response, http.StatusOK, `{
			"code":0,
			"message":"success",
			"data":{
				"items":[
					{"id":14,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-shared"}},
					{"id":15,"platform":"openai","type":"oauth","status":"active","parent_account_id":14,"credentials":{}},
					{"id":16,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-shared"}},
					{"id":17,"platform":"openai","type":"oauth","status":"active","credentials":{"chatgpt_account_id":"org-other"}}
				],
				"total":4,
				"page":1,
				"page_size":100,
				"pages":1
			}
		}`)
	}))
	defer upstream.Close()

	worker := newAutoResetWorker(newApp(testConfig(t, upstream.URL)))
	worker.discoverAccounts(context.Background(), start)

	if len(worker.accounts) != 2 {
		t.Fatalf("tracked accounts = %d, want one per underlying OpenAI account", len(worker.accounts))
	}
	if worker.accounts[14] == nil || worker.accounts[17] == nil {
		t.Fatalf("tracked account IDs = %#v, want representative IDs 14 and 17", worker.accounts)
	}
	if worker.accounts[15] != nil || worker.accounts[16] != nil {
		t.Fatalf("shadow or duplicate account was scheduled: %#v", worker.accounts)
	}
}

func TestEarliestFutureCreditExpiry(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	want := now.Add(20 * time.Minute)
	quota := autoResetQuota{
		RateLimitResetCredits: &autoResetCreditSummary{
			AvailableCount: 4,
			Credits: []autoResetCredit{
				{ExpiresAt: "not-a-date"},
				{ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)},
				{ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)},
				{ExpiresAt: want.Format(time.RFC3339)},
			},
		},
	}
	got, exists := earliestFutureCreditExpiry(quota, now)
	if !exists || !got.Equal(want) {
		t.Fatalf("earliest expiry = %s, exists=%t, want %s", got, exists, want)
	}
}
