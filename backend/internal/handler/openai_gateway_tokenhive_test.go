package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenHiveHandlerAccountRepo struct {
	service.AccountRepository
	mu         sync.Mutex
	accounts   []service.Account
	selections int
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

func (u *tokenHiveHandlerUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.account = append(u.account, accountID)
	u.mu.Unlock()
	switch u.mode {
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

func tokenHiveHandlerErrorResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(`{"error":{"message":"upstream failed"}}`))}
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

func newTokenHiveResponsePolicyHandlerWithUpstream(t *testing.T, dedicated bool, upstream *tokenHiveHandlerUpstream) (*OpenAIGatewayHandler, *tokenHiveHandlerAccountRepo, *tokenHiveHandlerUpstream) {
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
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
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
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 11, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}, User: &service.User{ID: 12}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 12})
	h.Responses(c)
	return rec
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
