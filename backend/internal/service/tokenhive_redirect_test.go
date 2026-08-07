package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenHiveResponseRecordingUpstream struct {
	service.HTTPUpstream
	statusCode int
	location   string
}

func (u *tokenHiveResponseRecordingUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	resp, err := u.HTTPUpstream.Do(req, proxyURL, accountID, accountConcurrency)
	if resp != nil {
		u.statusCode = resp.StatusCode
		u.location = resp.Header.Get("Location")
	}
	return resp, err
}

func TestTokenHiveMappedForwardDoesNotReplayRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalBody := []byte(`{"model":"gpt-5.4","stream":false,"instructions":"keep","input":"hello"}`)
	responseBody := []byte(`{"id":"resp_redirect","object":"response","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	var trapCalls atomic.Int64
	var trapBody []byte
	var trapTenantKey string
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trapCalls.Add(1)
		trapBody, _ = io.ReadAll(r.Body)
		trapTenantKey = r.Header.Get(service.TokenHiveHeaderTenantKey)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(responseBody)
	}))
	t.Cleanup(trap.Close)

	var localProxyCalls atomic.Int64
	var localProxyBody []byte
	var localProxyTenantKey string
	localProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localProxyCalls.Add(1)
		localProxyBody, _ = io.ReadAll(r.Body)
		localProxyTenantKey = r.Header.Get(service.TokenHiveHeaderTenantKey)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", trap.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = w.Write(responseBody)
	}))
	t.Cleanup(localProxy.Close)

	const accountID int64 = 42
	tokenHiveCfg := config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      localProxy.URL + "/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{accountID: service.UpstreamTypeOpenAICodexOAuth},
	}
	account := &service.Account{
		ID: accountID, Name: "tokenhive", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "placeholder", "base_url": "https://api.openai.com"},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: true,
		},
		Status: service.StatusActive, Schedulable: true,
	}
	registry, err := service.NewTokenHiveRegistry(tokenHiveCfg, []service.Account{*account})
	require.NoError(t, err)
	cfg := &config.Config{TokenHive: tokenHiveCfg}
	upstream := &tokenHiveResponseRecordingUpstream{HTTPUpstream: repository.NewHTTPUpstream(nil)}
	gateway := service.ProvideOpenAIGatewayService(
		registry,
		nil, nil, nil, nil, nil, nil, nil,
		cfg,
		nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(originalBody))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &service.APIKey{ID: 77})

	result, err := gateway.Forward(context.Background(), c, account, originalBody)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(1), localProxyCalls.Load())
	require.Equal(t, originalBody, localProxyBody)
	require.NotEmpty(t, localProxyTenantKey)
	require.Zero(t, trapCalls.Load())
	require.Empty(t, trapBody)
	require.Empty(t, trapTenantKey)
	require.Equal(t, http.StatusTemporaryRedirect, upstream.statusCode)
	require.Equal(t, trap.URL, upstream.location)
	require.Equal(t, http.StatusTemporaryRedirect, recorder.Code)
	require.Equal(t, trap.URL, recorder.Header().Get("Location"))
}
