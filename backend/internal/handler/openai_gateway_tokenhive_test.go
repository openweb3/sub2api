package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type tokenHiveHandlerAccountRepo struct {
	service.AccountRepository
	mu         sync.Mutex
	accounts   []service.Account
	selections int
}

type tokenHiveHandlerSchedulerSettingRepo struct {
	service.SettingRepository
}

func (tokenHiveHandlerSchedulerSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if key == "openai_advanced_scheduler_enabled" {
			values[key] = "true"
		}
	}
	return values, nil
}

func (tokenHiveHandlerSchedulerSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (r *tokenHiveHandlerAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return r.listAccounts(), nil
}

func (r *tokenHiveHandlerAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return r.listAccounts(), nil
}

func (r *tokenHiveHandlerAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return r.listAccounts(), nil
}

func (r *tokenHiveHandlerAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, platforms []string) ([]service.Account, error) {
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	accounts := r.listAccounts()
	filtered := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if _, ok := allowed[account.Platform]; ok {
			filtered = append(filtered, account)
		}
	}
	return filtered, nil
}

func (r *tokenHiveHandlerAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r *tokenHiveHandlerAccountRepo) listAccounts() []service.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selections++
	return append([]service.Account(nil), r.accounts...)
}

func (r *tokenHiveHandlerAccountRepo) selectionCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.selections
}

type tokenHiveHandlerUpstream struct {
	service.HTTPUpstream
	mu      sync.Mutex
	account []int64
	mode    string
}

type tokenHiveHandlerReadThenErrorCloser struct {
	reader io.Reader
	err    error
}

func (r *tokenHiveHandlerReadThenErrorCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) && r.err != nil {
		err = r.err
		r.err = nil
	}
	return n, err
}

func (r *tokenHiveHandlerReadThenErrorCloser) Close() error { return nil }

func (u *tokenHiveHandlerUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.account = append(u.account, accountID)
	u.mu.Unlock()
	switch u.mode {
	case "anthropic_count_tokens_success":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"input_tokens":17}`))}, nil
	case "openai_input_tokens_success":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"input_tokens":23}`))}, nil
	case "codex_manifest_success":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"models":[{"slug":"gpt-5.6-sol"}]}`))}, nil
	case "images_empty":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"created":1710000021,"data":[]}`))}, nil
	case "images_error_event":
		return tokenHiveHandlerImagesErrorResponse(http.StatusBadRequest, "image_generation_user_error", "moderation_blocked", "Your request was blocked by the safety system"), nil
	case "images_response_failed":
		return tokenHiveHandlerImagesErrorResponse(http.StatusBadRequest, "image_generation_user_error", "moderation_blocked", "The safety system rejected this image"), nil
	case "images_incomplete_retryable":
		return tokenHiveHandlerImagesErrorResponse(http.StatusBadGateway, "incomplete_error", "response_incomplete", "Upstream image generation incomplete: max_output_tokens"), nil
	case "images_incomplete_policy":
		return tokenHiveHandlerImagesErrorResponse(http.StatusBadRequest, "image_generation_user_error", "response_incomplete", "Upstream image generation incomplete: content_filter"), nil
	case "images_completed_refusal":
		return tokenHiveHandlerImagesErrorResponse(http.StatusBadRequest, "image_generation_user_error", "content_policy_violation", "This request conflicts with our image safety policy"), nil
	case "trusted_execution_error_response":
		header := http.Header{"Content-Type": []string{"application/json"}}
		header.Set("X-TokenHive-Execution-Error", "1")
		return &http.Response{StatusCode: http.StatusBadGateway, Header: header,
			Body: io.NopCloser(bytes.NewBufferString(`{"error":{"message":"proxy request failed HANDLER_EXECUTION_SECRET","source":"bee","type":"credential_unavailable"}}`))}, nil
	case "images_stream_success":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"cGFydGlhbA==\"}\n\n" +
				"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"ZmluYWw=\",\"usage\":{\"images\":1}}\n\n"))}, nil
	case "images_transport_error", "alpha_transport_error":
		return nil, errors.New("fixture auxiliary transport failure")
	case "alpha_success":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"output":"fixture alpha result"}`))}, nil
	case "alpha_500":
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(`{"error":{"type":"server_error","message":"temporary alpha failure"}}`))}, nil
	case "completed_then_read_error":
		body := strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp-tokenhive-billing","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3}}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"rid-tokenhive-billing"},
		}, Body: &tokenHiveHandlerReadThenErrorCloser{reader: strings.NewReader(body), err: io.ErrUnexpectedEOF}}, nil
	case "execution_error":
		return nil, errors.New("tokenhive execution error: bee disconnected")
	case "response_failed":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(bytes.NewBufferString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"provider failed\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n"))}, nil
	case "401":
		return tokenHiveHandlerErrorResponse(http.StatusUnauthorized), nil
	case "429":
		return tokenHiveHandlerErrorResponse(http.StatusTooManyRequests), nil
	case "network_read_error":
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &tokenHiveHandlerReadErrorBody{
			payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
		}}, nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"temporary upstream failure"}}`)),
	}, nil
}

type tokenHiveHandlerUsageLogRepo struct {
	service.UsageLogRepository
	calls int
	last  *service.UsageLog
}

func (r *tokenHiveHandlerUsageLogRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.calls++
	r.last = log
	return true, nil
}

type tokenHiveHandlerUserRepo struct {
	service.UserRepository
	deductCalls int
	lastAmount  float64
}

func (r *tokenHiveHandlerUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	return &service.User{ID: id, Balance: 100, Status: service.StatusActive}, nil
}

func (r *tokenHiveHandlerUserRepo) DeductBalance(_ context.Context, _ int64, amount float64) error {
	r.deductCalls++
	r.lastAmount = amount
	return nil
}

type tokenHiveHandlerSubscriptionRepo struct {
	service.UserSubscriptionRepository
	incrementCalls int
	lastAmount     float64
	active         *service.UserSubscription
}

func (r *tokenHiveHandlerSubscriptionRepo) GetActiveByUserIDAndGroupID(_ context.Context, _, _ int64) (*service.UserSubscription, error) {
	return r.active, nil
}

func (r *tokenHiveHandlerSubscriptionRepo) IncrementUsage(_ context.Context, _ int64, amount float64) error {
	r.incrementCalls++
	r.lastAmount = amount
	return nil
}

type tokenHiveHandlerBillingSpies struct {
	usage        *tokenHiveHandlerUsageLogRepo
	user         *tokenHiveHandlerUserRepo
	subscription *tokenHiveHandlerSubscriptionRepo
	upstream     *tokenHiveHandlerUpstream
}

func tokenHiveHandlerErrorResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(`{"error":{"message":"upstream failed"}}`))}
}

func tokenHiveHandlerImagesErrorResponse(status int, errType, code, message string) *http.Response {
	body := fmt.Sprintf(`{"error":{"type":%q,"code":%q,"message":%q}}`, errType, code, message)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(body))}
}

type tokenHiveHandlerReadErrorBody struct {
	payload []byte
	done    bool
}

func (r *tokenHiveHandlerReadErrorBody) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.payload), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (*tokenHiveHandlerReadErrorBody) Close() error { return nil }

func (u *tokenHiveHandlerUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.account)
}

func (u *tokenHiveHandlerUpstream) accounts() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.account...)
}

func newTokenHiveResponsePolicyHandler(t *testing.T, dedicated bool) (*OpenAIGatewayHandler, *tokenHiveHandlerAccountRepo, *tokenHiveHandlerUpstream) {
	return newTokenHiveResponsePolicyHandlerWithUpstream(t, dedicated, &tokenHiveHandlerUpstream{})
}

func newTokenHiveResponsePolicyHandlerWithUpstream(t *testing.T, dedicated bool, upstream *tokenHiveHandlerUpstream, schedulerEnabled ...bool) (*OpenAIGatewayHandler, *tokenHiveHandlerAccountRepo, *tokenHiveHandlerUpstream) {
	t.Helper()
	accounts := []service.Account{
		{ID: 1, Name: "first", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 0,
			Credentials: map[string]any{"api_key": "first-key", "pool_mode": true, "pool_mode_retry_count": float64(1), "pool_mode_retry_status_codes": []any{float64(500)}}},
		{ID: 2, Name: "second", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
			Credentials: map[string]any{"api_key": "second-key"}},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	if dedicated {
		cfg.TokenHive = config.TokenHiveConfig{
			Enabled:       true,
			ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
			TenantHMACKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
			Accounts:      map[int64]string{1: service.UpstreamTypeOpenAICodexOAuth},
		}
	}
	repo := &tokenHiveHandlerAccountRepo{accounts: accounts}
	var rateLimits *service.RateLimitService
	if len(schedulerEnabled) > 0 && schedulerEnabled[0] {
		settings := service.NewSettingService(tokenHiveHandlerSchedulerSettingRepo{}, cfg)
		require.NoError(t, settings.UpdateSettings(context.Background(), &service.SystemSettings{OpenAIAdvancedSchedulerEnabled: true}))
		t.Cleanup(func() {
			require.NoError(t, settings.UpdateSettings(context.Background(), &service.SystemSettings{}))
		})
		rateLimits = service.NewRateLimitService(repo, nil, cfg, nil, nil)
		rateLimits.SetSettingService(settings)
	}
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, rateLimits, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 2
	return h, repo, upstream
}

func runTokenHiveResponsePolicyRequest(t *testing.T, h *OpenAIGatewayHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","stream":false,"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 11, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowMessagesDispatch: true}, User: &service.User{ID: 12}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 12})
	h.Responses(c)
	return rec
}

func runTokenHiveOpenAICompatibilityPolicyRequest(t *testing.T, h *OpenAIGatewayHandler, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	body := `{"model":"gpt-5.4","stream":false,"messages":[{"role":"user","content":"hello"}]}`
	if endpoint == "/v1/messages" {
		body = `{"model":"gpt-5.4","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 11, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, AllowMessagesDispatch: true}, User: &service.User{ID: 12}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 12})
	if endpoint == "/v1/messages" {
		h.Messages(c)
	} else {
		h.ChatCompletions(c)
	}
	return rec
}

func newTokenHiveUsagePolicyHandler(t *testing.T, dedicated bool, subscriptionBilling bool, upstreamMode string) (*OpenAIGatewayHandler, *tokenHiveHandlerBillingSpies) {
	t.Helper()
	account := service.Account{ID: 1, Name: "billing-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{91},
		Credentials: map[string]any{"api_key": "external-key", "base_url": "https://api.openai.com"}}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	if dedicated {
		cfg.TokenHive = config.TokenHiveConfig{
			Enabled:       true,
			ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
			TenantHMACKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
			Accounts:      map[int64]string{account.ID: service.UpstreamTypeOpenAICodexOAuth},
		}
	}
	accountRepo := &tokenHiveHandlerAccountRepo{accounts: []service.Account{account}}
	spies := &tokenHiveHandlerBillingSpies{
		usage: &tokenHiveHandlerUsageLogRepo{},
		user:  &tokenHiveHandlerUserRepo{},
		subscription: &tokenHiveHandlerSubscriptionRepo{active: &service.UserSubscription{
			ID: 21, UserID: 12, GroupID: 91, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour),
		}},
		upstream: &tokenHiveHandlerUpstream{mode: upstreamMode},
	}
	billingCache := service.NewBillingCacheService(nil, spies.user, spies.subscription, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(
		accountRepo, spies.usage, nil, spies.user, spies.subscription, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache,
		spies.upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 0
	return h, spies
}

func runTokenHiveUsagePolicyRequest(t *testing.T, h *OpenAIGatewayHandler, subscriptionBilling bool) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard}
	if subscriptionBilling {
		group.SubscriptionType = service.SubscriptionTypeSubscription
	}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	if subscriptionBilling {
		c.Set(string(middleware2.ContextKeySubscription), &service.UserSubscription{
			ID: 21, UserID: user.ID, GroupID: groupID, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour),
		})
	}
	h.Responses(c)
	return rec
}

func runTokenHiveImagesUsagePolicyRequest(t *testing.T, h *OpenAIGatewayHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard, AllowImageGeneration: true}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"model":"gpt-image-2","prompt":"draw a cat","n":3,"response_format":"b64_json"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	h.Images(c)
	return rec
}

func runTokenHiveAuxiliaryPolicyRequest(t *testing.T, h *OpenAIGatewayHandler, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard, AllowImageGeneration: true}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	body := `{"id":"search-fixture","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`
	if endpoint == "/v1/images/generations" {
		body = `{"model":"gpt-image-2","prompt":"draw a cat","stream":true,"response_format":"b64_json"}`
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	if endpoint == "/v1/images/generations" {
		h.Images(c)
	} else {
		h.AlphaSearch(c)
	}
	return rec
}

func runTokenHiveExecutionErrorRequest(t *testing.T, h *OpenAIGatewayHandler, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard, AllowMessagesDispatch: true}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	body := `{"id":"search-fixture","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`
	if endpoint == "/v1/messages/count_tokens" {
		body = `{"model":"claude-opus-4-1","messages":[{"role":"user","content":"hello"}]}`
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	if endpoint == "/v1/messages/count_tokens" {
		h.CountTokens(c)
	} else {
		h.AlphaSearch(c)
	}
	return rec
}

func newTokenHiveAnthropicCountTokensUsageHandler(t *testing.T) (*GatewayHandler, *tokenHiveHandlerBillingSpies) {
	t.Helper()
	account := service.Account{
		ID: 42, Name: "anthropic-count-tokens", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{91}, Concurrency: 1,
		Credentials: map[string]any{
			"api_key":       "virtual-account-key",
			"model_mapping": map[string]any{"claude-sonnet-4": "claude-sonnet-4-20250514"},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.TokenHive = config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
		Accounts:      map[int64]string{account.ID: service.UpstreamTypeAnthropicOAuth},
	}
	spies := &tokenHiveHandlerBillingSpies{
		usage:        &tokenHiveHandlerUsageLogRepo{},
		user:         &tokenHiveHandlerUserRepo{},
		subscription: &tokenHiveHandlerSubscriptionRepo{},
		upstream:     &tokenHiveHandlerUpstream{mode: "anthropic_count_tokens_success"},
	}
	billingCache := service.NewBillingCacheService(nil, spies.user, spies.subscription, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewGatewayService(
		&tokenHiveHandlerAccountRepo{accounts: []service.Account{account}}, nil,
		spies.usage, nil, spies.user, spies.subscription, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, nil,
		spies.upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewGatewayHandler(
		gateway, nil, nil, nil, nil, service.NewConcurrencyService(nil), billingCache,
		nil, nil, nil, nil, nil, nil, cfg, nil,
	)
	return h, spies
}

func runTokenHiveAnthropicCountTokensUsageRequest(t *testing.T, h *GatewayHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive, Hydrated: true}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewBufferString(
		`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`,
	))
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	h.CountTokens(c)
	return recorder
}

func runTokenHiveCodexModelsUsageRequest(t *testing.T, h *OpenAIGatewayHandler) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true}
	user := &service.User{ID: 12, Balance: 100}
	apiKey := &service.APIKey{ID: 11, UserID: user.ID, GroupID: &groupID, Group: group, User: user, Status: service.StatusActive}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.145.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: user.ID})
	h.CodexModels(c)
	return recorder
}

func TestTokenHiveAnthropicCountTokensDoesNotRecordUsage(t *testing.T) {
	h, spies := newTokenHiveAnthropicCountTokensUsageHandler(t)
	recorder := runTokenHiveAnthropicCountTokensUsageRequest(t, h)

	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
	require.JSONEq(t, `{"input_tokens":17}`, recorder.Body.String())
	require.Equal(t, 1, spies.upstream.callCount())
	require.Zero(t, spies.usage.calls)
	require.Zero(t, spies.user.deductCalls)
	require.Zero(t, spies.subscription.incrementCalls)
}

func TestTokenHiveResponsesInputTokensDoesNotRecordUsage(t *testing.T) {
	h, spies := newTokenHiveUsagePolicyHandler(t, true, false, "openai_input_tokens_success")
	recorder := runTokenHiveExecutionErrorRequest(t, h, "/v1/messages/count_tokens")

	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
	require.JSONEq(t, `{"input_tokens":23}`, recorder.Body.String())
	require.Equal(t, 1, spies.upstream.callCount())
	require.Zero(t, spies.usage.calls)
	require.Zero(t, spies.user.deductCalls)
	require.Zero(t, spies.subscription.incrementCalls)
}

func TestTokenHiveCodexModelsManifestDoesNotRecordUsage(t *testing.T) {
	h, spies := newTokenHiveUsagePolicyHandler(t, true, false, "codex_manifest_success")
	recorder := runTokenHiveCodexModelsUsageRequest(t, h)

	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
	require.JSONEq(t, `{"models":[{"slug":"gpt-5.6-sol"}]}`, recorder.Body.String())
	require.Equal(t, 1, spies.upstream.callCount())
	require.Zero(t, spies.usage.calls)
	require.Zero(t, spies.user.deductCalls)
	require.Zero(t, spies.subscription.incrementCalls)
}

func TestTokenHiveResponsePolicyPreventsSameAccountRetryAndFailover(t *testing.T) {
	h, repo, upstream := newTokenHiveResponsePolicyHandler(t, true)
	rec := runTokenHiveResponsePolicyRequest(t, h)

	require.Equal(t, 1, upstream.callCount(), "status=%d body=%s", rec.Code, rec.Body.String())
	require.Equal(t, []int64{1}, upstream.accounts())
	require.Equal(t, 1, repo.selectionCount())
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestOrdinaryAccountResponsePolicyPreservesRetryAndFailover(t *testing.T) {
	h, repo, upstream := newTokenHiveResponsePolicyHandler(t, false)
	rec := runTokenHiveResponsePolicyRequest(t, h)

	require.Greater(t, upstream.callCount(), 1, "status=%d body=%s", rec.Code, rec.Body.String())
	require.Greater(t, repo.selectionCount(), 1)
	require.Contains(t, upstream.accounts(), int64(2), "ordinary account policy must preserve account switching")
}

func TestTokenHiveResponsePolicyDedicatedFailureStimuliDoNotRetry(t *testing.T) {
	for _, mode := range []string{"execution_error", "response_failed", "401", "429", "network_read_error"} {
		t.Run(mode, func(t *testing.T) {
			upstream := &tokenHiveHandlerUpstream{mode: mode}
			h, repo, _ := newTokenHiveResponsePolicyHandlerWithUpstream(t, true, upstream)
			_ = runTokenHiveResponsePolicyRequest(t, h)
			require.Equal(t, 1, upstream.callCount(), "dedicated %s must make at most one upstream attempt", mode)
			require.Equal(t, 1, repo.selectionCount(), "dedicated %s must not reselect an account", mode)
		})
	}
}

func TestTokenHiveOpenAICompatibilityHandlersDoNotRetryResponseFailed(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			upstream := &tokenHiveHandlerUpstream{mode: "response_failed"}
			h, repo, _ := newTokenHiveResponsePolicyHandlerWithUpstream(t, true, upstream)

			recorder := runTokenHiveOpenAICompatibilityPolicyRequest(t, h, endpoint)

			require.Equal(t, 1, upstream.callCount(), "status=%d body=%s", recorder.Code, recorder.Body.String())
			require.Equal(t, []int64{1}, upstream.accounts())
			require.Equal(t, 1, repo.selectionCount())
		})
	}
}

func TestOrdinaryOpenAICompatibilityHandlersPreserveFailover(t *testing.T) {
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(endpoint, func(t *testing.T) {
			h, repo, upstream := newTokenHiveResponsePolicyHandler(t, false)

			recorder := runTokenHiveOpenAICompatibilityPolicyRequest(t, h, endpoint)

			require.Greater(t, upstream.callCount(), 1, "status=%d body=%s", recorder.Code, recorder.Body.String())
			require.Greater(t, repo.selectionCount(), 1)
			require.Contains(t, upstream.accounts(), int64(2))
		})
	}
}

func TestTokenHiveResponsePolicyProfitVetoTerminatesWithoutFailover(t *testing.T) {
	h, repo, upstream := newTokenHiveResponsePolicyHandler(t, true)
	rate := 0.8
	repo.accounts[0].RateMultiplier = &rate
	repo.accounts[0].Extra = map[string]any{"upstream_billing_probe": map[string]any{
		"status":      service.UpstreamBillingProbeStatusOK,
		"received_at": time.Now().Add(-time.Minute),
		"fresh_until": time.Now().Add(time.Hour),
		"data":        map[string]any{"billing_scope": "token", "resolved_rate_multiplier": rate, "peak_rate_enabled": false},
	}}
	groupID := int64(91)
	group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard, ProfitControlEnabled: true, ProfitMinMargin: 0.5}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","stream":false,"input":"hello"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 11, GroupID: &groupID, Group: group, User: &service.User{ID: 12}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 12})

	h.Responses(c)

	require.Zero(t, upstream.callCount())
	require.Equal(t, 1, repo.selectionCount())
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestTokenHiveResponsePolicyPreservesRealHandlerUsageBilling(t *testing.T) {
	for _, subscriptionBilling := range []bool{false, true} {
		billingName := "balance"
		if subscriptionBilling {
			billingName = "subscription"
		}
		t.Run(billingName, func(t *testing.T) {
			var ordinaryLog *service.UsageLog
			for _, dedicated := range []bool{false, true} {
				policyName := "ordinary"
				if dedicated {
					policyName = "dedicated"
				}
				t.Run(policyName, func(t *testing.T) {
					h, spies := newTokenHiveUsagePolicyHandler(t, dedicated, subscriptionBilling, "completed_then_read_error")
					rec := runTokenHiveUsagePolicyRequest(t, h, subscriptionBilling)

					require.Equal(t, http.StatusOK, rec.Code, "upstream_calls=%d body=%s", spies.upstream.callCount(), rec.Body.String())
					require.Equal(t, 1, spies.usage.calls)
					require.NotNil(t, spies.usage.last)
					require.Equal(t, "rid-tokenhive-billing", spies.usage.last.RequestID)
					require.Equal(t, 8, spies.usage.last.InputTokens)
					require.Equal(t, 3, spies.usage.last.CacheReadTokens)
					require.Equal(t, 7, spies.usage.last.OutputTokens)
					if subscriptionBilling {
						require.Zero(t, spies.user.deductCalls)
						require.Equal(t, 1, spies.subscription.incrementCalls)
					} else {
						require.Equal(t, 1, spies.user.deductCalls)
						require.Zero(t, spies.subscription.incrementCalls)
					}
					if ordinaryLog == nil {
						copyLog := *spies.usage.last
						ordinaryLog = &copyLog
					} else {
						require.Equal(t, ordinaryLog.InputTokens, spies.usage.last.InputTokens)
						require.Equal(t, ordinaryLog.CacheReadTokens, spies.usage.last.CacheReadTokens)
						require.Equal(t, ordinaryLog.OutputTokens, spies.usage.last.OutputTokens)
						require.Equal(t, ordinaryLog.BillingType, spies.usage.last.BillingType)
						require.InDelta(t, ordinaryLog.ActualCost, spies.usage.last.ActualCost, 1e-12)
					}
				})
			}
		})
	}
}

func TestTokenHiveResponsePolicyPreservesResponseFailedBillingSemantics(t *testing.T) {
	const failedSSE = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"provider failed\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n"
	for _, dedicated := range []bool{false, true} {
		policyName := "ordinary"
		if dedicated {
			policyName = "dedicated"
		}
		t.Run(policyName, func(t *testing.T) {
			h, spies := newTokenHiveUsagePolicyHandler(t, dedicated, false, "response_failed")
			rec := runTokenHiveUsagePolicyRequest(t, h, false)

			require.Zero(t, spies.usage.calls, "current response.failed path does not submit ordinary RecordUsage")
			require.Zero(t, spies.user.deductCalls)
			require.Zero(t, spies.subscription.incrementCalls)
			if !dedicated {
				require.NotEqual(t, http.StatusOK, rec.Code)
			} else {
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
				require.Equal(t, failedSSE, rec.Body.String())
			}
		})
	}
}

func TestTokenHiveImagesZeroOutputHonorsOrdinaryAndDedicatedBillingBoundary(t *testing.T) {
	for _, dedicated := range []bool{false, true} {
		name := "ordinary"
		if dedicated {
			name = "dedicated"
		}
		t.Run(name, func(t *testing.T) {
			h, spies := newTokenHiveUsagePolicyHandler(t, dedicated, false, "images_empty")
			rec := runTokenHiveImagesUsagePolicyRequest(t, h)

			require.Equal(t, 1, spies.upstream.callCount())
			if dedicated {
				require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())
				require.Zero(t, spies.usage.calls)
				require.Zero(t, spies.user.deductCalls)
				require.Zero(t, spies.subscription.incrementCalls)
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.JSONEq(t, `{"created":1710000021,"data":[]}`, rec.Body.String())
			require.Equal(t, 1, spies.usage.calls)
			require.NotNil(t, spies.usage.last)
			require.Equal(t, 3, spies.usage.last.ImageCount)
			require.Equal(t, 1, spies.user.deductCalls)
			require.Zero(t, spies.subscription.incrementCalls)
		})
	}
}

func TestTokenHiveImagesProviderFailuresPreserveSourceBoundaryWithoutBilling(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantStatus int
		wantType   string
		wantCode   string
		wantMsg    string
	}{
		{name: "error event", mode: "images_error_event", wantStatus: http.StatusBadRequest, wantType: "image_generation_user_error", wantCode: "moderation_blocked", wantMsg: "Your request was blocked by the safety system"},
		{name: "response failed", mode: "images_response_failed", wantStatus: http.StatusBadRequest, wantType: "image_generation_user_error", wantCode: "moderation_blocked", wantMsg: "The safety system rejected this image"},
		{name: "retryable incomplete", mode: "images_incomplete_retryable", wantStatus: http.StatusBadGateway, wantType: "incomplete_error", wantCode: "response_incomplete", wantMsg: "Upstream image generation incomplete: max_output_tokens"},
		{name: "nonretryable incomplete", mode: "images_incomplete_policy", wantStatus: http.StatusBadRequest, wantType: "image_generation_user_error", wantCode: "response_incomplete", wantMsg: "Upstream image generation incomplete: content_filter"},
		{name: "completed refusal", mode: "images_completed_refusal", wantStatus: http.StatusBadRequest, wantType: "image_generation_user_error", wantCode: "content_policy_violation", wantMsg: "This request conflicts with our image safety policy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h, spies := newTokenHiveUsagePolicyHandler(t, true, false, test.mode)
			rec := runTokenHiveImagesUsagePolicyRequest(t, h)
			require.Equal(t, test.wantStatus, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, test.wantType, gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, test.wantCode, gjson.Get(rec.Body.String(), "error.code").String())
			require.Equal(t, test.wantMsg, gjson.Get(rec.Body.String(), "error.message").String())
			require.Equal(t, 1, spies.upstream.callCount())
			require.Zero(t, spies.usage.calls)
			require.Zero(t, spies.user.deductCalls)
			require.Zero(t, spies.subscription.incrementCalls)
		})
	}
}

func TestTokenHiveAuxiliaryHandlersPreserveTrustedExecutionErrorIdentity(t *testing.T) {
	for _, endpoint := range []string{"/v1/alpha/search", "/v1/messages/count_tokens"} {
		t.Run(endpoint, func(t *testing.T) {
			h, spies := newTokenHiveUsagePolicyHandler(t, true, false, "trusted_execution_error_response")
			rec := runTokenHiveExecutionErrorRequest(t, h, endpoint)

			require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, "bee", gjson.Get(rec.Body.String(), "error.source").String())
			require.Equal(t, "credential_unavailable", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "TokenHive execution failed", gjson.Get(rec.Body.String(), "error.message").String())
			require.NotContains(t, rec.Body.String(), "HANDLER_EXECUTION_SECRET")
			require.Equal(t, 1, spies.upstream.callCount())
			require.Zero(t, spies.usage.calls)
			require.Zero(t, spies.user.deductCalls)
			require.Zero(t, spies.subscription.incrementCalls)
		})
	}
}

func TestTokenHiveAuxiliaryHandlersApplyDedicatedSchedulerPolicy(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		mode     string
	}{
		{name: "images success with TTFT", endpoint: "/v1/images/generations", mode: "images_stream_success"},
		{name: "images failure", endpoint: "/v1/images/generations", mode: "images_transport_error"},
		{name: "alpha success", endpoint: "/v1/alpha/search", mode: "alpha_success"},
		{name: "alpha failure", endpoint: "/v1/alpha/search", mode: "alpha_transport_error"},
		{name: "alpha failover stimulus", endpoint: "/v1/alpha/search", mode: "alpha_500"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, dedicated := range []bool{true, false} {
				policyName := "ordinary"
				if dedicated {
					policyName = "dedicated"
				}
				t.Run(policyName, func(t *testing.T) {
					upstream := &tokenHiveHandlerUpstream{mode: test.mode}
					h, repo, _ := newTokenHiveResponsePolicyHandlerWithUpstream(t, dedicated, upstream, true)
					if dedicated {
						repo.accounts = repo.accounts[:1]
					}
					_ = runTokenHiveAuxiliaryPolicyRequest(t, h, test.endpoint)
					metrics := h.gatewayService.SnapshotOpenAIAccountSchedulerMetrics()
					if dedicated {
						require.Zero(t, metrics.RuntimeStatsAccountCount)
						require.Zero(t, metrics.AccountSwitchTotal)
						return
					}
					require.Greater(t, metrics.RuntimeStatsAccountCount, 0)
					if test.mode == "alpha_500" {
						require.Greater(t, metrics.AccountSwitchTotal, int64(0))
					}
				})
			}
		})
	}
}
