//go:build integration

package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	redisclient "github.com/redis/go-redis/v9"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

type tokenHiveIntegrationUpstream struct {
	mu            sync.Mutex
	mode          string
	failingID     int64
	fallbackID    int64
	accountRepo   service.AccountRepository
	calls         []int64
	perAccount    map[int64]int
	fallbackArmed bool
}

func (u *tokenHiveIntegrationUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.do(accountID)
}

func (u *tokenHiveIntegrationUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.do(accountID)
}

func (u *tokenHiveIntegrationUpstream) do(accountID int64) (*http.Response, error) {
	u.mu.Lock()
	if u.perAccount == nil {
		u.perAccount = make(map[int64]int)
	}
	u.calls = append(u.calls, accountID)
	u.perAccount[accountID]++
	armFallback := accountID == u.failingID && u.fallbackID > 0 && !u.fallbackArmed
	if armFallback {
		u.fallbackArmed = true
	}
	mode := u.mode
	u.mu.Unlock()
	if armFallback {
		if err := u.accountRepo.SetSchedulable(context.Background(), u.fallbackID, true); err != nil {
			return nil, fmt.Errorf("arm fallback account: %w", err)
		}
	}
	if accountID != u.failingID || mode == "success" {
		return tokenHiveIntegrationSuccessResponse(), nil
	}
	switch mode {
	case "500":
		return tokenHiveIntegrationErrorResponse(http.StatusInternalServerError), nil
	case "429":
		response := tokenHiveIntegrationErrorResponse(http.StatusTooManyRequests)
		response.Header.Set("Retry-After", "60")
		return response, nil
	case "transport":
		return nil, errors.New("dial tcp 127.0.0.1:9: connect: connection refused")
	default:
		return nil, fmt.Errorf("unknown integration upstream mode %q", mode)
	}
}

func (u *tokenHiveIntegrationUpstream) trace() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.calls...)
}

func (u *tokenHiveIntegrationUpstream) reset(mode string, failingID, fallbackID int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.mode = mode
	u.failingID = failingID
	u.fallbackID = fallbackID
	u.calls = nil
	u.perAccount = make(map[int64]int)
	u.fallbackArmed = false
}

func tokenHiveIntegrationErrorResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"integration upstream failure"}}`)),
	}
}

func tokenHiveIntegrationSuccessResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(
			`{"id":"resp-integration","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		)),
	}
}

var (
	tokenHiveIntegrationDB    *sql.DB
	tokenHiveIntegrationEnt   *dbent.Client
	tokenHiveIntegrationRedis *redisclient.Client
)

func TestMain(m *testing.M) {
	os.Exit(runTokenHiveIntegrationTests(m))
}

func runTokenHiveIntegrationTests(m *testing.M) int {
	ctx := context.Background()
	if err := timezone.Init("UTC"); err != nil {
		fmt.Fprintln(os.Stderr, "init timezone:", err)
		return 1
	}
	postgresImage := os.Getenv("SUB2API_TEST_POSTGRES_IMAGE")
	if postgresImage == "" {
		postgresImage = "postgres:18.1-alpine3.23"
	}
	postgresContainer, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("sub2api_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		return 1
	}
	defer func() { _ = postgresContainer.Terminate(ctx) }()
	redisContainer, err := tcredis.Run(ctx, "redis:8.4-alpine")
	if err != nil {
		fmt.Fprintln(os.Stderr, "start redis:", err)
		return 1
	}
	defer func() { _ = redisContainer.Terminate(ctx) }()
	dsn, err := postgresContainer.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres DSN:", err)
		return 1
	}
	tokenHiveIntegrationDB, err = sql.Open("postgres", dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open postgres:", err)
		return 1
	}
	defer tokenHiveIntegrationDB.Close()
	if err := tokenHiveIntegrationDB.PingContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ping postgres:", err)
		return 1
	}
	if err := repository.ApplyMigrations(ctx, tokenHiveIntegrationDB); err != nil {
		fmt.Fprintln(os.Stderr, "apply migrations:", err)
		return 1
	}
	tokenHiveIntegrationEnt = dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, tokenHiveIntegrationDB)))
	defer tokenHiveIntegrationEnt.Close()
	redisHost, err := redisContainer.Host(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis host:", err)
		return 1
	}
	redisPort, err := redisContainer.MappedPort(ctx, "6379/tcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "redis port:", err)
		return 1
	}
	tokenHiveIntegrationRedis = redisclient.NewClient(&redisclient.Options{Addr: fmt.Sprintf("%s:%d", redisHost, redisPort.Int())})
	defer tokenHiveIntegrationRedis.Close()
	if err := tokenHiveIntegrationRedis.Ping(ctx).Err(); err != nil {
		fmt.Fprintln(os.Stderr, "ping redis:", err)
		return 1
	}
	return m.Run()
}

func TestIntegrationTokenHiveOrdinaryAccountRegression(t *testing.T) {
	ctx := context.Background()
	schedulerCache := repository.NewSchedulerCache(tokenHiveIntegrationRedis)
	accountRepository := repository.NewAccountRepository(tokenHiveIntegrationEnt, tokenHiveIntegrationDB, schedulerCache)
	settingRepository := repository.NewSettingRepository(tokenHiveIntegrationEnt)
	if err := settingRepository.Set(ctx, "openai_advanced_scheduler_enabled", "true"); err != nil {
		t.Fatal(err)
	}

	ordinaryRetry := createTokenHiveIntegrationAccount(t, accountRepository, "ordinary-retry", 0, map[string]any{
		"api_key":                      "ordinary-retry-key",
		"pool_mode":                    true,
		"pool_mode_retry_count":        float64(1),
		"pool_mode_retry_status_codes": []any{float64(http.StatusInternalServerError)},
	})
	ordinary500 := createTokenHiveIntegrationAccount(t, accountRepository, "ordinary-500", 1, map[string]any{"api_key": "ordinary-500-key"})
	ordinary429 := createTokenHiveIntegrationAccount(t, accountRepository, "ordinary-429", 2, map[string]any{"api_key": "ordinary-429-key"})
	ordinaryTransport := createTokenHiveIntegrationAccount(t, accountRepository, "ordinary-transport", 3, map[string]any{"api_key": "ordinary-transport-key"})
	mapped := createTokenHiveIntegrationAccount(t, accountRepository, "mapped", 100, map[string]any{"api_key": "mapped-key"})
	allAccounts := []*service.Account{ordinaryRetry, ordinary500, ordinary429, ordinaryTransport, mapped}
	for _, account := range allAccounts {
		if err := schedulerCache.SetAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{mapped.ID: service.UpstreamTypeOpenAICodexOAuth},
	}

	t.Run("ordinary 500 preserves same-account retry and failover", func(t *testing.T) {
		setOnlyTokenHiveIntegrationAccounts(t, accountRepository, schedulerCache, allAccounts, ordinaryRetry.ID)
		loaded, err := accountRepository.GetByID(ctx, ordinaryRetry.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !loaded.IsPoolMode() || loaded.GetPoolModeRetryCount() != 1 || !loaded.IsPoolModeRetryableStatus(http.StatusInternalServerError) {
			t.Fatalf("retry account lost production pool settings: credentials=%#v", loaded.Credentials)
		}
		upstream := &tokenHiveIntegrationUpstream{accountRepo: accountRepository}
		upstream.reset("500", ordinaryRetry.ID, mapped.ID)
		gatewayHandler, gateway := newTokenHiveIntegrationHandler(t, accountRepository, cfg, upstream, settingRepository)
		policy := gateway.ResolveTokenHiveResponsePolicy(loaded)
		if policy.Dedicated || !policy.AllowSameAccountRetry {
			t.Fatalf("ordinary DB-loaded account received dedicated policy: %+v", policy)
		}
		probeContext, _ := tokenHiveIntegrationGinContext(t)
		_, probeErr := gateway.ForwardWithResponsePolicy(
			probeContext.Request.Context(), probeContext, loaded,
			[]byte(`{"model":"gpt-5.4","stream":false,"input":"integration"}`), policy,
		)
		var probeFailover *service.UpstreamFailoverError
		if !errors.As(probeErr, &probeFailover) || !probeFailover.RetryableOnSameAccount {
			t.Fatalf("production Forward lost retryable 500: err=%v failover=%+v", probeErr, probeFailover)
		}
		if err := accountRepository.SetSchedulable(ctx, mapped.ID, false); err != nil {
			t.Fatal(err)
		}
		upstream.reset("500", ordinaryRetry.ID, mapped.ID)
		recorder := runTokenHiveIntegrationRequest(t, gatewayHandler)
		want := []int64{ordinaryRetry.ID, ordinaryRetry.ID, mapped.ID}
		if got := upstream.trace(); !reflect.DeepEqual(got, want) {
			t.Fatalf("ordinary retry/failover trace=%v want=%v status=%d body=%s", got, want, recorder.Code, recorder.Body.String())
		}
		metrics := gateway.SnapshotOpenAIAccountSchedulerMetrics()
		if metrics.AccountSwitchTotal < 1 || metrics.RuntimeStatsAccountCount < 1 {
			t.Fatalf("ordinary scheduler feedback missing: %+v", metrics)
		}
	})

	t.Run("ordinary 500 preserves model runtime block", func(t *testing.T) {
		setOnlyTokenHiveIntegrationAccounts(t, accountRepository, schedulerCache, allAccounts, ordinary500.ID)
		upstream := &tokenHiveIntegrationUpstream{accountRepo: accountRepository}
		upstream.reset("500", ordinary500.ID, mapped.ID)
		gatewayHandler, _ := newTokenHiveIntegrationHandler(t, accountRepository, cfg, upstream, settingRepository)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{ordinary500.ID, mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ordinary 500 trace=%v want=%v", got, want)
		}
		if err := accountRepository.SetSchedulable(ctx, ordinary500.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := accountRepository.SetSchedulable(ctx, mapped.ID, true); err != nil {
			t.Fatal(err)
		}
		upstream.reset("success", ordinary500.ID, 0)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("model runtime block did not exclude ordinary account: trace=%v want=%v", got, want)
		}
	})

	t.Run("ordinary 429 persists DB cache and runtime feedback", func(t *testing.T) {
		setOnlyTokenHiveIntegrationAccounts(t, accountRepository, schedulerCache, allAccounts, ordinary429.ID)
		upstream := &tokenHiveIntegrationUpstream{accountRepo: accountRepository}
		upstream.reset("429", ordinary429.ID, mapped.ID)
		gatewayHandler, _ := newTokenHiveIntegrationHandler(t, accountRepository, cfg, upstream, settingRepository)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{ordinary429.ID, mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ordinary 429 trace=%v want=%v", got, want)
		}
		assertTokenHiveIntegrationRateLimited(t, accountRepository, schedulerCache, ordinary429.ID)
		if err := accountRepository.ClearRateLimit(ctx, ordinary429.ID); err != nil {
			t.Fatal(err)
		}
		if err := accountRepository.SetSchedulable(ctx, ordinary429.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := accountRepository.SetSchedulable(ctx, mapped.ID, true); err != nil {
			t.Fatal(err)
		}
		upstream.reset("success", ordinary429.ID, 0)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("429 runtime block did not survive DB/cache clear: trace=%v want=%v", got, want)
		}
	})

	t.Run("ordinary transport persists mutation and runtime block", func(t *testing.T) {
		setOnlyTokenHiveIntegrationAccounts(t, accountRepository, schedulerCache, allAccounts, ordinaryTransport.ID)
		upstream := &tokenHiveIntegrationUpstream{accountRepo: accountRepository}
		upstream.reset("transport", ordinaryTransport.ID, mapped.ID)
		gatewayHandler, _ := newTokenHiveIntegrationHandler(t, accountRepository, cfg, upstream, settingRepository)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{ordinaryTransport.ID, mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ordinary transport trace=%v want=%v", got, want)
		}
		assertTokenHiveIntegrationTempUnschedulable(t, accountRepository, schedulerCache, ordinaryTransport.ID)
		if err := accountRepository.ClearTempUnschedulable(ctx, ordinaryTransport.ID); err != nil {
			t.Fatal(err)
		}
		if err := accountRepository.SetSchedulable(ctx, ordinaryTransport.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := accountRepository.SetSchedulable(ctx, mapped.ID, true); err != nil {
			t.Fatal(err)
		}
		upstream.reset("success", ordinaryTransport.ID, 0)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("transport runtime block did not survive DB/cache clear: trace=%v want=%v", got, want)
		}
	})

	t.Run("mapped control suppresses 500 429 and transport behavior", func(t *testing.T) {
		setOnlyTokenHiveIntegrationAccounts(t, accountRepository, schedulerCache, allAccounts, mapped.ID)
		upstream := &tokenHiveIntegrationUpstream{accountRepo: accountRepository}
		gatewayHandler, gateway := newTokenHiveIntegrationHandler(t, accountRepository, cfg, upstream, settingRepository)
		for _, mode := range []string{"500", "429", "transport"} {
			upstream.reset(mode, mapped.ID, 0)
			_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
			if got, want := upstream.trace(), []int64{mapped.ID}; !reflect.DeepEqual(got, want) {
				t.Fatalf("mapped %s trace=%v want=%v", mode, got, want)
			}
			assertTokenHiveIntegrationUnmutated(t, accountRepository, schedulerCache, mapped.ID)
		}
		upstream.reset("success", mapped.ID, 0)
		_ = runTokenHiveIntegrationRequest(t, gatewayHandler)
		if got, want := upstream.trace(), []int64{mapped.ID}; !reflect.DeepEqual(got, want) {
			t.Fatalf("mapped transport incorrectly installed a runtime block: trace=%v want=%v", got, want)
		}
		metrics := gateway.SnapshotOpenAIAccountSchedulerMetrics()
		if metrics.AccountSwitchTotal != 0 || metrics.RuntimeStatsAccountCount != 0 {
			t.Fatalf("mapped scheduler feedback leaked: %+v", metrics)
		}
	})
}

func createTokenHiveIntegrationAccount(t *testing.T, accountRepository service.AccountRepository, prefix string, priority int, credentials map[string]any) *service.Account {
	t.Helper()
	account := &service.Account{
		Name:        prefix + "-" + uuid.NewString(),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: credentials,
		Status:      service.StatusActive,
		Schedulable: false,
		Concurrency: 1,
		Priority:    priority,
	}
	if err := accountRepository.Create(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account
}

func setOnlyTokenHiveIntegrationAccounts(t *testing.T, accountRepository service.AccountRepository, schedulerCache service.SchedulerCache, accounts []*service.Account, activeID int64) {
	t.Helper()
	for _, account := range accounts {
		if err := accountRepository.SetSchedulable(context.Background(), account.ID, account.ID == activeID); err != nil {
			t.Fatal(err)
		}
		loaded, err := accountRepository.GetByID(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := schedulerCache.SetAccount(context.Background(), loaded); err != nil {
			t.Fatal(err)
		}
	}
}

func newTokenHiveIntegrationHandler(
	t *testing.T,
	accountRepository service.AccountRepository,
	tokenHiveConfig config.TokenHiveConfig,
	upstream service.HTTPUpstream,
	settingRepository service.SettingRepository,
) (*handler.OpenAIGatewayHandler, *service.OpenAIGatewayService) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple, TokenHive: tokenHiveConfig}
	cfg.Gateway.MaxAccountSwitches = 3
	registry, err := service.ProvideTokenHiveRegistry(cfg, accountRepository)
	if err != nil {
		t.Fatal(err)
	}
	settings := service.NewSettingService(settingRepository, cfg)
	rateLimits := service.NewRateLimitService(accountRepository, nil, cfg, nil, nil)
	concurrency := service.NewConcurrencyService(repository.NewConcurrencyCache(tokenHiveIntegrationRedis, 1, 60))
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.ProvideOpenAIGatewayService(
		registry,
		accountRepository, nil, nil, nil, nil, nil, repository.NewGatewayCache(tokenHiveIntegrationRedis), cfg,
		nil, concurrency, nil, rateLimits, billingCache, upstream,
		nil, nil, nil, nil, nil, nil, settings, nil,
	)
	rateLimits.SetSettingService(settings)
	apiKeys := service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg)
	return handler.NewOpenAIGatewayHandler(gateway, concurrency, billingCache, apiKeys, nil, nil, nil, nil, cfg), gateway
}

func runTokenHiveIntegrationRequest(t *testing.T, gatewayHandler *handler.OpenAIGatewayHandler) *httptest.ResponseRecorder {
	t.Helper()
	ginContext, responseRecorder := tokenHiveIntegrationGinContext(t)
	gatewayHandler.Responses(ginContext)
	return responseRecorder
}

func tokenHiveIntegrationGinContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(9001)
	userID := int64(9002)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","stream":false,"input":"integration"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), ctxkey.RequestID, uuid.NewString()))
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = request
	ginContext.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      9003,
		UserID:  userID,
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
		User:    &service.User{ID: userID, Balance: 100, Status: service.StatusActive},
	})
	ginContext.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
	return ginContext, recorder
}

func assertTokenHiveIntegrationRateLimited(t *testing.T, accountRepository service.AccountRepository, schedulerCache service.SchedulerCache, accountID int64) {
	t.Helper()
	loaded, err := accountRepository.GetByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := schedulerCache.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RateLimitedAt == nil || loaded.RateLimitResetAt == nil {
		t.Fatalf("ordinary DB rate-limit mutation missing: %+v", loaded)
	}
	if cached == nil || cached.RateLimitedAt == nil || cached.RateLimitResetAt == nil {
		t.Fatalf("ordinary Redis rate-limit feedback missing: %+v", cached)
	}
}

func assertTokenHiveIntegrationTempUnschedulable(t *testing.T, accountRepository service.AccountRepository, schedulerCache service.SchedulerCache, accountID int64) {
	t.Helper()
	loaded, err := accountRepository.GetByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := schedulerCache.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TempUnschedulableUntil == nil || loaded.TempUnschedulableReason == "" {
		t.Fatalf("ordinary DB transport mutation missing: %+v", loaded)
	}
	if cached == nil || cached.TempUnschedulableUntil == nil || cached.TempUnschedulableReason == "" {
		t.Fatalf("ordinary Redis transport feedback missing: %+v", cached)
	}
}

func assertTokenHiveIntegrationUnmutated(t *testing.T, accountRepository service.AccountRepository, schedulerCache service.SchedulerCache, accountID int64) {
	t.Helper()
	loaded, err := accountRepository.GetByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := schedulerCache.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	for location, account := range map[string]*service.Account{"DB": loaded, "Redis": cached} {
		if account == nil || account.Status != service.StatusActive || !account.Schedulable || account.ErrorMessage != "" ||
			account.RateLimitedAt != nil || account.RateLimitResetAt != nil || account.TempUnschedulableUntil != nil || account.TempUnschedulableReason != "" {
			t.Fatalf("mapped control mutated in %s: %+v", location, account)
		}
	}
}
