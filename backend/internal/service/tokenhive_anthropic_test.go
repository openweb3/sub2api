package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenHiveAnthropicUpstream struct {
	calls    int
	request  *http.Request
	body     []byte
	proxyURL string
	response *http.Response
}

type tokenHiveStickyCache struct {
	*schedulerTestGatewayCache
	refreshedSessions map[string]int
}

type tokenHiveAnthropicSchedulerRepo struct {
	schedulerTestOpenAIAccountRepo
}

func (r tokenHiveAnthropicSchedulerRepo) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]Account, error) {
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	accounts := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if _, ok := allowed[account.Platform]; ok {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (c *tokenHiveStickyCache) RefreshSessionTTL(_ context.Context, _ int64, sessionHash string, _ time.Duration) error {
	if c.refreshedSessions == nil {
		c.refreshedSessions = make(map[string]int)
	}
	c.refreshedSessions[sessionHash]++
	return nil
}

func (u *tokenHiveAnthropicUpstream) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.request = req.Clone(req.Context())
	u.body, _ = io.ReadAll(req.Body)
	u.proxyURL = proxyURL
	return u.response, nil
}

func (u *tokenHiveAnthropicUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func tokenHiveAnthropicSuccessResponse(stream bool) *http.Response {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250514","content":[],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":3}}`
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
		body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}, "x-request-id": []string{"anthropic-hive-1"}}, Body: io.NopCloser(bytes.NewBufferString(body))}
}

func newTokenHiveAnthropicService(t *testing.T, upstream HTTPUpstream) (*GatewayService, *Account, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	account := &Account{
		ID: 42, Name: "anthropic-hive", Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "VIRTUAL_ACCOUNT_SECRET", "model_mapping": map[string]any{"claude-sonnet-4": "claude-sonnet-4-20250514"}},
		Status:      StatusActive, Schedulable: true,
	}
	tokenHiveCfg := tokenHiveAnthropicConfigForTest(account.ID)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
	require.NoError(t, err)
	cfg := &config.Config{TokenHive: tokenHiveCfg}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("anthropic-version", "2023-06-01")
	c.Request.Header.Set("anthropic-beta", "context-management-2025-06-27")
	c.Request.Header.Set("X-Claude-Code-Session-Id", "session-fixture")
	c.Request.Header.Set("Authorization", "Bearer CLIENT_API_KEY_SECRET")
	c.Set("api_key", &APIKey{ID: 77})
	return &GatewayService{cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream, responseHeaderFilter: compileResponseHeaderFilter(cfg)}, account, c, recorder
}

func TestTokenHiveAnthropicMessagesCanonicalHandoffPrecedesProviderMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &tokenHiveAnthropicUpstream{response: tokenHiveAnthropicSuccessResponse(false)}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	body := []byte(`{"model":"claude-sonnet-4","stream":false,"max_tokens":64,"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"one","cache_control":{"type":"ephemeral"}},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}},{"type":"text","text":"three","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"mcp__fixture__lookup","description":"tool","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"tool_choice":{"type":"tool","name":"mcp__fixture__lookup"},"metadata":{"user_id":"session_abc"}}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, upstream.calls)
	require.Empty(t, upstream.proxyURL)
	require.Equal(t, http.MethodPost, upstream.request.Method)
	require.Equal(t, "http://127.0.0.1:18081/internal/v1/proxy", upstream.request.URL.String())
	require.Equal(t, UpstreamTypeAnthropicOAuth, upstream.request.Header.Get(TokenHiveHeaderUpstreamType))
	require.Equal(t, claudeAPIURL, upstream.request.Header.Get(TokenHiveHeaderRawURL))
	require.Equal(t, "claude-sonnet-4-20250514", upstream.request.Header.Get(TokenHiveHeaderUpstreamModel))
	require.Equal(t, "context-management-2025-06-27", getHeaderRaw(upstream.request.Header, "anthropic-beta"))
	require.Empty(t, getHeaderRaw(upstream.request.Header, "authorization"))
	require.Empty(t, getHeaderRaw(upstream.request.Header, "x-api-key"))
	require.NotContains(t, string(upstream.body), "VIRTUAL_ACCOUNT_SECRET")
	require.NotContains(t, string(upstream.body), "CLIENT_API_KEY_SECRET")
	require.JSONEq(t, string(svc.replaceModelInBody(body, "claude-sonnet-4-20250514")), string(upstream.body))
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestTokenHiveAnthropicCountTokensCanonicalHandoffKeepsGenerationFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"input_tokens":17}`))}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	c.Request.URL.Path = "/v1/messages/count_tokens"
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}],"max_tokens":512,"temperature":0.2,"stop_sequences":["done"],"metadata":{"user_id":"session_count"}}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, claudeAPICountTokensURL, upstream.request.Header.Get(TokenHiveHeaderRawURL))
	require.Equal(t, UpstreamTypeAnthropicOAuth, upstream.request.Header.Get(TokenHiveHeaderUpstreamType))
	require.JSONEq(t, string(svc.replaceModelInBody(body, "claude-sonnet-4-20250514")), string(upstream.body))
	require.JSONEq(t, `{"input_tokens":17}`, recorder.Body.String())
}

func TestTokenHiveAnthropicCountTokensReadErrorWritesBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &streamReadCloser{payload: []byte(`{"input_tokens":`), err: io.ErrUnexpectedEOF},
	}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	c.Request.URL.Path = "/v1/messages/count_tokens"
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.JSONEq(t, `{"type":"error","error":{"type":"upstream_error","message":"Failed to read response"}}`, recorder.Body.String())
}

func TestTokenHiveAnthropicCountTokensDoesNotRepairSignature400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"invalid_request_error","message":"Invalid signature in thinking block"}}`)),
	}}
	svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
	repo := &tokenHivePolicyAccountRepo{}
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	c.Request.URL.Path = "/v1/messages/count_tokens"
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"keep","signature":"bad-signature"}]}],"max_tokens":512,"temperature":0.2}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.Equal(t, 1, upstream.calls)
	require.JSONEq(t, string(svc.replaceModelInBody(body, "claude-sonnet-4-20250514")), string(upstream.body))
	require.Zero(t, repo.tempUnschedulable)
	require.Zero(t, repo.setError)
	require.Zero(t, repo.rateLimited)
}

func TestTokenHiveAnthropicDedicatedPolicyIsOneAttemptWithoutAccountMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseBody := `{"type":"error","error":{"type":"rate_limit_error","message":"retry me"}}`
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"60"}},
		Body:       io.NopCloser(bytes.NewBufferString(responseBody)),
	}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	repo := &tokenHivePolicyAccountRepo{}
	svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	body := []byte(`{"model":"claude-sonnet-4","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, upstream.calls)
	require.Zero(t, repo.tempUnschedulable)
	require.Zero(t, repo.setError)
	require.Zero(t, repo.rateLimited)
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
}

func TestTokenHiveAnthropicCompatibilityHandoffStripsCredentialsAndDisablesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		path   string
		body   []byte
		invoke func(*GatewayService, *gin.Context, *Account, []byte) (*ForwardResult, error)
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`),
			invoke: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"claude-sonnet-4","stream":false,"input":"hello"}`),
			invoke: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsResponses(context.Background(), c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"60"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"retry me"}}`)),
			}}
			svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
			c.Request.URL.Path = tt.path
			repo := &tokenHivePolicyAccountRepo{}
			svc.rateLimitService = NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

			result, err := tt.invoke(svc, c, account, tt.body)

			require.Error(t, err)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr))
			require.Equal(t, 1, upstream.calls)
			require.NotNil(t, upstream.request)
			require.Empty(t, getHeaderRaw(upstream.request.Header, "authorization"))
			require.Empty(t, getHeaderRaw(upstream.request.Header, "x-api-key"))
			require.Empty(t, getHeaderRaw(upstream.request.Header, "cookie"))
			require.NotContains(t, fmt.Sprint(upstream.request.Header), "VIRTUAL_ACCOUNT_SECRET")
			require.Zero(t, repo.tempUnschedulable)
			require.Zero(t, repo.setError)
			require.Zero(t, repo.rateLimited)
			require.Equal(t, http.StatusTooManyRequests, recorder.Code)
		})
	}
}

func TestOrdinaryAnthropicCompatibilityPreservesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		path   string
		body   []byte
		invoke func(*GatewayService, *gin.Context, *Account, []byte) (*ForwardResult, error)
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hello"}]}`),
			invoke: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"claude-sonnet-4","stream":false,"input":"hello"}`),
			invoke: func(s *GatewayService, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return s.ForwardAsResponses(context.Background(), c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"60"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"type":"error","error":{"type":"rate_limit_error","message":"retry me"}}`)),
			}}
			svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
			svc.cfg = &config.Config{}
			svc.tokenHiveRegistry = nil
			c.Request.URL.Path = tt.path
			repo := &tokenHivePolicyAccountRepo{}
			svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)

			result, err := tt.invoke(svc, c, account, tt.body)

			require.Error(t, err)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.True(t, errors.As(err, &failoverErr))
			require.Equal(t, 1, upstream.calls)
			require.Greater(t, repo.rateLimited, 0)
			require.Equal(t, "VIRTUAL_ACCOUNT_SECRET", getHeaderRaw(upstream.request.Header, "x-api-key"))
		})
	}
}

func TestTokenHiveAnthropicStreamReadErrorDoesNotFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &streamReadCloser{err: io.ErrUnexpectedEOF},
	}}
	svc, account, c, _ := newTokenHiveAnthropicService(t, upstream)
	body := []byte(`{"model":"claude-sonnet-4","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, upstream.calls)
}

func TestTokenHiveAnthropicStreamErrorEventRemainsLegalSSEWithoutFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const errorData = `{"type":"error","error":{"type":"overloaded_error","message":"capacity unavailable"}}`
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString("event: error\ndata: " + errorData + "\n\n")),
	}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	body := []byte(`{"model":"claude-sonnet-4","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	require.Equal(t, "event: error\ndata: "+errorData+"\n\n", recorder.Body.String())
}

func TestTokenHiveAnthropicStreamErrorEventReturnsObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const errorData = `{"type":"error","error":{"type":"overloaded_error","message":"capacity unavailable"}}`
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n" +
		"event: error\ndata: " + errorData + "\n\n"
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(stream)),
	}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	body := []byte(`{"model":"claude-sonnet-4","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Contains(t, recorder.Body.String(), "event: message_start")
	require.Contains(t, recorder.Body.String(), "event: error\ndata: "+errorData)
}

func TestTokenHiveAnthropicMappedAccountSuppressesStickyFeedback(t *testing.T) {
	mapped := Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	ordinary := Account{ID: 43, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	openAIMapped := Account{ID: 44, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	cfg := tokenHiveAnthropicConfigForTest(mapped.ID)
	cfg.Accounts[openAIMapped.ID] = UpstreamTypeOpenAICodexOAuth
	registry, err := NewTokenHiveRegistry(cfg, []Account{mapped, openAIMapped})
	require.NoError(t, err)
	cache := &schedulerTestGatewayCache{}
	svc := &GatewayService{tokenHiveRegistry: registry, cache: cache}

	require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), nil, "mapped-selection", mapped.ID))
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), nil, "mapped-admission", mapped.ID))
	require.NotContains(t, cache.sessionBindings, "mapped-selection")
	require.NotContains(t, cache.sessionBindings, "mapped-admission")
	require.False(t, svc.ResolveTokenHiveResponsePolicy(&mapped).AllowSchedulerFeedback)

	require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), nil, "ordinary-selection", ordinary.ID))
	require.NoError(t, svc.BindStickySessionAfterProfitAdmission(context.Background(), nil, "ordinary-admission", ordinary.ID))
	require.Equal(t, ordinary.ID, cache.sessionBindings["ordinary-selection"])
	require.Equal(t, ordinary.ID, cache.sessionBindings["ordinary-admission"])
	require.True(t, svc.ResolveTokenHiveResponsePolicy(&ordinary).AllowSchedulerFeedback)

	require.NoError(t, svc.bindGatewayStickySessionDuringSelection(context.Background(), nil, "openai-selection", openAIMapped.ID))
	require.Equal(t, openAIMapped.ID, cache.sessionBindings["openai-selection"])
}

func TestTokenHiveAnthropicMappedAccountSuppressesPreexistingStickyMutation(t *testing.T) {
	mapped := Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}
	ordinary := Account{ID: 43, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}
	cfg := &config.Config{RunMode: config.RunModeSimple, TokenHive: tokenHiveAnthropicConfigForTest(mapped.ID)}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	registry, err := NewTokenHiveRegistry(cfg.TokenHive, []Account{mapped})
	require.NoError(t, err)

	for _, tt := range []struct {
		name      string
		account   Account
		wantWrite bool
	}{
		{name: "mapped", account: mapped, wantWrite: false},
		{name: "ordinary", account: ordinary, wantWrite: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const sessionHash = "preexisting-sticky"
			cache := &tokenHiveStickyCache{schedulerTestGatewayCache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{sessionHash: tt.account.ID}}}
			svc := &GatewayService{
				accountRepo:        tokenHiveAnthropicSchedulerRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{tt.account}}},
				cache:              cache,
				cfg:                cfg,
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				tokenHiveRegistry:  registry,
			}

			selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, sessionHash, "", nil, "", 0)
			require.NoError(t, err)
			require.Equal(t, tt.account.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if tt.wantWrite {
				require.Equal(t, 1, cache.refreshedSessions[sessionHash])
			} else {
				require.NotContains(t, cache.refreshedSessions, sessionHash)
			}

			require.NoError(t, svc.deleteGatewayStickySession(context.Background(), nil, sessionHash, tt.account.ID))
			if tt.wantWrite {
				require.Equal(t, 1, cache.deletedSessions[sessionHash])
			} else {
				require.NotContains(t, cache.deletedSessions, sessionHash)
			}
		})
	}
}

func TestTokenHiveAnthropicTrustedExecutionErrorIsSanitized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	header := http.Header{"Content-Type": []string{"application/json"}}
	header.Set("X-TokenHive-Execution-Error", "1")
	upstream := &tokenHiveAnthropicUpstream{response: &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"EXECUTION_SECRET","source":"bee","type":"credential_unavailable"}}`)),
	}}
	svc, account, c, recorder := newTokenHiveAnthropicService(t, upstream)
	body := []byte(`{"model":"claude-sonnet-4","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.ErrorContains(t, err, "tokenhive execution error")
	require.Equal(t, 1, upstream.calls)
	require.NotContains(t, recorder.Body.String(), "EXECUTION_SECRET")
	require.JSONEq(t, `{"type":"error","error":{"message":"TokenHive execution failed","source":"bee","type":"credential_unavailable"}}`, recorder.Body.String())
}

func TestOrdinaryAnthropicOAuthAndSetupCustomBaseBypassTokenHive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
		for _, route := range []string{"messages_json", "messages_sse", "count_tokens"} {
			t.Run(accountType+"/"+route, func(t *testing.T) {
				mappedAccount := Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
				tokenHiveCfg := tokenHiveAnthropicConfigForTest(mappedAccount.ID)
				registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{mappedAccount})
				require.NoError(t, err)
				proxyID := int64(9)
				credentials := map[string]any{"access_token": "ORDINARY_ACCESS_TOKEN"}
				extra := map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://ordinary-relay.example"}
				if accountType == AccountTypeAPIKey {
					credentials = map[string]any{"api_key": "ORDINARY_API_KEY", "base_url": "https://ordinary-relay.example"}
					extra = map[string]any{"anthropic_passthrough": true}
				}
				account := &Account{
					ID: 43, Name: "ordinary-anthropic", Platform: PlatformAnthropic, Type: accountType, Concurrency: 1,
					Credentials: credentials,
					Extra:       extra,
					ProxyID:     &proxyID,
					Proxy:       &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 18090, Username: "relay", Password: "secret"},
					Status:      StatusActive, Schedulable: true,
				}
				responseBody := `{"id":"msg_ordinary","type":"message","role":"assistant","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`
				contentType := "application/json"
				if route == "messages_sse" {
					contentType = "text/event-stream"
					responseBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ordinary\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				if route == "count_tokens" {
					responseBody = `{"input_tokens":2}`
				}
				upstream := &tokenHiveAnthropicUpstream{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewBufferString(responseBody))}}
				cfg := &config.Config{TokenHive: tokenHiveCfg, Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}
				svc := &GatewayService{cfg: cfg, tokenHiveRegistry: registry, httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				path := "/v1/messages"
				if route == "count_tokens" {
					path += "/count_tokens"
				}
				c.Request = httptest.NewRequest(http.MethodPost, path, nil)
				c.Request.Header.Set("User-Agent", "claude-cli/2.1.81 (external, cli)")
				c.Set("api_key", &APIKey{ID: 77})
				stream := route == "messages_sse"
				body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4","stream":%t,"max_tokens":32,"metadata":{"user_id":"user_123_account__session_123"},"messages":[{"role":"user","content":"hello"}]}`, stream))
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				require.NoError(t, err)

				if route == "count_tokens" {
					err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
				} else {
					_, err = svc.Forward(context.Background(), c, account, parsed)
				}
				require.NoError(t, err)
				require.Equal(t, 1, upstream.calls)
				if accountType == AccountTypeAPIKey {
					require.Equal(t, account.Proxy.URL(), upstream.proxyURL)
				} else {
					require.Empty(t, upstream.proxyURL)
				}
				require.Equal(t, "https", upstream.request.URL.Scheme)
				require.Equal(t, "ordinary-relay.example", upstream.request.URL.Host)
				wantPath := "/v1/messages"
				if route == "count_tokens" {
					wantPath += "/count_tokens"
				}
				require.Equal(t, wantPath, upstream.request.URL.Path)
				require.Equal(t, "true", upstream.request.URL.Query().Get("beta"))
				if accountType == AccountTypeAPIKey {
					require.Empty(t, upstream.request.URL.Query().Get("proxy"))
				} else {
					require.Equal(t, account.Proxy.URL(), upstream.request.URL.Query().Get("proxy"))
				}
				require.Empty(t, upstream.request.Header.Get(TokenHiveHeaderUpstreamType))
				require.Empty(t, upstream.request.Header.Get(TokenHiveHeaderTenantKey))
				if accountType == AccountTypeAPIKey {
					require.Equal(t, "ORDINARY_API_KEY", getHeaderRaw(upstream.request.Header, "x-api-key"))
					require.Empty(t, getHeaderRaw(upstream.request.Header, "authorization"))
				} else {
					require.Equal(t, "Bearer ORDINARY_ACCESS_TOKEN", getHeaderRaw(upstream.request.Header, "authorization"))
				}
				require.False(t, IsTokenHiveInternalHandoff(upstream.request.Context()))
			})
		}
	}
}
