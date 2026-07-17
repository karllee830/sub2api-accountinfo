package main

import (
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

//go:embed web/*
var webFiles embed.FS

type app struct {
	config config
	client *http.Client
}

func newApp(cfg config) *app {
	return &app{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.requestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/assets/app.css", a.handleAsset("web/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/assets/app.js", a.handleAsset("web/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/api/dashboard", a.handleDashboard)
	mux.HandleFunc("/api/accounts/", a.handleAccountAPI)
	mux.HandleFunc("/", a.handlePage)
	return mux
}

func (a *app) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	writeJSON(response, http.StatusOK, apiResponse{Code: 0, Message: "success", Data: map[string]string{"status": "ok"}})
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
		response.Header().Set("Cache-Control", "no-cache")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = response.Write(content)
	}
}

func (a *app) handlePage(response http.ResponseWriter, request *http.Request) {
	a.setSecurityHeaders(response)
	if request.URL.Path != "/" {
		http.NotFound(response, request)
		return
	}
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
	_, _ = response.Write(content)
}

func (a *app) handleDashboard(response http.ResponseWriter, request *http.Request) {
	a.setSecurityHeaders(response)
	if request.Method != http.MethodGet {
		methodNotAllowed(response, http.MethodGet)
		return
	}
	userID, requestErr := a.authenticateUser(request)
	if requestErr != nil {
		writeRequestError(response, requestErr)
		return
	}
	dashboard, requestErr := a.loadDashboard(request.Context(), userID, request.URL.Query().Get("active") == "1")
	if requestErr != nil {
		writeRequestError(response, requestErr)
		return
	}
	writeJSON(response, http.StatusOK, apiResponse{Code: 0, Message: "success", Data: dashboard})
}

func (a *app) handleAccountAPI(response http.ResponseWriter, request *http.Request) {
	a.setSecurityHeaders(response)
	accountID, action, ok := parseAccountAPIPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}

	userID, requestErr := a.authenticateUser(request)
	if requestErr != nil {
		writeRequestError(response, requestErr)
		return
	}
	allowed, requestErr := a.userCanAccessAccount(request.Context(), userID, accountID)
	if requestErr != nil {
		writeRequestError(response, requestErr)
		return
	}
	if !allowed {
		writeJSON(response, http.StatusForbidden, apiResponse{Code: 403, Message: "账号不属于当前用户的有效订阅分组"})
		return
	}

	switch action {
	case "quota":
		if request.Method != http.MethodGet {
			methodNotAllowed(response, http.MethodGet)
			return
		}
		var data json.RawMessage
		if upstreamErr := a.doAdminRequest(request.Context(), http.MethodGet, "/admin/openai/accounts/"+strconv.FormatInt(accountID, 10)+"/quota", nil, &data); upstreamErr != nil {
			writeUpstreamError(response, upstreamErr)
			return
		}
		writeJSON(response, http.StatusOK, apiResponse{Code: 0, Message: "success", Data: data})
	case "reset":
		if request.Method != http.MethodPost {
			methodNotAllowed(response, http.MethodPost)
			return
		}
		if !a.config.allowReset {
			writeJSON(response, http.StatusForbidden, apiResponse{Code: 403, Message: "服务端未开启重置功能"})
			return
		}
		var data json.RawMessage
		if upstreamErr := a.doAdminRequest(request.Context(), http.MethodPost, "/admin/openai/accounts/"+strconv.FormatInt(accountID, 10)+"/reset-quota", nil, &data); upstreamErr != nil {
			writeUpstreamError(response, upstreamErr)
			return
		}
		writeJSON(response, http.StatusOK, apiResponse{Code: 0, Message: "success", Data: data})
	default:
		http.NotFound(response, request)
	}
}

func parseAccountAPIPath(path string) (int64, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "accounts" {
		return 0, "", false
	}
	accountID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || accountID <= 0 {
		return 0, "", false
	}
	if parts[3] != "quota" && parts[3] != "reset" {
		return 0, "", false
	}
	return accountID, parts[3], true
}

func (a *app) setSecurityHeaders(response http.ResponseWriter) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors "+a.config.frameAncestors)
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Content-Type-Options", "nosniff")
}

func methodNotAllowed(response http.ResponseWriter, allowed string) {
	response.Header().Set("Allow", allowed)
	writeJSON(response, http.StatusMethodNotAllowed, apiResponse{Code: 405, Message: "method not allowed"})
}

type apiResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeJSON(response http.ResponseWriter, status int, body apiResponse) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeRequestError(response http.ResponseWriter, requestErr *requestError) {
	writeJSON(response, requestErr.Status, apiResponse{Code: requestErr.Code, Message: requestErr.Message})
}

func writeUpstreamError(response http.ResponseWriter, upstreamErr *upstreamAPIError) {
	status := http.StatusBadGateway
	if upstreamErr.Status >= 400 && upstreamErr.Status < 500 {
		status = upstreamErr.Status
	}
	code := status
	if errors.Is(upstreamErr, errSub2APIUnauthorized) {
		code = http.StatusUnauthorized
	}
	writeJSON(response, status, apiResponse{Code: code, Message: upstreamErr.Message})
}
