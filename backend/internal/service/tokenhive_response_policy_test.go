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

	t.Run("partial usage remains billable", func(t *testing.T) {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		userRepo := &openAIRecordUsageUserRepoStub{}
		billing := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
		partialResult := *result
		partialResult.RequestID = "rid-tokenhive-partial"
		partialResult.Usage = OpenAIUsage{InputTokens: 9, OutputTokens: 2, CacheReadInputTokens: 1}
		require.NoError(t, billing.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: &partialResult, APIKey: &APIKey{ID: 707, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 708}, Account: account,
		}))
		require.Equal(t, 1, usageRepo.calls)
		require.Equal(t, 1, userRepo.deductCalls)
		require.Equal(t, "rid-tokenhive-partial", usageRepo.lastLog.RequestID)
		require.Equal(t, 8, usageRepo.lastLog.InputTokens)
		require.Equal(t, 1, usageRepo.lastLog.CacheReadTokens)
		require.Equal(t, 2, usageRepo.lastLog.OutputTokens)
	})
}
