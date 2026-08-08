package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func validTokenHiveConfigForTest() TokenHiveConfig {
	return TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		Accounts:      map[int64]string{42: "openai_codex_oauth"},
	}
}

func TestTokenHiveConfigRejectsInvalidRegistry(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TokenHiveConfig)
	}{
		{name: "missing account mapping", mutate: func(cfg *TokenHiveConfig) { cfg.Accounts = nil }},
		{name: "empty account mapping", mutate: func(cfg *TokenHiveConfig) { cfg.Accounts = map[int64]string{} }},
		{name: "non-positive account ID", mutate: func(cfg *TokenHiveConfig) { cfg.Accounts = map[int64]string{0: "openai_codex_oauth"} }},
		{name: "empty upstream type", mutate: func(cfg *TokenHiveConfig) { cfg.Accounts = map[int64]string{42: ""} }},
		{name: "duplicate upstream type", mutate: func(cfg *TokenHiveConfig) {
			cfg.Accounts = map[int64]string{42: "openai_codex_oauth", 43: "openai_codex_oauth"}
		}},
		{name: "capability used as upstream type", mutate: func(cfg *TokenHiveConfig) {
			cfg.Accounts = map[int64]string{42: "openai.codex.responses.http"}
		}},
		{name: "unknown upstream type", mutate: func(cfg *TokenHiveConfig) { cfg.Accounts = map[int64]string{42: "other"} }},
		{name: "non-loopback URL", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL = "http://example.com/internal/v1/proxy" }},
		{name: "alternate IPv4 loopback", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL = "http://127.0.0.2:18081/internal/v1/proxy" }},
		{name: "IPv6 loopback", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL = "http://[::1]:18081/internal/v1/proxy" }},
		{name: "HTTPS URL", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL = "https://127.0.0.1:18081/internal/v1/proxy" }},
		{name: "wrong path", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL = "http://127.0.0.1:18081/v1/responses" }},
		{name: "URL query", mutate: func(cfg *TokenHiveConfig) { cfg.ProxyURL += "?forged=true" }},
		{name: "invalid encoded key", mutate: func(cfg *TokenHiveConfig) { cfg.TenantHMACKey = "not base64!" }},
		{name: "decoded key too short", mutate: func(cfg *TokenHiveConfig) {
			cfg.TenantHMACKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 31)))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTokenHiveConfigForTest()
			tt.mutate(&cfg)
			if err := validateTokenHiveConfig(cfg); err == nil {
				t.Fatal("validateTokenHiveConfig() error = nil")
			}
		})
	}
}

func TestTokenHiveConfigAcceptsSingleCodexMapping(t *testing.T) {
	cfg := validTokenHiveConfigForTest()
	if err := validateTokenHiveConfig(cfg); err != nil {
		t.Fatalf("validateTokenHiveConfig() error = %v", err)
	}
}
