package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	UpstreamTypeOpenAICodexOAuth       = "openai_codex_oauth"
	CapabilityOpenAICodexResponsesHTTP = "openai.codex.responses.http"
	TokenHiveHeaderRequestID           = "X-TokenHive-Request-ID"
	TokenHiveHeaderRawURL              = "X-TokenHive-Raw-URL"
	TokenHiveHeaderUpstreamType        = "X-TokenHive-Upstream-Type"
	TokenHiveHeaderUpstreamModel       = "X-TokenHive-Upstream-Model"
	TokenHiveHeaderTenantKey           = "X-TokenHive-Tenant-Key"
	tokenHiveHeaderPrefix              = "x-tokenhive-"
	tokenHiveTenantKeyMessagePrefix    = "tokenhive:tenant-key:v1\x00api-key-record-id\x00"
)

type TokenHiveAccount struct {
	AccountID    int64
	UpstreamType string
}

type TokenHiveRegistry struct {
	accounts map[int64]TokenHiveAccount
}

func NewTokenHiveRegistry(cfg config.TokenHiveConfig, accounts []Account) (*TokenHiveRegistry, error) {
	if err := config.ValidateTokenHiveConfig(cfg); err != nil {
		return nil, err
	}
	registry := &TokenHiveRegistry{accounts: make(map[int64]TokenHiveAccount)}
	if !cfg.Enabled {
		return registry, nil
	}
	accountsByID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		accountsByID[accounts[i].ID] = &accounts[i]
	}
	for accountID, upstreamType := range cfg.Accounts {
		account := accountsByID[accountID]
		if account == nil {
			return nil, fmt.Errorf("tokenhive mapped account %d does not exist", accountID)
		}
		if account.Type != AccountTypeAPIKey {
			return nil, fmt.Errorf("tokenhive mapped account %d must have type %s", accountID, AccountTypeAPIKey)
		}
		if account.Platform != PlatformOpenAI {
			return nil, fmt.Errorf("tokenhive mapped account %d must have platform %s", accountID, PlatformOpenAI)
		}
		if upstreamType != UpstreamTypeOpenAICodexOAuth {
			return nil, fmt.Errorf("tokenhive mapped account %d has unsupported upstream type %q", accountID, upstreamType)
		}
		registry.accounts[accountID] = TokenHiveAccount{AccountID: accountID, UpstreamType: upstreamType}
	}
	return registry, nil
}

func (r *TokenHiveRegistry) Match(account *Account) (TokenHiveAccount, bool) {
	if r == nil || account == nil || account.Type != AccountTypeAPIKey || account.Platform != PlatformOpenAI {
		return TokenHiveAccount{}, false
	}
	mapped, ok := r.accounts[account.ID]
	return mapped, ok
}

func resolveTokenHiveAccount(cfg *config.Config, registry *TokenHiveRegistry, account *Account) (*TokenHiveAccount, error) {
	if cfg == nil || !cfg.TokenHive.Enabled || account == nil {
		return nil, nil
	}
	if registry == nil {
		if _, configured := cfg.TokenHive.Accounts[account.ID]; !configured {
			return nil, nil
		}
		var err error
		registry, err = NewTokenHiveRegistry(cfg.TokenHive, []Account{*account})
		if err != nil {
			return nil, err
		}
	}
	mapped, ok := registry.Match(account)
	if !ok {
		return nil, nil
	}
	return &mapped, nil
}

func BuildTokenHiveMetadata(ctx context.Context, apiKeyRecordID int64, rawURL string, upstreamModel string, account TokenHiveAccount, key []byte) (http.Header, error) {
	if apiKeyRecordID <= 0 {
		return nil, fmt.Errorf("tokenhive API key record ID must be positive")
	}
	if account.UpstreamType != UpstreamTypeOpenAICodexOAuth {
		return nil, fmt.Errorf("invalid tokenhive account mapping")
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("tokenhive tenant HMAC key must be at least 32 bytes")
	}
	parsedRawURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsedRawURL.Scheme == "" || parsedRawURL.Host == "" {
		return nil, fmt.Errorf("invalid tokenhive raw URL")
	}
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil, fmt.Errorf("tokenhive upstream model must not be empty")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(tokenHiveTenantKeyMessagePrefix + strconv.FormatInt(apiKeyRecordID, 10)))
	tenantKey := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	headers := make(http.Header, 5)
	headers.Set(TokenHiveHeaderRequestID, ResolveUsageBillingRequestID(ctx, ""))
	headers.Set(TokenHiveHeaderRawURL, parsedRawURL.String())
	headers.Set(TokenHiveHeaderUpstreamType, account.UpstreamType)
	headers.Set(TokenHiveHeaderUpstreamModel, upstreamModel)
	headers.Set(TokenHiveHeaderTenantKey, tenantKey)
	return headers, nil
}

func applyTokenHiveHandoff(ctx context.Context, cfg *config.Config, account *TokenHiveAccount, apiKeyRecordID int64, upstreamModel string, req *http.Request) error {
	if cfg == nil || !cfg.TokenHive.Enabled || account == nil || req == nil {
		return nil
	}
	key, err := config.DecodeTokenHiveTenantHMACKey(cfg.TokenHive.TenantHMACKey)
	if err != nil {
		return err
	}
	metadata, err := BuildTokenHiveMetadata(ctx, apiKeyRecordID, req.URL.String(), upstreamModel, *account, key)
	if err != nil {
		return err
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), tokenHiveHeaderPrefix) {
			delete(req.Header, name)
		}
	}
	for name, values := range metadata {
		req.Header[name] = append([]string(nil), values...)
	}
	proxyURL, err := url.Parse(strings.TrimSpace(cfg.TokenHive.ProxyURL))
	if err != nil {
		return fmt.Errorf("parse tokenhive proxy URL: %w", err)
	}
	req.URL = proxyURL
	req.Host = ""
	*req = *req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
	return nil
}

type TokenHiveResponsePolicy struct {
	Dedicated              bool
	AllowSameAccountRetry  bool
	AllowAccountFailover   bool
	AllowAccountMutation   bool
	AllowRuntimeBlock      bool
	AllowSchedulerFeedback bool
}

func ResolveTokenHiveResponsePolicy(account *Account, registry *TokenHiveRegistry) TokenHiveResponsePolicy {
	_, dedicated := registry.Match(account)
	return TokenHiveResponsePolicy{
		Dedicated:              dedicated,
		AllowSameAccountRetry:  !dedicated,
		AllowAccountFailover:   !dedicated,
		AllowAccountMutation:   !dedicated,
		AllowRuntimeBlock:      !dedicated,
		AllowSchedulerFeedback: !dedicated,
	}
}
