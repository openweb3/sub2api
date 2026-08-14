package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func tokenHiveConfigForTest(accountID int64) config.TokenHiveConfig {
	return config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{accountID: UpstreamTypeOpenAICodexOAuth},
	}
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
			if err := applyTokenHiveHandoff(context.Background(), cfg, mapped, 7, "fixture-model", req); err != nil {
				t.Fatal(err)
			}
			if req.Method != http.MethodPost || req.Header.Get("X-TokenHive-Method") != method || len(req.Header.Values("X-TokenHive-Method")) != 1 {
				t.Fatalf("transport_method=%q logical_method=%q method_values=%d", req.Method, req.Header.Get("X-TokenHive-Method"), len(req.Header.Values("X-TokenHive-Method")))
			}
		})
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
	headers, err := BuildTokenHiveMetadata(ctx, 99, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
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
	headers, err := BuildTokenHiveMetadata(context.Background(), 42, http.MethodPost, "https://api.openai.com/v1/responses", "gpt-5.4", account, []byte("slice-1-test-tenant-hmac-key-32bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := headers.Get(TokenHiveHeaderTenantKey), "kayWd6oTodmBiOOzoawG8lDZJIetGTxg6-OExYtrwvA"; got != want {
		t.Fatalf("tenant key = %q, want %q", got, want)
	}
}

func TestTokenHiveAuxiliaryCallSitesUseCanonicalHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID: 42, Name: "mapped", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "mapped-fixture-key"}, Status: StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	newService := func(responseBody string) (*OpenAIGatewayService, *httpUpstreamRecorder) {
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(responseBody)),
		}}
		return &OpenAIGatewayService{cfg: &config.Config{TokenHive: tokenHiveCfg}, tokenHiveRegistry: registry, httpUpstream: upstream}, upstream
	}
	assertHandoff := func(t *testing.T, upstream *httpUpstreamRecorder, rawURL string) {
		t.Helper()
		if upstream.lastReq == nil {
			t.Fatal("missing upstream request")
		}
		if got := upstream.lastReq.URL.String(); got != tokenHiveCfg.ProxyURL {
			t.Fatalf("transport target=%q", got)
		}
		if got := upstream.lastReq.Header.Get(TokenHiveHeaderRawURL); got != rawURL {
			t.Fatalf("logical target=%q", got)
		}
		if upstream.lastReq.Header.Get(TokenHiveHeaderMethod) != http.MethodPost || upstream.lastProxyURL != "" || len(upstream.requests) != 1 {
			t.Fatalf("logical method=%q has_proxy=%t calls=%d", upstream.lastReq.Header.Get(TokenHiveHeaderMethod), upstream.lastProxyURL != "", len(upstream.requests))
		}
	}

	t.Run("images generation", func(t *testing.T) {
		body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat","response_format":"b64_json"}`)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key", &APIKey{ID: 707})
		svc, upstream := newService(`{"created":1710000004,"data":[{"b64_json":"aGVsbG8="}]}`)
		parsed, err := svc.ParseOpenAIImagesRequest(c, body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ForwardImages(context.Background(), c, account, body, parsed, ""); err != nil {
			t.Fatal(err)
		}
		assertHandoff(t, upstream, "https://api.openai.com/v1/images/generations")
		if gjson.Get(recorder.Body.String(), "data.0.b64_json").String() != "aGVsbG8=" {
			t.Fatal("sub2api image client response ownership changed")
		}
	})

	t.Run("alpha search", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search?feature=standalone", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 707})
		svc, upstream := newService(`{"output":"result"}`)
		if _, err := svc.ForwardAlphaSearch(context.Background(), c, account, body); err != nil {
			t.Fatal(err)
		}
		assertHandoff(t, upstream, "https://api.openai.com/v1/alpha/search?feature=standalone")
		if gjson.Get(recorder.Body.String(), "output").String() != "result" {
			t.Fatal("sub2api search client response ownership changed")
		}
	})

	t.Run("count input tokens", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 707})
		svc, upstream := newService(`{"object":"response.input_tokens","input_tokens":42}`)
		if err := svc.ForwardCountTokensAsAnthropic(context.Background(), c, account, body, "gpt-5.4"); err != nil {
			t.Fatal(err)
		}
		assertHandoff(t, upstream, "https://api.openai.com/v1/responses/input_tokens")
		if gjson.Get(recorder.Body.String(), "input_tokens").Int() != 42 || gjson.GetBytes(upstream.lastBody, "input").Raw == "" {
			t.Fatal("sub2api count conversion or projection ownership changed")
		}
	})
}
