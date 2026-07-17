package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr = ":8080"
	maxUpstreamBody   = 2 << 20
)

type config struct {
	sub2APIURL        *url.URL
	adminAPIKey       string
	allowReset        bool
	trustProxyHeaders bool
	frameAncestors    string
	listenAddr        string
	requestTimeout    time.Duration
}

func loadConfig() (config, error) {
	sub2APIURL, err := normalizeSub2APIURL(os.Getenv("SUB2API_URL"))
	if err != nil {
		return config{}, err
	}

	adminAPIKey := strings.TrimSpace(os.Getenv("SUB2API_ADMIN_API_KEY"))
	if adminAPIKey == "" {
		return config{}, errors.New("SUB2API_ADMIN_API_KEY is required")
	}

	allowReset, err := parseBoolEnv("ALLOW_RESET", false)
	if err != nil {
		return config{}, err
	}
	trustProxyHeaders, err := parseBoolEnv("TRUST_PROXY_HEADERS", true)
	if err != nil {
		return config{}, err
	}
	frameAncestors, err := normalizeFrameAncestors(os.Getenv("FRAME_ANCESTORS"), sub2APIURL)
	if err != nil {
		return config{}, err
	}

	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	return config{
		sub2APIURL:        sub2APIURL,
		adminAPIKey:       adminAPIKey,
		allowReset:        allowReset,
		trustProxyHeaders: trustProxyHeaders,
		frameAncestors:    frameAncestors,
		listenAddr:        listenAddr,
		requestTimeout:    30 * time.Second,
	}, nil
}

func normalizeSub2APIURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("SUB2API_URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("SUB2API_URL is invalid: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("SUB2API_URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("SUB2API_URL must include a host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("SUB2API_URL must not contain a query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/api/v1") {
		parsed.Path += "/api/v1"
	}
	return parsed, nil
}

func normalizeFrameAncestors(raw string, sub2APIURL *url.URL) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = sub2APIURL.Scheme + "://" + sub2APIURL.Host
	}
	if strings.ContainsAny(value, ";\r\n") {
		return "", errors.New("FRAME_ANCESTORS must be a CSP source list without semicolons")
	}
	return value, nil
}

func parseBoolEnv(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}
