package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountQuotaCacheLimitsSuccessfulRequests(t *testing.T) {
	cache := newAccountQuotaCache()
	currentTime := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return currentTime }
	var loads atomic.Int32
	loader := func() (json.RawMessage, *upstreamAPIError) {
		loads.Add(1)
		return json.RawMessage(`{"rate_limit_reset_credits":{"available_count":1}}`), nil
	}

	for range 3 {
		if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
			t.Fatal(upstreamErr)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}

	currentTime = currentTime.Add(accountQuotaCacheTTL - time.Second)
	if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if loads.Load() != 1 {
		t.Fatalf("loads before TTL = %d, want 1", loads.Load())
	}

	currentTime = currentTime.Add(2 * time.Second)
	if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads after TTL = %d, want 2", loads.Load())
	}
}

func TestAccountQuotaCacheLimitsFailedRequests(t *testing.T) {
	cache := newAccountQuotaCache()
	currentTime := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	cache.now = func() time.Time { return currentTime }
	var loads atomic.Int32
	expectedError := &upstreamAPIError{Message: "quota unavailable"}
	loader := func() (json.RawMessage, *upstreamAPIError) {
		loads.Add(1)
		return nil, expectedError
	}

	for range 3 {
		if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != expectedError {
			t.Fatalf("error = %v, want cached upstream error", upstreamErr)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}

	currentTime = currentTime.Add(accountQuotaErrorCacheTTL + time.Second)
	if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != expectedError {
		t.Fatalf("error = %v, want upstream error", upstreamErr)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads after error TTL = %d, want 2", loads.Load())
	}
}

func TestAccountQuotaCacheInvalidation(t *testing.T) {
	cache := newAccountQuotaCache()
	var loads atomic.Int32
	loader := func() (json.RawMessage, *upstreamAPIError) {
		loads.Add(1)
		return json.RawMessage(`{}`), nil
	}

	if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	cache.invalidate(14)
	if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if loads.Load() != 2 {
		t.Fatalf("loads = %d, want 2", loads.Load())
	}
}

func TestAccountQuotaCacheCoalescesConcurrentRequests(t *testing.T) {
	cache := newAccountQuotaCache()
	var loads atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	loader := func() (json.RawMessage, *upstreamAPIError) {
		if loads.Add(1) == 1 {
			close(started)
		}
		<-release
		return json.RawMessage(`{"ok":true}`), nil
	}

	const callers = 8
	var waitGroup sync.WaitGroup
	waitGroup.Add(callers)
	for range callers {
		go func() {
			defer waitGroup.Done()
			if _, upstreamErr := cache.load(context.Background(), 14, loader); upstreamErr != nil {
				t.Errorf("load error: %v", upstreamErr)
			}
		}()
	}
	<-started
	close(release)
	waitGroup.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestAccountQuotaCacheInvalidationDoesNotRestoreStaleFlight(t *testing.T) {
	cache := newAccountQuotaCache()
	var loads atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})
	loader := func() (json.RawMessage, *upstreamAPIError) {
		if loads.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			return json.RawMessage(`{"version":"old"}`), nil
		}
		return json.RawMessage(`{"version":"new"}`), nil
	}

	go func() {
		defer close(firstDone)
		_, _ = cache.load(context.Background(), 14, loader)
	}()
	<-firstStarted
	cache.invalidate(14)
	data, upstreamErr := cache.load(context.Background(), 14, loader)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if string(data) != `{"version":"new"}` {
		t.Fatalf("data = %s, want new version", data)
	}
	close(releaseFirst)
	<-firstDone

	data, upstreamErr = cache.load(context.Background(), 14, loader)
	if upstreamErr != nil {
		t.Fatal(upstreamErr)
	}
	if string(data) != `{"version":"new"}` || loads.Load() != 2 {
		t.Fatalf("cached data = %s, loads = %d", data, loads.Load())
	}
}

func TestQueryOpenAIQuotaCacheOnlyRunsWithAutoReset(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		autoReset    bool
		wantRequests int32
	}{
		{name: "disabled stays real time", wantRequests: 2},
		{name: "enabled shares cache", autoReset: true, wantRequests: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/v1/admin/openai/accounts/14/quota" {
					t.Fatalf("unexpected upstream path %s", request.URL.Path)
				}
				requests.Add(1)
				writeUpstream(
					response,
					http.StatusOK,
					`{"code":0,"message":"success","data":{"rate_limit_reset_credits":{"available_count":1}}}`,
				)
			}))
			defer upstream.Close()

			cfg := testConfig(t, upstream.URL)
			cfg.autoResetCredits = testCase.autoReset
			application := newApp(cfg)
			for range 2 {
				var quota autoResetQuota
				if upstreamErr := application.queryOpenAIQuota(context.Background(), 14, &quota); upstreamErr != nil {
					t.Fatal(upstreamErr)
				}
			}
			if requests.Load() != testCase.wantRequests {
				t.Fatalf("requests = %d, want %d", requests.Load(), testCase.wantRequests)
			}
		})
	}
}
