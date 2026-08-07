package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenHivePolicyAccountRepo struct {
	AccountRepository
	tempUnschedulable int
	setError          int
	rateLimited       int
}

func (r *tokenHivePolicyAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.tempUnschedulable++
	return nil
}

func (r *tokenHivePolicyAccountRepo) SetError(context.Context, int64, string) error {
	r.setError++
	return nil
}

func (r *tokenHivePolicyAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.rateLimited++
	return nil
}

type tokenHivePolicyScheduler struct {
	reports  int
	switches int
}

type tokenHivePolicySettingRepo struct {
	SettingRepository
	value string
}

func (r *tokenHivePolicySettingRepo) GetValue(context.Context, string) (string, error) {
	return r.value, nil
}

func (s *tokenHivePolicyScheduler) Select(context.Context, OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
}

func (s *tokenHivePolicyScheduler) ReportResult(int64, bool, *int) { s.reports++ }
func (s *tokenHivePolicyScheduler) ReportSwitch()                  { s.switches++ }
func (s *tokenHivePolicyScheduler) SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	return OpenAIAccountSchedulerMetricsSnapshot{}
}

func tokenHiveDedicatedPolicy() TokenHiveResponsePolicy {
	return TokenHiveResponsePolicy{Dedicated: true}
}

func ordinaryTokenHivePolicy() TokenHiveResponsePolicy {
	return TokenHiveResponsePolicy{
		AllowSameAccountRetry:  true,
		AllowAccountFailover:   true,
		AllowAccountMutation:   true,
		AllowRuntimeBlock:      true,
		AllowSchedulerFeedback: true,
	}
}

func newTokenHivePolicyContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func enableTokenHivePolicyTestScheduler(t *testing.T) {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
		enabled:   true,
		expiresAt: time.Now().Add(time.Minute).UnixNano(),
	})
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
}

func TestTokenHiveResponsePolicySuppressesTransportAndUpstreamSideEffects(t *testing.T) {
	enableTokenHivePolicyTestScheduler(t)
	repo := &tokenHivePolicyAccountRepo{}
	scheduler := &tokenHivePolicyScheduler{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimitService, openaiScheduler: scheduler}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 81, Name: "tokenhive", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	policy := tokenHiveDedicatedPolicy()
	c := newTokenHivePolicyContext()

	_ = svc.handleOpenAIUpstreamTransportErrorWithPolicy(context.Background(), c, account,
		errors.New("proxyconnect tcp: connection refused"), false, policy)
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		_ = svc.handleOpenAIAccountUpstreamErrorWithPolicy(context.Background(), account, status, http.Header{},
			[]byte(`{"error":{"message":"upstream failed"}}`), policy, "gpt-5.4")
	}
	_ = svc.handleOpenAIUpstreamTransportErrorWithPolicy(context.Background(), c, account,
		errors.New("unexpected EOF while reading response stream"), false, policy)
	svc.ReportOpenAIAccountScheduleResultWithPolicy(policy, account.ID, "gpt-5.4", false, nil)
	svc.RecordOpenAIAccountSwitchWithPolicy(policy)

	require.Zero(t, repo.tempUnschedulable)
	require.Zero(t, repo.setError)
	require.Zero(t, repo.rateLimited)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-5.4"))
	require.Zero(t, scheduler.reports)
	require.Zero(t, scheduler.switches)
}

func TestOrdinaryAccountResponsePolicyPreservesSideEffects(t *testing.T) {
	enableTokenHivePolicyTestScheduler(t)
	repo := &tokenHivePolicyAccountRepo{}
	scheduler := &tokenHivePolicyScheduler{}
	rateLimitService := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rateLimitService, openaiScheduler: scheduler}
	rateLimitService.SetAccountRuntimeBlocker(svc)
	account := &Account{ID: 82, Name: "ordinary", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	policy := ordinaryTokenHivePolicy()
	c := newTokenHivePolicyContext()

	_ = svc.handleOpenAIUpstreamTransportErrorWithPolicy(context.Background(), c, account,
		errors.New("proxyconnect tcp: connection refused"), false, policy)
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		_ = svc.handleOpenAIAccountUpstreamErrorWithPolicy(context.Background(), account, status, http.Header{},
			[]byte(`{"error":{"message":"upstream failed"}}`), policy, "gpt-5.4")
	}
	_ = svc.handleOpenAIUpstreamTransportErrorWithPolicy(context.Background(), c, account,
		errors.New("unexpected EOF while reading response stream"), false, policy)
	svc.ReportOpenAIAccountScheduleResultWithPolicy(policy, account.ID, "gpt-5.4", false, nil)
	svc.RecordOpenAIAccountSwitchWithPolicy(policy)

	require.Equal(t, 1, repo.tempUnschedulable)
	require.Greater(t, repo.setError, 0)
	require.Greater(t, repo.rateLimited, 0)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 1, scheduler.reports)
	require.Equal(t, 1, scheduler.switches)
}

func TestTokenHiveResponsePolicyPreservesUsageBillingAndPartialUsageSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("api_key", &APIKey{ID: 707})

	upstreamResponse := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid-tokenhive-complete"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp-tokenhive","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3}}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
	}
	upstream := &httpUpstreamRecorder{resp: upstreamResponse}
	tokenHiveCfg := tokenHiveConfigForTest(81)
	registry, err := NewTokenHiveRegistry(tokenHiveCfg, []Account{{ID: 81, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}})
	require.NoError(t, err)
	forwarder := &OpenAIGatewayService{
		cfg:                  &config.Config{TokenHive: tokenHiveCfg},
		httpUpstream:         upstream,
		tokenHiveRegistry:    registry,
		responseHeaderFilter: compileResponseHeaderFilter(&config.Config{}),
	}
	account := &Account{ID: 81, Name: "tokenhive", Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "external-key", "base_url": "https://api.openai.com"}}
	policy := forwarder.ResolveTokenHiveResponsePolicy(account)
	require.True(t, policy.Dedicated)

	result, err := forwarder.ForwardWithResponsePolicy(context.Background(), c, account, body, policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.CacheReadInputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)

	t.Run("balance billing and usage log", func(t *testing.T) {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		userRepo := &openAIRecordUsageUserRepoStub{}
		subRepo := &openAIRecordUsageSubRepoStub{}
		billing := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)
		require.NoError(t, billing.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: result, APIKey: &APIKey{ID: 707, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 708}, Account: account,
		}))
		require.Equal(t, 1, usageRepo.calls)
		require.Equal(t, 1, userRepo.deductCalls)
		require.Zero(t, subRepo.incrementCalls)
		require.Equal(t, 8, usageRepo.lastLog.InputTokens)
		require.Equal(t, 3, usageRepo.lastLog.CacheReadTokens)
		require.Equal(t, 7, usageRepo.lastLog.OutputTokens)
	})

	t.Run("subscription billing", func(t *testing.T) {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		userRepo := &openAIRecordUsageUserRepoStub{}
		subRepo := &openAIRecordUsageSubRepoStub{}
		billing := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)
		subscription := &UserSubscription{ID: 709}
		require.NoError(t, billing.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: result,
			APIKey: &APIKey{ID: 707, Group: &Group{SubscriptionType: SubscriptionTypeSubscription, RateMultiplier: 1}},
			User:   &User{ID: 708}, Account: account, Subscription: subscription,
		}))
		require.Equal(t, 1, usageRepo.calls)
		require.Zero(t, userRepo.deductCalls)
		require.Equal(t, 1, subRepo.incrementCalls)
		require.Equal(t, BillingTypeSubscription, usageRepo.lastLog.BillingType)
	})

}

func TestTokenHiveResponsePolicyGuardsForwardCompatibilityRetries(t *testing.T) {
	tests := []struct {
		name         string
		body         []byte
		firstFailure string
	}{
		{
			name:         "invalid encrypted content",
			body:         []byte(`{"model":"gpt-5.4","stream":false,"input":[{"type":"reasoning","encrypted_content":"ciphertext","summary":[]}]}`),
			firstFailure: `{"error":{"code":"invalid_encrypted_content","message":"encrypted content was rejected"}}`,
		},
		{
			name:         "rejected max output tokens",
			body:         []byte(`{"model":"gpt-5.4","stream":false,"max_output_tokens":1024,"input":"hello"}`),
			firstFailure: `{"error":{"code":"unsupported_parameter","message":"Unsupported parameter: max_output_tokens","param":"max_output_tokens"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, dedicated := range []bool{true, false} {
				label := "ordinary"
				if dedicated {
					label = "dedicated"
				}
				t.Run(label, func(t *testing.T) {
					upstream := &httpUpstreamRecorder{responses: []*http.Response{
						{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tt.firstFailure))},
						{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp-retry","usage":{"input_tokens":1,"output_tokens":1}}`))},
					}}
					cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}
					account := &Account{ID: 81, Name: label, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
						Credentials: map[string]any{"api_key": "external-key", "base_url": "https://api.openai.com"}}
					var registry *TokenHiveRegistry
					if dedicated {
						tokenHiveCfg := tokenHiveConfigForTest(account.ID)
						cfg.TokenHive = tokenHiveCfg
						var err error
						registry, err = NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
						require.NoError(t, err)
					}
					svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tokenHiveRegistry: registry,
						responseHeaderFilter: compileResponseHeaderFilter(cfg)}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tt.body))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set("api_key", &APIKey{ID: 707})

					result, err := svc.ForwardWithResponsePolicy(context.Background(), c, account, tt.body, svc.ResolveTokenHiveResponsePolicy(account))

					if dedicated {
						require.Error(t, err)
						require.Nil(t, result)
						require.Len(t, upstream.requests, 1, "dedicated handoff is at-most-once")
						return
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.requests, 2, "ordinary compatibility retry must remain enabled")
				})
			}
		})
	}
}

func TestTokenHiveResponsePolicySuppressesProxyStreamCircuitFeedback(t *testing.T) {
	for _, dedicated := range []bool{true, false} {
		label := "ordinary"
		if dedicated {
			label = "dedicated"
		}
		t.Run(label, func(t *testing.T) {
			proxyID := int64(91)
			account := &Account{ID: 81, Name: label, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
				Credentials: map[string]any{"api_key": "external-key", "base_url": "https://api.openai.com"}}
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}, Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
			var registry *TokenHiveRegistry
			if dedicated {
				tokenHiveCfg := tokenHiveConfigForTest(account.ID)
				cfg.TokenHive = tokenHiveCfg
				var err error
				registry, err = NewTokenHiveRegistry(tokenHiveCfg, []Account{*account})
				require.NoError(t, err)
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: &openAIStreamReadThenErrorCloser{
					reader: strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
					err:    io.ErrUnexpectedEOF,
				},
			}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tokenHiveRegistry: registry,
				responseHeaderFilter: compileResponseHeaderFilter(cfg)}
			svc.openaiProxyStreamCircuit = newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
				failureThreshold: 1, failureWindow: time.Minute, quarantineTTL: time.Minute, maxEntries: 8,
			})
			body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hello"}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("api_key", &APIKey{ID: 707})

			_, err := svc.ForwardWithResponsePolicy(context.Background(), c, account, body, svc.ResolveTokenHiveResponsePolicy(account))

			require.ErrorContains(t, err, "stream read error")
			if dedicated {
				require.False(t, svc.isOpenAIProxyStreamQuarantined(context.Background(), account))
			} else {
				require.True(t, svc.isOpenAIProxyStreamQuarantined(context.Background(), account))
			}
		})
	}
}

func TestTokenHiveResponsePolicySuppressesStreamTimeoutMutation(t *testing.T) {
	for _, dedicated := range []bool{true, false} {
		label := "ordinary"
		policy := ordinaryTokenHivePolicy()
		if dedicated {
			label = "dedicated"
			policy = tokenHiveDedicatedPolicy()
		}
		t.Run(label, func(t *testing.T) {
			repo := &tokenHivePolicyAccountRepo{}
			proxyID := int64(91)
			cfg := &config.Config{Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: 1,
				MaxLineSize:               defaultMaxLineSize,
				OpenAIProxyStreamCircuit: config.GatewayOpenAIProxyStreamCircuitConfig{
					FailureThreshold: 1, WindowSeconds: 60, TTLSeconds: 60,
				},
			}}
			rateLimitService := NewRateLimitService(repo, nil, cfg, nil, nil)
			rateLimitService.SetSettingService(NewSettingService(&tokenHivePolicySettingRepo{value: `{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":1}`}, cfg))
			svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, rateLimitService: rateLimitService}
			rateLimitService.SetAccountRuntimeBlocker(svc)
			account := &Account{ID: 81, Name: label, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, ProxyID: &proxyID}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			reader, writer := io.Pipe()
			t.Cleanup(func() {
				_ = writer.Close()
				_ = reader.Close()
			})
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}

			_, err := svc.handleStreamingResponseWithReasoningAndPolicy(
				context.Background(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4", "", policy,
			)

			require.ErrorContains(t, err, "stream data interval timeout")
			if dedicated {
				require.Zero(t, repo.tempUnschedulable)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			} else {
				require.Equal(t, 1, repo.tempUnschedulable)
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			}
		})
	}
}

func TestTokenHiveResponsePolicySuppressesFirstOutputTimeoutMutation(t *testing.T) {
	for _, dedicated := range []bool{true, false} {
		label := "ordinary"
		policy := ordinaryTokenHivePolicy()
		if dedicated {
			label = "dedicated"
			policy = tokenHiveDedicatedPolicy()
		}
		t.Run(label, func(t *testing.T) {
			repo := &tokenHivePolicyAccountRepo{}
			proxyID := int64(91)
			cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIProxyStreamCircuit: config.GatewayOpenAIProxyStreamCircuitConfig{
				FailureThreshold: 1, WindowSeconds: 60, TTLSeconds: 60,
			}}}
			rateLimitService := NewRateLimitService(repo, nil, cfg, nil, nil)
			rateLimitService.SetSettingService(NewSettingService(&tokenHivePolicySettingRepo{value: `{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":1,"threshold_count":1,"threshold_window_minutes":1}`}, cfg))
			svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, rateLimitService: rateLimitService}
			rateLimitService.SetAccountRuntimeBlocker(svc)
			account := &Account{ID: 81, Name: label, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, ProxyID: &proxyID}
			c := newTokenHivePolicyContext()

			svc.newOpenAIFirstOutputTimeoutErrorWithPolicy(
				context.Background(), c, account, time.Now(), "gpt-5.4", "", time.Second, "semantic_output", nil, policy,
			)

			if dedicated {
				require.Zero(t, repo.tempUnschedulable)
				require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
			} else {
				require.Equal(t, 1, repo.tempUnschedulable)
				require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
			}
		})
	}
}
