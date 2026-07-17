package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

//go:embed web/*
var webFiles embed.FS

type app struct {
	config config
	client *http.Client
	auth   sub2APIAuthState
}

func newApp(cfg config) *app {
	application := &app{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.requestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if cfg.sub2APIAdminEmail == "" {
		application.auth.accessToken = cfg.sub2APIStaticToken
	}
	return application
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/assets/app.css", a.handleAsset("web/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/assets/app.js", a.handleAsset("web/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/", a.handleProtected)
	return mux
}

func (a *app) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(`{"status":"ok"}`))
}

func (a *app) handleAsset(name, contentType string) http.HandlerFunc {
	content, err := webFiles.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			methodNotAllowed(response, http.MethodGet)
			return
		}
		response.Header().Set("Content-Type", contentType)
		response.Header().Set("Cache-Control", "public, max-age=3600")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = response.Write(content)
	}
}

func (a *app) handleProtected(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response)

	segments, err := escapedPathSegments(request.URL.EscapedPath())
	if err != nil || (len(segments) != 2 && len(segments) != 4) {
		http.NotFound(response, request)
		return
	}
	if subtle.ConstantTimeCompare([]byte(segments[0]), []byte(a.config.accessToken)) != 1 {
		http.NotFound(response, request)
		return
	}

	accountID, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || accountID <= 0 {
		http.NotFound(response, request)
		return
	}
	if _, allowed := a.config.accountIDs[accountID]; !allowed {
		http.NotFound(response, request)
		return
	}

	if len(segments) == 2 {
		a.handlePage(response, request)
		return
	}
	if segments[2] != "api" {
		http.NotFound(response, request)
		return
	}

	switch segments[3] {
	case "config":
		a.handlePublicConfig(response, request, accountID)
	case "usage":
		a.handleUsage(response, request, accountID)
	case "quota":
		a.handleQuota(response, request, accountID)
	case "reset":
		a.handleReset(response, request, accountID)
	default:
		http.NotFound(response, request)
	}
}

func escapedPathSegments(escapedPath string) ([]string, error) {
	parts := strings.Split(strings.Trim(escapedPath, "/"), "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, err
		}
		segments = append(segments, decoded)
	}
	return segments, nil
}

func (a *app) handlePage(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	content, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		http.Error(response, "page unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	_, _ = response.Write(content)
}

func (a *app) handlePublicConfig(response http.ResponseWriter, request *http.Request, accountID int64) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	writeJSON(response, http.StatusOK, fmt.Sprintf(`{"account_id":%d,"allow_reset":%t}`, accountID, a.config.allowReset))
}

func (a *app) handleUsage(response http.ResponseWriter, request *http.Request, accountID int64) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	query := url.Values{}
	if request.URL.Query().Get("active") == "1" {
		query.Set("source", "active")
		query.Set("force", "true")
	}
	a.proxyUpstream(response, request, http.MethodGet, fmt.Sprintf("/admin/accounts/%d/usage", accountID), query)
}

func (a *app) handleQuota(response http.ResponseWriter, request *http.Request, accountID int64) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	a.proxyUpstream(response, request, http.MethodGet, fmt.Sprintf("/admin/openai/accounts/%d/quota", accountID), nil)
}

func (a *app) handleReset(response http.ResponseWriter, request *http.Request, accountID int64) {
	if request.Method != http.MethodPost {
		methodNotAllowed(response, http.MethodPost)
		return
	}
	if !a.config.allowReset {
		writeJSON(response, http.StatusForbidden, `{"code":403,"message":"Reset is disabled by server configuration"}`)
		return
	}
	a.proxyUpstream(response, request, http.MethodPost, fmt.Sprintf("/admin/openai/accounts/%d/reset-quota", accountID), nil)
}

func (a *app) proxyUpstream(response http.ResponseWriter, request *http.Request, method, path string, query url.Values) {
	target := *a.config.sub2APIURL
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery = query.Encode()

	authToken, err := a.getSub2APIAccessToken(request.Context())
	if err != nil {
		log.Printf("Sub2API authentication failed: %v", err)
		writeAPIError(response, http.StatusBadGateway, "Sub2API authentication failed: "+err.Error())
		return
	}

	upstreamResponse, err := a.doSub2APIRequest(request.Context(), method, target.String(), authToken)
	if err != nil {
		log.Printf("upstream request failed for account endpoint %s: %v", path, err)
		writeJSON(response, http.StatusBadGateway, `{"code":502,"message":"Unable to reach Sub2API"}`)
		return
	}
	if upstreamResponse.StatusCode == http.StatusUnauthorized && a.usesPasswordAuthentication() {
		_ = upstreamResponse.Body.Close()
		authToken, err = a.renewSub2APIAccessToken(request.Context(), authToken)
		if err != nil {
			log.Printf("Sub2API reauthentication failed: %v", err)
			writeAPIError(response, http.StatusBadGateway, "Sub2API reauthentication failed: "+err.Error())
			return
		}
		upstreamResponse, err = a.doSub2APIRequest(request.Context(), method, target.String(), authToken)
		if err != nil {
			log.Printf("upstream retry failed for account endpoint %s: %v", path, err)
			writeJSON(response, http.StatusBadGateway, `{"code":502,"message":"Unable to reach Sub2API"}`)
			return
		}
	}
	defer upstreamResponse.Body.Close()

	body, err := io.ReadAll(io.LimitReader(upstreamResponse.Body, maxUpstreamBody+1))
	if err != nil {
		writeJSON(response, http.StatusBadGateway, `{"code":502,"message":"Unable to read Sub2API response"}`)
		return
	}
	if len(body) > maxUpstreamBody {
		writeJSON(response, http.StatusBadGateway, `{"code":502,"message":"Sub2API response is too large"}`)
		return
	}

	contentType := upstreamResponse.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(upstreamResponse.StatusCode)
	_, _ = response.Write(body)
}

func setSecurityHeaders(response http.ResponseWriter) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Frame-Options", "DENY")
}

func methodNotAllowed(response http.ResponseWriter, allowed string) {
	response.Header().Set("Allow", allowed)
	http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(body))
}

func writeAPIError(response http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]any{
		"code":    status,
		"message": message,
	})
	if err != nil {
		writeJSON(response, status, `{"code":500,"message":"Unable to encode error response"}`)
		return
	}
	writeJSON(response, status, string(body))
}
