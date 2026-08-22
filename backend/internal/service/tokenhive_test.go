package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
)

func tokenHiveConfigForTest(accountID int64) config.TokenHiveConfig {
	return config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{accountID: UpstreamTypeOpenAICodexOAuth},
	}
}

func tokenHiveAnthropicConfigForTest(accountID int64) config.TokenHiveConfig {
	cfg := tokenHiveConfigForTest(accountID)
	cfg.Accounts[accountID] = UpstreamTypeAnthropicOAuth
	return cfg
}

func TestTokenHiveHandoffCarriesLogicalMethodAndForcesLoopbackPOST(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			cfg := &config.Config{TokenHive: tokenHiveConfigForTest(42)}
			mapped := &TokenHiveAccount{AccountID: 42, UpstreamType: UpstreamTypeOpenAICodexOAuth}
			body := []byte(`{"fixture":true}`)
			if method == http.MethodGet {
				body = nil
			}
			req := httptest.NewRequest(method, "https://api.openai.com/v1/fixture", bytes.NewReader(body))
			req.Header.Add("X-TokenHive-Method", "FORGED")
			if err := applyTokenHiveHandoff(context.Background(), cfg, mapped, 7, "fixture-model", SourceOperationOpenAIResponsesHTTP, req); err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || req.Header.Get("X-TokenHive-Method") != method || len(req.Header.Values("X-TokenHive-Method")) != 1 {
				t.Fatalf("transport_method=%q logical_method=%q method_values=%d", req.Method, req.Header.Get("X-TokenHive-Method"), len(req.Header.Values("X-TokenHive-Method")))
			}
		})
	}
}

func TestTokenHiveMetadataSourceOperationExactSet(t *testing.T) {
	want := map[string]struct{}{
		"anthropic.messages.create":       {},
		"anthropic.messages.stream":       {},
		"anthropic.messages.count_tokens": {},
		"openai.responses.http":           {},
		"openai.responses.compact":        {},
		"openai.responses.input_tokens":   {},
		"openai.images.generations":       {},
		"openai.images.edits":             {},
		"openai.alpha_search":             {},
		"openai.codex.models.manifest":    {},
	}
	if len(tokenHiveSourceOperations) != len(want) {
		t.Fatalf("source operation count = %d, want %d", len(tokenHiveSourceOperations), len(want))
	}
	for operation := range want {
		if _, ok := tokenHiveSourceOperations[operation]; !ok {
			t.Errorf("missing source operation %q", operation)
		}
	}
	for operation := range tokenHiveSourceOperations {
		if _, ok := want[operation]; !ok {
			t.Errorf("unexpected source operation %q", operation)
		}
	}

	account := TokenHiveAccount{AccountID: 42, UpstreamType: UpstreamTypeOpenAICodexOAuth}
	key := []byte("slice-1-test-tenant-hmac-key-32bytes")
	for operation := range want {
		t.Run(operation, func(t *testing.T) {
			headers, err := BuildTokenHiveMetadata(context.Background(), 7, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", operation, account, key)
			if err != nil {
				t.Fatal(err)
			}
			if got := headers.Values(TokenHiveHeaderSourceOperation); len(got) != 1 || got[0] != operation {
				t.Fatalf("source operation values = %q, want [%q]", got, operation)
			}
		})
	}

	for _, operation := range []string{"", " openai.responses.http", "openai.responses.http ", "OPENAI.RESPONSES.HTTP", "openai.responses"} {
		t.Run("reject_"+operation, func(t *testing.T) {
			if _, err := BuildTokenHiveMetadata(context.Background(), 7, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", operation, account, key); err == nil {
				t.Fatalf("BuildTokenHiveMetadata accepted invalid source operation %q", operation)
			}
		})
	}
}

func TestApplyTokenHiveHandoffStripsForgedHeaders(t *testing.T) {
	cfg := &config.Config{TokenHive: tokenHiveConfigForTest(42)}
	mapped := &TokenHiveAccount{AccountID: 42, UpstreamType: UpstreamTypeOpenAICodexOAuth}
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	forgedHeaders := http.Header{
		"X-TokenHive-Request-ID":          []string{"forged-request"},
		"x-tokenhive-raw-url":             []string{"forged-url"},
		"X-TOKENHIVE-UPSTREAM-TYPE":       []string{"forged-type"},
		"x-ToKeNhIvE-Upstream-Model":      []string{"forged-model"},
		"X-TokenHive-Tenant-Key":          []string{"forged-key"},
		"x-tokenhive-method":              []string{"DELETE"},
		"X-TOKENHIVE-SOURCE-OPERATION":    []string{"forged-operation"},
		"x-ToKeNhIvE-Untrusted-Extension": []string{"forged-extra"},
	}
	for name, values := range forgedHeaders {
		req.Header[name] = values
	}
	if err := applyTokenHiveHandoff(context.Background(), cfg, mapped, 7, "gpt-5.4", SourceOperationOpenAIResponsesHTTP, req); err != nil {
		t.Fatal(err)
	}

	trusted := map[string]string{
		TokenHiveHeaderRawURL:          "https://api.openai.com/v1/responses",
		TokenHiveHeaderUpstreamType:    UpstreamTypeOpenAICodexOAuth,
		TokenHiveHeaderUpstreamModel:   "gpt-5.4",
		TokenHiveHeaderMethod:          http.MethodPost,
		TokenHiveHeaderSourceOperation: SourceOperationOpenAIResponsesHTTP,
	}
	tokenHiveHeaders := 0
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), tokenHiveHeaderPrefix) {
			tokenHiveHeaders++
		}
		if strings.EqualFold(name, "x-tokenhive-untrusted-extension") {
			t.Fatalf("forged TokenHive header survived: %s", name)
		}
	}
	if tokenHiveHeaders != 7 {
		t.Fatalf("TokenHive header count = %d, want 7", tokenHiveHeaders)
	}
	for name, wantValue := range trusted {
		if got := req.Header.Values(name); len(got) != 1 || got[0] != wantValue {
			t.Errorf("%s values = %q, want [%q]", name, got, wantValue)
		}
	}
	if len(req.Header.Values(TokenHiveHeaderRequestID)) != 1 || len(req.Header.Values(TokenHiveHeaderTenantKey)) != 1 {
		t.Fatal("server-generated request ID and tenant key must each have exactly one value")
	}
}

func TestApplyTokenHiveHandoffStripsDuplicateAndAliasedForgedHeaders(t *testing.T) {
	cfg := &config.Config{TokenHive: tokenHiveConfigForTest(42)}
	mapped := &TokenHiveAccount{AccountID: 42, UpstreamType: UpstreamTypeOpenAICodexOAuth}
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	forgedValues := []string{
		"forged-source-one", "forged-source-two", "forged-source-alias-one", "forged-source-alias-two",
		"forged-request-one", "forged-request-two", "forged-request-alias-one", "forged-request-alias-two",
	}
	req.Header[TokenHiveHeaderSourceOperation] = []string{forgedValues[0], forgedValues[1]}
	req.Header["x-ToKeNhIvE-sOuRcE-oPeRaTiOn"] = []string{forgedValues[2], forgedValues[3]}
	req.Header[TokenHiveHeaderRequestID] = []string{forgedValues[4], forgedValues[5]}
	req.Header["x-ToKeNhIvE-rEqUeSt-Id"] = []string{forgedValues[6], forgedValues[7]}

	if err := applyTokenHiveHandoff(context.Background(), cfg, mapped, 7, "gpt-5.4", SourceOperationOpenAIResponsesHTTP, req); err != nil {
		t.Fatal(err)
	}

	if got := req.Header.Values(TokenHiveHeaderSourceOperation); len(got) != 1 || got[0] != SourceOperationOpenAIResponsesHTTP {
		t.Fatalf("source operation values = %q, want one canonical value", got)
	}
	if got := req.Header.Values(TokenHiveHeaderRequestID); len(got) != 1 || strings.TrimSpace(got[0]) == "" {
		t.Fatalf("request ID values = %q, want one server value", got)
	}
	tokenHiveHeaders := 0
	for name, values := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), tokenHiveHeaderPrefix) {
			tokenHiveHeaders++
		}
		for _, value := range values {
			for _, forged := range forgedValues {
				if value == forged {
					t.Fatalf("forged metadata survived name=%s", name)
				}
			}
		}
	}
	if tokenHiveHeaders != 7 {
		t.Fatalf("TokenHive header count = %d, want 7", tokenHiveHeaders)
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

func TestTokenHiveAnthropicRegistryRequiresOneAPIKeyVirtualAccount(t *testing.T) {
	cfg := tokenHiveAnthropicConfigForTest(42)
	tests := []struct {
		name     string
		accounts []Account
	}{
		{name: "mapped account missing"},
		{name: "mapped account is oauth", accounts: []Account{{ID: 42, Type: AccountTypeOAuth, Platform: PlatformAnthropic}}},
		{name: "mapped account is setup", accounts: []Account{{ID: 42, Type: AccountTypeSetupToken, Platform: PlatformAnthropic}}},
		{name: "mapped account is openai", accounts: []Account{{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewTokenHiveRegistry(cfg, tt.accounts); err == nil {
				t.Fatal("NewTokenHiveRegistry() error = nil")
			}
		})
	}

	registry, err := NewTokenHiveRegistry(cfg, []Account{{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformAnthropic}})
	if err != nil {
		t.Fatal(err)
	}
	if mapped, ok := registry.Match(&Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformAnthropic}); !ok || mapped.UpstreamType != UpstreamTypeAnthropicOAuth {
		t.Fatalf("Match() = %#v, %v", mapped, ok)
	}
	if _, ok := registry.Match(&Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}); ok {
		t.Fatal("registry matched wrong platform")
	}
}

func TestAnthropicGatewayTokenHivePolicyDoesNotChangeOpenAIAccounts(t *testing.T) {
	cfg := tokenHiveConfigForTest(42)
	account := &Account{ID: 42, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}
	registry, err := NewTokenHiveRegistry(cfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	policy := (&GatewayService{cfg: &config.Config{TokenHive: cfg}, tokenHiveRegistry: registry}).ResolveTokenHiveResponsePolicy(account)
	if policy.Dedicated || !policy.AllowSameAccountRetry || !policy.AllowAccountFailover || !policy.AllowAccountMutation || !policy.AllowRuntimeBlock || !policy.AllowSchedulerFeedback {
		t.Fatalf("GatewayService policy changed OpenAI behavior: %#v", policy)
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
	headers, err := BuildTokenHiveMetadata(ctx, 99, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", SourceOperationOpenAIResponsesHTTP, account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
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
	headers, err := BuildTokenHiveMetadata(context.Background(), 42, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", SourceOperationOpenAIResponsesHTTP, account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := headers.Get(TokenHiveHeaderTenantKey), "kayWd6oTodmBiOOzoawG8lDZJIetGTxg6-OExYtrwvA"; got != want {
		t.Fatalf("tenant key = %q, want %q", got, want)
	}
}

type handoffRecorder struct {
	httpUpstreamRecorder
}

func (r *handoffRecorder) respond(status int, contentType, body string) {
	r.resp = &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type sourceOperationCase struct {
	name      string
	operation string
	rawURL    string
	method    string
	invoke    func(*testing.T, *handoffRecorder)
}

func TestTokenHiveHTTPSourceOperationMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openAIAccount := &Account{
		ID: 42, Name: "mapped", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "mapped-fixture-key", "base_url": "https://api.openai.com"},
		Extra:       map[string]any{"openai_responses_supported": true},
		Status:      StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(openAIAccount.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*openAIAccount})
	if err != nil {
		t.Fatal(err)
	}
	newOpenAIService := func(upstream *handoffRecorder) *OpenAIGatewayService {
		cfg := &config.Config{TokenHive: tokenHiveCfg}
		return &OpenAIGatewayService{
			cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream,
			responseHeaderFilter: compileResponseHeaderFilter(cfg),
		}
	}
	newOpenAIContext := func(path string, body []byte) *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key", &APIKey{ID: 707})
		return c
	}

	tests := []sourceOperationCase{
		{
			name: "anthropic messages create", operation: SourceOperationAnthropicMessagesCreate,
			rawURL: claudeAPIURL, method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"type":"error","error":{"type":"api_error","message":"captured"}}`)
				svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
				body := []byte(`{"model":"claude-sonnet-4","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
				parsed, parseErr := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				_, _ = svc.Forward(context.Background(), c, account, parsed)
			},
		},
		{
			name: "anthropic messages stream", operation: SourceOperationAnthropicMessagesStream,
			rawURL: claudeAPIURL, method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"type":"error","error":{"type":"api_error","message":"captured"}}`)
				svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
				body := []byte(`{"model":"claude-sonnet-4","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
				parsed, parseErr := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				_, _ = svc.Forward(context.Background(), c, account, parsed)
			},
		},
		{
			name: "anthropic messages count tokens", operation: SourceOperationAnthropicMessagesCountTokens,
			rawURL: claudeAPICountTokensURL, method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"type":"error","error":{"type":"api_error","message":"captured"}}`)
				svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
				body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
				parsed, parseErr := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				_ = svc.ForwardCountTokens(context.Background(), c, account, parsed)
			},
		},
		{
			name: "openai responses http", operation: SourceOperationOpenAIResponsesHTTP,
			rawURL: "https://api.openai.com/v1/responses", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-5.4","stream":false,"input":"hello"}`)
				_, _ = newOpenAIService(upstream).Forward(context.Background(), newOpenAIContext("/v1/responses", body), openAIAccount, body)
			},
		},
		{
			name: "openai responses compact", operation: SourceOperationOpenAIResponsesCompact,
			rawURL: "https://api.openai.com/v1/responses/compact", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"}]}`)
				_, _ = newOpenAIService(upstream).Forward(context.Background(), newOpenAIContext("/v1/responses/compact", body), openAIAccount, body)
			},
		},
		{
			name: "openai responses input tokens", operation: SourceOperationOpenAIResponsesInputTokens,
			rawURL: "https://api.openai.com/v1/responses/input_tokens", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
				_ = newOpenAIService(upstream).ForwardCountTokensAsAnthropic(context.Background(), newOpenAIContext("/v1/messages/count_tokens", body), openAIAccount, body, "gpt-5.4")
			},
		},
		{
			name: "openai images generations", operation: SourceOperationOpenAIImagesGenerations,
			rawURL: "https://api.openai.com/v1/images/generations", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-image-2","prompt":"draw"}`)
				c := newOpenAIContext("/v1/images/generations", body)
				svc := newOpenAIService(upstream)
				parsed, parseErr := svc.ParseOpenAIImagesRequest(c, body)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				_, _ = svc.ForwardImages(context.Background(), c, openAIAccount, body, parsed, "")
			},
		},
		{
			name: "openai images edits", operation: SourceOperationOpenAIImagesEdits,
			rawURL: "https://api.openai.com/v1/images/edits", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"https://example.com/input.png"}]}`)
				c := newOpenAIContext("/v1/images/edits", body)
				svc := newOpenAIService(upstream)
				parsed, parseErr := svc.ParseOpenAIImagesRequest(c, body)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				_, _ = svc.ForwardImages(context.Background(), c, openAIAccount, body, parsed, "")
			},
		},
		{
			name: "openai alpha search", operation: SourceOperationOpenAIAlphaSearch,
			rawURL: "https://api.openai.com/v1/alpha/search?feature=standalone", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
				_, _ = newOpenAIService(upstream).ForwardAlphaSearch(context.Background(), newOpenAIContext("/v1/alpha/search?feature=standalone", body), openAIAccount, body)
			},
		},
		{
			name: "openai codex models manifest", operation: SourceOperationOpenAICodexModelsManifest,
			rawURL: "https://api.openai.com/v1/models?client_version=0.145.0", method: http.MethodGet,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				_, _ = newOpenAIService(upstream).FetchCodexModelsManifestForClient(context.Background(), openAIAccount, "0.145.0", "", 707)
			},
		},
	}

	if len(tests) != len(tokenHiveSourceOperations) {
		t.Fatalf("matrix rows = %d, source operations = %d", len(tests), len(tokenHiveSourceOperations))
	}
	seen := make(map[string]struct{}, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &handoffRecorder{}
			tt.invoke(t, upstream)
			if upstream.lastReq == nil {
				t.Fatal("missing upstream request")
			}
			if got := upstream.lastReq.URL.String(); got != tokenHiveCfg.ProxyURL {
				t.Fatalf("transport target = %q, want %q", got, tokenHiveCfg.ProxyURL)
			}
			if got := upstream.lastReq.Header.Get(TokenHiveHeaderSourceOperation); got != tt.operation || len(upstream.lastReq.Header.Values(TokenHiveHeaderSourceOperation)) != 1 {
				t.Fatalf("source operation values = %q, want [%q]", upstream.lastReq.Header.Values(TokenHiveHeaderSourceOperation), tt.operation)
			}
			if got := upstream.lastReq.Header.Get(TokenHiveHeaderRawURL); got != tt.rawURL {
				t.Fatalf("logical URL = %q, want %q", got, tt.rawURL)
			}
			if got := upstream.lastReq.Header.Get(TokenHiveHeaderMethod); got != tt.method {
				t.Fatalf("logical method = %q, want %q", got, tt.method)
			}
			if upstream.lastProxyURL != "" || len(upstream.requests) != 1 {
				t.Fatalf("proxy URL = %q, upstream calls = %d, want direct and one", upstream.lastProxyURL, len(upstream.requests))
			}
			seen[tt.operation] = struct{}{}
		})
	}
	if len(seen) != len(tokenHiveSourceOperations) {
		t.Fatalf("matrix covered %d unique source operations, want %d", len(seen), len(tokenHiveSourceOperations))
	}
}

func TestTokenHiveCompatibilitySourceOperationMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openAIAccount := &Account{
		ID: 42, Name: "mapped-openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "mapped-fixture-key", "base_url": "https://api.openai.com"},
		Extra:       map[string]any{"openai_responses_supported": true}, Status: StatusActive, Schedulable: true,
	}
	openAICfg := tokenHiveConfigForTest(openAIAccount.ID)
	openAIRegistry, err := NewTokenHiveRegistry(openAICfg, []Account{*openAIAccount})
	if err != nil {
		t.Fatal(err)
	}
	newOpenAIService := func(upstream *handoffRecorder) *OpenAIGatewayService {
		cfg := &config.Config{TokenHive: openAICfg}
		return &OpenAIGatewayService{cfg: cfg, tokenHiveRegistry: openAIRegistry, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	}
	newContext := func(path string, body []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key", &APIKey{ID: 707})
		return c
	}

	tests := []sourceOperationCase{
		{
			name: "chat completions converts to OpenAI Responses", operation: SourceOperationOpenAIResponsesHTTP,
			rawURL: "https://api.openai.com/v1/responses", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
				_, _ = newOpenAIService(upstream).ForwardAsChatCompletions(context.Background(), newContext("/v1/chat/completions", body), openAIAccount, body, "", "")
			},
		},
		{
			name: "OpenAI compatible Messages converts to Responses", operation: SourceOperationOpenAIResponsesHTTP,
			rawURL: "https://api.openai.com/v1/responses", method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
				body := []byte(`{"model":"gpt-5.4","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
				_, _ = newOpenAIService(upstream).ForwardAsAnthropic(context.Background(), newContext("/v1/messages", body), openAIAccount, body, "", "")
			},
		},
		{
			name: "Anthropic compatible Chat Completions forces Messages stream", operation: SourceOperationAnthropicMessagesStream,
			rawURL: claudeAPIURL, method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"type":"error","error":{"type":"api_error","message":"captured"}}`)
				svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
				body := []byte(`{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
				_, _ = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			},
		},
		{
			name: "Anthropic compatible Responses forces Messages stream", operation: SourceOperationAnthropicMessagesStream,
			rawURL: claudeAPIURL, method: http.MethodPost,
			invoke: func(t *testing.T, upstream *handoffRecorder) {
				upstream.respond(http.StatusTeapot, "application/json", `{"type":"error","error":{"type":"api_error","message":"captured"}}`)
				svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
				body := []byte(`{"model":"claude-sonnet-4","stream":false,"input":"hello"}`)
				_, _ = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &handoffRecorder{}
			tt.invoke(t, upstream)
			if upstream.lastReq == nil {
				t.Fatal("missing compatibility upstream request")
			}
			if got := upstream.lastReq.URL.String(); got != openAICfg.ProxyURL {
				t.Fatalf("transport target = %q, want %q", got, openAICfg.ProxyURL)
			}
			if got := upstream.lastReq.Header.Values(TokenHiveHeaderSourceOperation); len(got) != 1 || got[0] != tt.operation {
				t.Fatalf("source operation values = %q, want [%q]", got, tt.operation)
			}
			if got := upstream.lastReq.Header.Get(TokenHiveHeaderRawURL); got != tt.rawURL {
				t.Fatalf("logical URL = %q, want %q", got, tt.rawURL)
			}
			if got := upstream.lastReq.Header.Get(TokenHiveHeaderMethod); got != tt.method {
				t.Fatalf("logical method = %q, want %q", got, tt.method)
			}
			if upstream.lastProxyURL != "" || len(upstream.requests) != 1 {
				t.Fatalf("proxy URL = %q, upstream calls = %d, want direct and one", upstream.lastProxyURL, len(upstream.requests))
			}
		})
	}
}

func TestTokenHiveMappedCompatibilityForceChatCompletionsUsesCanonicalHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID: 42, Name: "mapped-force-chat", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "mapped-fixture-key", "base_url": "https://api.openai.com"},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions),
		},
		Status: StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	newService := func(upstream *handoffRecorder) *OpenAIGatewayService {
		cfg := &config.Config{TokenHive: tokenHiveCfg}
		return &OpenAIGatewayService{cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	}
	newContext := func(path string, body []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key", &APIKey{ID: 707})
		return c
	}

	tests := []struct {
		name   string
		path   string
		body   []byte
		invoke func(*OpenAIGatewayService, *gin.Context, []byte)
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hello"}]}`),
			invoke: func(svc *OpenAIGatewayService, c *gin.Context, body []byte) {
				_, _ = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			},
		},
		{
			name: "messages",
			path: "/v1/messages",
			body: []byte(`{"model":"gpt-5.4","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`),
			invoke: func(svc *OpenAIGatewayService, c *gin.Context, body []byte) {
				_, _ = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &handoffRecorder{}
			upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
			tt.invoke(newService(upstream), newContext(tt.path, tt.body), tt.body)
			assertSingleTokenHiveCanonicalRequest(t, upstream, tokenHiveCfg.ProxyURL, SourceOperationOpenAIResponsesHTTP)
		})
	}
}

func TestTokenHiveMappedChatCompletionsUnknownResponsesErrorDoesNotRawRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID: 42, Name: "mapped-unknown", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "mapped-fixture-key", "base_url": "https://api.openai.com"},
		Status:      StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &handoffRecorder{httpUpstreamRecorder: httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"responses unsupported"}}`)),
		},
		{
			StatusCode: http.StatusTeapot,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"raw retry captured"}}`)),
		},
	}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &APIKey{ID: 707})
	cfg := &config.Config{TokenHive: tokenHiveCfg}
	svc := &OpenAIGatewayService{cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}

	_, _ = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")

	assertSingleTokenHiveCanonicalRequest(t, upstream, tokenHiveCfg.ProxyURL, SourceOperationOpenAIResponsesHTTP)
}

func TestTokenHiveMappedPATShapedAlphaSearchUsesCanonicalOperation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var whoamiCalls int32
	whoamiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&whoamiCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chatgpt_account_id":"unexpected-preflight"}`))
	}))
	defer whoamiServer.Close()
	oldWhoamiURL := openAICodexPATWhoamiURL
	openAICodexPATWhoamiURL = whoamiServer.URL
	defer func() { openAICodexPATWhoamiURL = oldWhoamiURL }()

	account := &Account{
		ID: 42, Name: "mapped-pat-shaped", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{
			"api_key":   "at-mapped-fixture-token",
			"auth_mode": OpenAIAuthModePersonalAccessToken,
			"base_url":  "https://api.openai.com",
		},
		Status: StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &handoffRecorder{}
	upstream.respond(http.StatusTeapot, "application/json", `{"error":{"message":"captured"}}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body := []byte(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search?feature=standalone", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &APIKey{ID: 707})
	cfg := &config.Config{TokenHive: tokenHiveCfg}
	svc := &OpenAIGatewayService{
		cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream,
		openAITokenProvider:  NewOpenAITokenProvider(nil, nil, NewOpenAIOAuthService(nil, nil)),
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
	}

	_, _ = svc.ForwardAlphaSearch(context.Background(), c, account, body)

	assertSingleTokenHiveCanonicalRequest(t, upstream, tokenHiveCfg.ProxyURL, SourceOperationOpenAIAlphaSearch)
	if got := atomic.LoadInt32(&whoamiCalls); got != 0 {
		t.Fatalf("PAT metadata validation calls = %d, want 0 for mapped account", got)
	}
}

func assertSingleTokenHiveCanonicalRequest(t *testing.T, upstream *handoffRecorder, proxyURL, operation string) {
	t.Helper()
	if got := len(upstream.requests); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly one canonical TokenHive send", got)
	}
	req := upstream.requests[0]
	if req == nil {
		t.Fatal("canonical TokenHive request is nil")
	}
	if got := req.URL.String(); got != proxyURL {
		t.Fatalf("transport target = %q, want %q", got, proxyURL)
	}
	if got := req.Header.Values(TokenHiveHeaderSourceOperation); len(got) != 1 || got[0] != operation {
		t.Fatalf("source operation values = %q, want [%q]", got, operation)
	}
	if upstream.lastProxyURL != "" {
		t.Fatalf("account proxy = %q, want direct TokenHive transport", upstream.lastProxyURL)
	}
}
