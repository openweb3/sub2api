package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

func TestTokenHiveCodexManifestSentinelContract(t *testing.T) {
	if CapabilityOpenAICodexModelsManifest != "openai.codex.models.manifest" {
		t.Fatalf("manifest sentinel = %q", CapabilityOpenAICodexModelsManifest)
	}
}

func TestTokenHiveCodexManifestUsesCanonicalLogicalGETOnce(t *testing.T) {
	account := &Account{
		ID: 42, Name: "mapped", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: map[string]any{"api_key": "VIRTUAL_ACCOUNT_SECRET"},
		Status: StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "Etag": []string{`W/"oauth"`}},
		Body:       io.NopCloser(strings.NewReader(` {"models":[{"slug":"gpt-5.6-sol","use_responses_lite":true}]} `)),
	}}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{TokenHive: tokenHiveCfg}, tokenHiveRegistry: registry, httpUpstream: upstream,
	}
	manifest, err := svc.FetchCodexModelsManifestForClient(context.Background(), account, "0.145.0", `W/"client"`, 707)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstream.requests) != 1 || upstream.lastReq == nil {
		t.Fatalf("upstream calls = %d", len(upstream.requests))
	}
	request := upstream.lastReq
	if request.Method != http.MethodPost || request.URL.String() != tokenHiveCfg.ProxyURL || upstream.lastProxyURL != "" || len(upstream.lastBody) != 0 {
		t.Fatalf("transport request method=%q url=%q proxy=%q body=%q", request.Method, request.URL, upstream.lastProxyURL, upstream.lastBody)
	}
	if got, want := request.Header.Get(TokenHiveHeaderMethod), http.MethodGet; got != want {
		t.Fatalf("logical method = %q, want %q", got, want)
	}
	if got, want := request.Header.Get(TokenHiveHeaderRawURL), "https://api.openai.com/v1/models?client_version=0.145.0"; got != want {
		t.Fatalf("logical URL = %q, want %q", got, want)
	}
	if got := request.Header.Get(TokenHiveHeaderUpstreamModel); got != CapabilityOpenAICodexModelsManifest {
		t.Fatalf("upstream model metadata = %q", got)
	}
	if got := request.Header.Get("If-None-Match"); got != `W/"client"` {
		t.Fatalf("If-None-Match = %q", got)
	}
	for _, secret := range []string{"VIRTUAL_ACCOUNT_SECRET", "Authorization", "Bearer"} {
		if strings.Contains(request.Header.Get("Authorization")+string(upstream.lastBody), secret) {
			t.Fatalf("handoff exposed %q", secret)
		}
	}
	if got, want := string(manifest.Body), ` {"models":[{"slug":"gpt-5.6-sol","use_responses_lite":true}]} `; got != want || manifest.ETag != `W/"oauth"` {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestTokenHiveCodexManifestMappedFailureIsOneAttempt(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Status: StatusActive, Schedulable: true}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"unavailable"}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{TokenHive: tokenHiveCfg}, tokenHiveRegistry: registry, httpUpstream: upstream}
	_, err = svc.FetchCodexModelsManifestForClient(context.Background(), account, "0.145.0", "", 707)
	if err == nil || IsRetryableCodexModelsManifestError(err) {
		t.Fatalf("mapped error = %v, retryable=%v", err, IsRetryableCodexModelsManifestError(err))
	}
	if len(upstream.requests) != 1 {
		t.Fatalf("mapped calls = %d, want 1", len(upstream.requests))
	}
}

func TestTokenHiveCodexManifestMappedWithoutClientVersionMakesZeroCalls(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Status: StatusActive, Schedulable: true}
	tokenHiveCfg := tokenHiveConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: &config.Config{TokenHive: tokenHiveCfg}, tokenHiveRegistry: registry, httpUpstream: upstream}
	_, err = svc.FetchCodexModelsManifestForClient(context.Background(), account, "", "", 707)
	if err == nil || IsRetryableCodexModelsManifestError(err) || len(upstream.requests) != 0 {
		t.Fatalf("error=%v retryable=%v calls=%d", err, IsRetryableCodexModelsManifestError(err), len(upstream.requests))
	}
}

func TestOrdinaryCodexManifestPreservesCustomAPIKeyPath(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"ordinary-model"}]}`)),
	}}
	svc := newCodexModelsAPIKeyTestService(upstream)
	account := newCodexModelsAPIKeyTestAccount("https://ordinary.example/v1")
	manifest, err := svc.FetchCodexModelsManifestForClient(context.Background(), account, "0.145.0", "", 707)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstream.requests) != 1 || upstream.lastReq.URL.String() != "https://ordinary.example/v1/models?client_version=0.145.0" || upstream.lastReq.Method != http.MethodGet {
		t.Fatalf("ordinary request = %#v", upstream.lastReq)
	}
	if request := upstream.lastReq; request.Header.Get(TokenHiveHeaderMethod) != "" || request.Header.Get(TokenHiveHeaderRawURL) != "" {
		t.Fatalf("ordinary request gained TokenHive metadata: %#v", request.Header)
	}
	if got, want := gjson.GetBytes(manifest.Body, "models.0.slug").String(), "ordinary-model"; got != want {
		t.Fatalf("ordinary model slug = %q, want %q", got, want)
	}
}
