package service

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

func tokenHiveConfigForTest(accountID int64) config.TokenHiveConfig {
	return config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{accountID: UpstreamTypeOpenAICodexOAuth},
	}
}

func TestTokenHiveConfigRejectsInvalidRegistry(t *testing.T) {
	cfg := tokenHiveConfigForTest(42)
	tests := []struct {
		name     string
		accounts []Account
	}{
		{name: "mapped account missing"},
		{name: "mapped account is not API key", accounts: []Account{{ID: 42, Type: AccountTypeOAuth, Platform: PlatformOpenAI}}},
		{name: "mapped account is not OpenAI", accounts: []Account{{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformAnthropic}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTokenHiveRegistry(cfg, tt.accounts); err == nil {
				t.Fatal("NewTokenHiveRegistry() error = nil")
			}
		})
	}
}

func TestTokenHiveMetadataUsesUsageBillingRequestID(t *testing.T) {
	registry, err := NewTokenHiveRegistry(tokenHiveConfigForTest(42), []Account{{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}})
	if err != nil {
		t.Fatal(err)
	}
	account, ok := registry.Match(&Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI})
	if !ok {
		t.Fatal("registry did not match dedicated account")
	}
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "openai-client-stable-123")
	headers, err := BuildTokenHiveMetadata(ctx, 99, "https://api.openai.com/v1/responses", "gpt-5.4", account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	want := ResolveUsageBillingRequestID(ctx, "provider-id")
	if want != "client:openai-client-stable-123" {
		t.Fatalf("ResolveUsageBillingRequestID() = %q", want)
	}
	if got := headers.Get(TokenHiveHeaderRequestID); got != want {
		t.Fatalf("request ID header = %q, want %q", got, want)
	}
}

func TestTokenHiveTenantKeyVector(t *testing.T) {
	registry, err := NewTokenHiveRegistry(tokenHiveConfigForTest(42), []Account{{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}})
	if err != nil {
		t.Fatal(err)
	}
	account, ok := registry.Match(&Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI})
	if !ok {
		t.Fatal("registry did not match dedicated account")
	}
	headers, err := BuildTokenHiveMetadata(context.Background(), 42, "https://api.openai.com/v1/responses", "gpt-5.4", account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := headers.Get(TokenHiveHeaderTenantKey), "kayWd6oTodmBiOOzoawG8lDZJIetGTxg6-OExYtrwvA"; got != want {
		t.Fatalf("tenant key = %q, want %q", got, want)
	}
}
