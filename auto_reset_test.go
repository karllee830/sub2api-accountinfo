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
	worker := newAutoResetWorker(application)

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
					{"id":16,"platform":"openai","type":"oauth","status":"active"}
				],
				"total":2,
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
	if got, want := worker.accounts[16].nextQuotaCheck, start.Add(autoResetInitialQuerySpacing); !got.Equal(want) {
		t.Fatalf("second account query = %s, want %s", got, want)
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
