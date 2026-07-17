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
	accessToken          string
	accountIDs           map[int64]struct{}
	sub2APIURL           *url.URL
	sub2APIAdminEmail    string
	sub2APIAdminPassword string
	sub2APIStaticToken   string
	allowReset           bool
	listenAddr           string
	requestTimeout       time.Duration
}

func loadConfig() (config, error) {
	accessToken := strings.TrimSpace(os.Getenv("ACCESS_TOKEN"))
	if accessToken == "" {
		return config{}, errors.New("ACCESS_TOKEN is required")
	}

	accountIDs, err := parseAccountIDs(os.Getenv("ACCOUNT_IDS"))
	if err != nil {
		return config{}, err
	}

	sub2APIURL, err := normalizeSub2APIURL(os.Getenv("SUB2API_URL"))
	if err != nil {
		return config{}, err
	}

	sub2APIAdminEmail := strings.TrimSpace(os.Getenv("SUB2API_ADMIN_EMAIL"))
	sub2APIAdminPassword := os.Getenv("SUB2API_ADMIN_PASSWORD")
	sub2APIStaticToken := strings.TrimSpace(os.Getenv("SUB2API_AUTH_TOKEN"))
	if (sub2APIAdminEmail == "") != (sub2APIAdminPassword == "") {
		return config{}, errors.New("SUB2API_ADMIN_EMAIL and SUB2API_ADMIN_PASSWORD must be configured together")
	}
	if sub2APIAdminEmail == "" && sub2APIStaticToken == "" {
		return config{}, errors.New("SUB2API_ADMIN_EMAIL and SUB2API_ADMIN_PASSWORD are required (or use SUB2API_AUTH_TOKEN for compatibility)")
	}

	allowReset, err := parseBoolEnv("ALLOW_RESET", false)
	if err != nil {
		return config{}, err
	}

	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}

	return config{
		accessToken:          accessToken,
		accountIDs:           accountIDs,
		sub2APIURL:           sub2APIURL,
		sub2APIAdminEmail:    sub2APIAdminEmail,
		sub2APIAdminPassword: sub2APIAdminPassword,
		sub2APIStaticToken:   sub2APIStaticToken,
		allowReset:           allowReset,
		listenAddr:           listenAddr,
		requestTimeout:       30 * time.Second,
	}, nil
}

func parseAccountIDs(raw string) (map[int64]struct{}, error) {
	values := strings.Split(raw, ",")
	result := make(map[int64]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		accountID, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || accountID <= 0 {
			return nil, fmt.Errorf("ACCOUNT_IDS contains invalid account ID %q", trimmed)
		}
		result[accountID] = struct{}{}
	}
	if len(result) == 0 {
		return nil, errors.New("ACCOUNT_IDS must contain at least one positive account ID")
	}
	return result, nil
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
