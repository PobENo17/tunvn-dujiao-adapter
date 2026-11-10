package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr         string
	TunvnBaseURL       string
	TunvnAPIKey        string
	TunvnCurrency      string
	AdapterAPIKey      string
	AdapterAPISecret   string
	DefaultCallbackURL string
	DataFile           string
	SiteName           string
	ProductsTTL        time.Duration
	BuyTimeout         time.Duration
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:         env("LISTEN_ADDR", ":8080"),
		TunvnBaseURL:       strings.TrimRight(env("TUNVN_BASE_URL", "https://tunvnmmo.duckdns.org"), "/"),
		TunvnAPIKey:        strings.TrimSpace(os.Getenv("TUNVN_API_KEY")),
		TunvnCurrency:      strings.ToLower(env("TUNVN_CURRENCY", "usdt")),
		AdapterAPIKey:      strings.TrimSpace(os.Getenv("ADAPTER_API_KEY")),
		AdapterAPISecret:   strings.TrimSpace(os.Getenv("ADAPTER_API_SECRET")),
		DefaultCallbackURL: strings.TrimSpace(os.Getenv("DEFAULT_CALLBACK_URL")),
		DataFile:           env("DATA_FILE", "data/orders.json"),
		SiteName:           env("SITE_NAME", "TUNVN Adapter"),
		ProductsTTL:        time.Duration(envInt("PRODUCTS_TTL_SECONDS", 60)) * time.Second,
		BuyTimeout:         time.Duration(envInt("BUY_TIMEOUT_SECONDS", 90)) * time.Second,
	}

	if cfg.TunvnAPIKey == "" {
		return nil, errors.New("TUNVN_API_KEY is required")
	}
	if cfg.AdapterAPIKey == "" {
		return nil, errors.New("ADAPTER_API_KEY is required")
	}
	if cfg.AdapterAPISecret == "" {
		return nil, errors.New("ADAPTER_API_SECRET is required")
	}
	if cfg.TunvnCurrency != "usdt" && cfg.TunvnCurrency != "vnd" {
		return nil, errors.New("TUNVN_CURRENCY must be usdt or vnd")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
