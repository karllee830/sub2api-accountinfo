package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(t *testing.T, upstreamURL string, allowReset bool) config {
	t.Helper()
	parsed, err := normalizeSub2APIURL(upstreamURL)
	if err != nil {
		t.Fatalf("normalizeSub2APIURL: %v", err)
	}
	return config{
		accessToken:    "viewer-secret",
		accountIDs:     map[int64]struct{}{42: {}},
		sub2APIURL:     parsed,
		adminAPIKey:    "admin-test-key",
		allowReset:     allowReset,
		listenAddr:     defaultListenAddr,
		requestTimeout: time.Second,
	}
}

func TestProtectedPageRequiresTokenAndAllowedAccount(t *testing.T) {
	application := newApp(testConfig(t, "https://sub2api.example", false))
	handler := application.routes()

	for _, testCase := range []struct {
		path   string
		status int
	}{
		{path: "/viewer-secret/42", status: http.StatusOK},
		{path: "/wrong/42", status: http.StatusNotFound},
		{path: "/viewer-secret/43", status: http.StatusNotFound},
		{path: "/", status: http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != testCase.status {
			t.Errorf("GET %s status = %d, want %d", testCase.path, response.Code, testCase.status)
		}
	}
}

func TestActiveUsageProxiesExpectedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/admin/accounts/42/usage" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("source") != "active" || request.URL.Query().Get("force") != "true" {
			t.Errorf("query = %q", request.URL.RawQuery)
		}
		if request.Header.Get("x-api-key") != "admin-test-key" {
			t.Errorf("x-api-key = %q", request.Header.Get("x-api-key"))
		}
		if request.Header.Get("Authorization") != "" {
			t.Errorf("Authorization must be empty, got %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("X-Admin-UI-Request") != "1" {
			t.Errorf("X-Admin-UI-Request = %q", request.Header.Get("X-Admin-UI-Request"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"code":0,"message":"success","data":{"five_hour":{"utilization":25}}}`))
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL, false))
	request := httptest.NewRequest(http.MethodGet, "/viewer-secret/42/api/usage?active=1", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"utilization":25`) {
		t.Fatalf("unexpected body: %s", response.Body.String())
	}
}

func TestResetDisabledDoesNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL, false))
	request := httptest.NewRequest(http.MethodPost, "/viewer-secret/42/api/reset", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestResetEnabledProxiesExpectedRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %q", request.Method)
		}
		if request.URL.Path != "/api/v1/admin/openai/accounts/42/reset-quota" {
			t.Errorf("path = %q", request.URL.Path)
		}
		_, _ = response.Write([]byte(`{"code":0,"message":"success","data":{"windows_reset":2}}`))
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL, true))
	request := httptest.NewRequest(http.MethodPost, "/viewer-secret/42/api/reset", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestNormalizeSub2APIURL(t *testing.T) {
	testCases := []struct {
		input string
		want  string
	}{
		{input: "https://example.com", want: "https://example.com/api/v1"},
		{input: "https://example.com/sub2api/", want: "https://example.com/sub2api/api/v1"},
		{input: "https://example.com/api/v1", want: "https://example.com/api/v1"},
	}
	for _, testCase := range testCases {
		got, err := normalizeSub2APIURL(testCase.input)
		if err != nil {
			t.Fatalf("normalizeSub2APIURL(%q): %v", testCase.input, err)
		}
		if got.String() != testCase.want {
			t.Errorf("normalizeSub2APIURL(%q) = %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

func TestEscapedAccessToken(t *testing.T) {
	segments, err := escapedPathSegments("/token%2Fpart/42")
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0] != "token/part" {
		t.Fatalf("segments = %#v", segments)
	}
}

func TestHealth(t *testing.T) {
	application := newApp(testConfig(t, "https://sub2api.example", false))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	body, _ := io.ReadAll(response.Result().Body)
	if response.Code != http.StatusOK || string(body) != `{"status":"ok"}` {
		t.Fatalf("status = %d body = %q", response.Code, string(body))
	}
}

func TestProxyDoesNotFollowRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "https://example.com")
		response.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL, false))
	request := httptest.NewRequest(http.MethodGet, "/viewer-secret/42/api/quota", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want redirect response from upstream", response.Code)
	}
}

func TestSub2APIURLRejectsQueries(t *testing.T) {
	_, err := normalizeSub2APIURL((&url.URL{Scheme: "https", Host: "example.com", RawQuery: "token=bad"}).String())
	if err == nil {
		t.Fatal("expected query-bearing URL to be rejected")
	}
}
