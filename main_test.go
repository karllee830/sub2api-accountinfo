package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(t *testing.T, upstreamURL string) config {
	t.Helper()
	parsed, err := normalizeSub2APIURL(upstreamURL)
	if err != nil {
		t.Fatalf("normalizeSub2APIURL: %v", err)
	}
	return config{
		sub2APIURL:        parsed,
		adminAPIKey:       "admin-test-key",
		trustProxyHeaders: true,
		frameAncestors:    "https://sub2api.example",
		listenAddr:        defaultListenAddr,
		requestTimeout:    time.Second,
	}
}

func embeddedRequest(method, target string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer user-test-token")
	request.Header.Set(embeddedUserIDHeader, "2")
	request.Header.Set("X-Real-IP", "203.0.113.20")
	request.Header.Set("User-Agent", "embedded-browser/1.0")
	return request
}

func writeUpstream(response http.ResponseWriter, status int, data string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(data))
}

func TestDashboardAuthenticatesUserAndLoadsSubscribedAccountUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/auth/me":
			if request.Header.Get("Authorization") != "Bearer user-test-token" {
				t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
			}
			if request.Header.Get("x-api-key") != "" {
				t.Errorf("auth request must not include admin key")
			}
			if request.Header.Get("CF-Connecting-IP") != "203.0.113.20" {
				t.Errorf("forwarded client IP = %q", request.Header.Get("CF-Connecting-IP"))
			}
			if request.UserAgent() != "embedded-browser/1.0" {
				t.Errorf("forwarded user agent = %q", request.UserAgent())
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":2,"status":"active"}}`)
		case "/api/v1/admin/user-attributes":
			assertAdminRequest(t, request)
			if request.URL.Query().Get("enabled") != "true" {
				t.Errorf("enabled query = %q", request.URL.Query().Get("enabled"))
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"id":7,"key":"allow_reset","enabled":true}]}`)
		case "/api/v1/admin/users/2/attributes":
			assertAdminRequest(t, request)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"attribute_id":7,"value":"true"}]}`)
		case "/api/v1/admin/users/2/subscriptions":
			assertAdminRequest(t, request)
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"id":1,"user_id":2,"group_id":9,"status":"active","expires_at":null,"group":{"id":9,"name":"test","platform":"openai","status":"active","subscription_type":"subscription"}}]}`)
		case "/api/v1/admin/accounts":
			assertAdminRequest(t, request)
			if request.URL.Query().Get("group") != "9" {
				t.Errorf("group query = %q", request.URL.Query().Get("group"))
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"items":[{"id":14,"name":"account@example.com","platform":"openai","type":"oauth","status":"active","schedulable":true,"group_ids":[9],"credentials":{"access_token":"must-not-leak"}}],"total":1,"page":1,"page_size":100,"pages":1}}`)
		case "/api/v1/admin/accounts/14/usage":
			assertAdminRequest(t, request)
			if request.URL.Query().Get("source") != "active" || request.URL.Query().Get("force") != "true" {
				t.Errorf("usage query = %q", request.URL.RawQuery)
			}
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"updated_at":"2026-07-17T12:00:00Z","five_hour":{"utilization":25}}}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, embeddedRequest(http.MethodGet, "/api/dashboard?active=1"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Code int               `json:"code"`
		Data dashboardResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Code != 0 || result.Data.UserID != 2 || !result.Data.AllowReset || len(result.Data.Groups) != 1 {
		t.Fatalf("unexpected dashboard: %+v", result)
	}
	accounts := result.Data.Groups[0].Accounts
	if len(accounts) != 1 || accounts[0].ID != 14 || accounts[0].Type != "oauth" {
		t.Fatalf("unexpected accounts: %+v", accounts)
	}
	if !strings.Contains(string(accounts[0].Usage), `"utilization":25`) {
		t.Fatalf("unexpected usage: %s", accounts[0].Usage)
	}
	if strings.Contains(response.Body.String(), "must-not-leak") {
		t.Fatal("account credentials leaked to dashboard response")
	}
}

func TestDashboardUsesAllowResetUserAttribute(t *testing.T) {
	testCases := []struct {
		name              string
		includeDefinition bool
		attributeValue    string
		wantAllowReset    bool
	}{
		{name: "true", includeDefinition: true, attributeValue: "true", wantAllowReset: true},
		{name: "false", includeDefinition: true, attributeValue: "false"},
		{name: "invalid", includeDefinition: true, attributeValue: "enabled"},
		{name: "missing definition"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/auth/me":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":2}}`)
				case "/api/v1/admin/user-attributes":
					if testCase.includeDefinition {
						writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"id":7,"key":"allow_reset","enabled":true}]}`)
					} else {
						writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[]}`)
					}
				case "/api/v1/admin/users/2/attributes":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"attribute_id":7,"value":"`+testCase.attributeValue+`"}]}`)
				case "/api/v1/admin/users/2/subscriptions":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[]}`)
				default:
					t.Fatalf("unexpected upstream path %s", request.URL.Path)
				}
			}))
			defer upstream.Close()

			application := newApp(testConfig(t, upstream.URL))
			response := httptest.NewRecorder()
			application.routes().ServeHTTP(response, embeddedRequest(http.MethodGet, "/api/dashboard"))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var result struct {
				Data dashboardResponse `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Data.AllowReset != testCase.wantAllowReset {
				t.Fatalf("allow_reset = %t, want %t", result.Data.AllowReset, testCase.wantAllowReset)
			}
		})
	}
}

func TestDashboardAllowResetConfigBypassesUserAttributes(t *testing.T) {
	var attributeCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/auth/me":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":2}}`)
		case "/api/v1/admin/users/2/subscriptions":
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[]}`)
		case "/api/v1/admin/user-attributes", "/api/v1/admin/users/2/attributes":
			attributeCalls.Add(1)
			writeUpstream(response, http.StatusInternalServerError, `{"code":500,"message":"must not be called"}`)
		default:
			t.Fatalf("unexpected upstream path %s", request.URL.Path)
		}
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL)
	cfg.allowReset = true
	application := newApp(cfg)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, embeddedRequest(http.MethodGet, "/api/dashboard"))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Data dashboardResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Data.AllowReset {
		t.Fatal("ALLOW_RESET=true must allow resets")
	}
	if attributeCalls.Load() != 0 {
		t.Fatalf("user attribute calls = %d, want 0", attributeCalls.Load())
	}
}

func assertAdminRequest(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Header.Get("x-api-key") != "admin-test-key" {
		t.Errorf("x-api-key = %q", request.Header.Get("x-api-key"))
	}
	if request.Header.Get("Authorization") != "" {
		t.Errorf("admin request must not include user Authorization")
	}
	if request.Header.Get("X-Admin-UI-Request") != "1" {
		t.Errorf("X-Admin-UI-Request = %q", request.Header.Get("X-Admin-UI-Request"))
	}
}

func TestDashboardRejectsMismatchedUserIDBeforeAdminLookup(t *testing.T) {
	var adminCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/auth/me" {
			writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":2}}`)
			return
		}
		adminCalls.Add(1)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	request := embeddedRequest(http.MethodGet, "/api/dashboard")
	request.Header.Set(embeddedUserIDHeader, "3")
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if adminCalls.Load() != 0 {
		t.Fatalf("admin calls = %d, want 0", adminCalls.Load())
	}
}

func TestDashboardRejectsInvalidToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeUpstream(response, http.StatusUnauthorized, `{"code":"TOKEN_REVOKED","message":"revoked"}`)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, embeddedRequest(http.MethodGet, "/api/dashboard"))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestResetRequiresSubscribedAccountAndUserAttribute(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		allowReset     bool
		attributeValue string
		accountID      string
		wantStatus     int
		wantReset      int32
	}{
		{name: "disabled", attributeValue: "false", accountID: "14", wantStatus: http.StatusForbidden},
		{name: "not subscribed", attributeValue: "true", accountID: "99", wantStatus: http.StatusForbidden},
		{name: "enabled", attributeValue: "true", accountID: "14", wantStatus: http.StatusOK, wantReset: 1},
		{name: "global override", allowReset: true, attributeValue: "false", accountID: "14", wantStatus: http.StatusOK, wantReset: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var resetCalls atomic.Int32
			var attributeCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/auth/me":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"id":2}}`)
				case "/api/v1/admin/users/2/subscriptions":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"id":1,"user_id":2,"group_id":9,"status":"active","group":{"id":9,"name":"test","platform":"openai","status":"active"}}]}`)
				case "/api/v1/admin/accounts":
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"items":[{"id":14,"name":"account","platform":"openai","type":"oauth","status":"active"}],"total":1,"page":1,"page_size":100,"pages":1}}`)
				case "/api/v1/admin/user-attributes":
					attributeCalls.Add(1)
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"id":7,"key":"allow_reset","enabled":true}]}`)
				case "/api/v1/admin/users/2/attributes":
					attributeCalls.Add(1)
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":[{"attribute_id":7,"value":"`+testCase.attributeValue+`"}]}`)
				case "/api/v1/admin/openai/accounts/14/reset-quota":
					resetCalls.Add(1)
					writeUpstream(response, http.StatusOK, `{"code":0,"message":"success","data":{"windows_reset":2}}`)
				default:
					t.Fatalf("unexpected upstream path %s", request.URL.Path)
				}
			}))
			defer upstream.Close()

			cfg := testConfig(t, upstream.URL)
			cfg.allowReset = testCase.allowReset
			application := newApp(cfg)
			response := httptest.NewRecorder()
			application.routes().ServeHTTP(response, embeddedRequest(http.MethodPost, "/api/accounts/"+testCase.accountID+"/reset"))
			if response.Code != testCase.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if resetCalls.Load() != testCase.wantReset {
				t.Fatalf("reset calls = %d, want %d", resetCalls.Load(), testCase.wantReset)
			}
			if testCase.allowReset && attributeCalls.Load() != 0 {
				t.Fatalf("user attribute calls = %d, want 0", attributeCalls.Load())
			}
		})
	}
}

func TestPageAllowsConfiguredFrameAncestor(t *testing.T) {
	application := newApp(testConfig(t, "https://sub2api.example"))
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://sub2api.example") {
		t.Fatalf("CSP = %q", csp)
	}
	if response.Header().Get("X-Frame-Options") != "" {
		t.Fatalf("X-Frame-Options must be omitted for embedded page")
	}
}

func TestResetUsesCustomConfirmationDialog(t *testing.T) {
	script, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "window.confirm") {
		t.Fatal("reset must not use the browser confirmation dialog")
	}
	if !strings.Contains(string(script), "重置次数非常珍贵，务必确认好再重置") ||
		!strings.Contains(string(script), "confirmResetFinal") {
		t.Fatal("reset must require the final custom confirmation dialog")
	}

	page, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"confirm-modal", "confirm-modal-cancel", "confirm-modal-accept"} {
		if !strings.Contains(string(page), `id="`+id+`"`) {
			t.Fatalf("missing custom confirmation element %q", id)
		}
	}
}

func TestQuotaPanelShowsEveryCreditExpiration(t *testing.T) {
	script, err := webFiles.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	content := string(script)
	for _, text := range []string{
		"查看重置额度",
		"剩余次数与全部到期时间",
		"credits.forEach",
		"formatFullDate(credit.expiresAt)",
		"按当前设备时区显示",
	} {
		if !strings.Contains(content, text) {
			t.Fatalf("quota panel is missing %q", text)
		}
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
			t.Errorf("normalizeSub2APIURL(%q) = %q, want %q", testCase.input, got.String(), testCase.want)
		}
	}
}

func TestNormalizeFrameAncestors(t *testing.T) {
	upstream, _ := url.Parse("https://sub2api.example/api/v1")
	value, err := normalizeFrameAncestors("", upstream)
	if err != nil || value != "https://sub2api.example" {
		t.Fatalf("value = %q, err = %v", value, err)
	}
	if _, err := normalizeFrameAncestors("*; script-src *", upstream); err == nil {
		t.Fatal("expected injected CSP directive to be rejected")
	}
}

func TestHealth(t *testing.T) {
	application := newApp(testConfig(t, "https://sub2api.example"))
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ok"`) {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
}

func TestSub2APIClientDoesNotFollowRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "https://example.com")
		response.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	application := newApp(testConfig(t, upstream.URL))
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, embeddedRequest(http.MethodGet, "/api/dashboard"))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
}

func TestSub2APIURLRejectsQueries(t *testing.T) {
	_, err := normalizeSub2APIURL((&url.URL{Scheme: "https", Host: "example.com", RawQuery: "token=bad"}).String())
	if err == nil {
		t.Fatal("expected query-bearing URL to be rejected")
	}
}
