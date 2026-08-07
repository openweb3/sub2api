//go:build integration

package service_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	redisclient "github.com/redis/go-redis/v9"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

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
	ordinary := &service.Account{
		Name:        "ordinary-openai-" + uuid.NewString(),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "ordinary-fixture-key"},
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	mapped := &service.Account{
		Name:        "mapped-openai-" + uuid.NewString(),
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "mapped-fixture-key"},
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	if err := accountRepository.Create(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	if err := accountRepository.Create(ctx, mapped); err != nil {
		t.Fatal(err)
	}
	accounts, err := accountRepository.GetByIDs(ctx, []int64{ordinary.ID, mapped.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("loaded accounts=%d", len(accounts))
	}
	cfg := config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString([]byte("slice-1-test-tenant-hmac-key-32bytes")),
		Accounts:      map[int64]string{mapped.ID: service.UpstreamTypeOpenAICodexOAuth},
	}
	registry, err := service.NewTokenHiveRegistry(cfg, []service.Account{*accounts[0], *accounts[1]})
	if err != nil {
		t.Fatal(err)
	}
	ordinaryAccount, mappedAccount := accounts[0], accounts[1]
	if accounts[0].ID != ordinary.ID {
		ordinaryAccount, mappedAccount = accounts[1], accounts[0]
	}
	mappedPolicy := service.ResolveTokenHiveResponsePolicy(mappedAccount, registry)
	if !mappedPolicy.Dedicated || mappedPolicy.AllowSameAccountRetry || mappedPolicy.AllowAccountFailover || mappedPolicy.AllowAccountMutation || mappedPolicy.AllowRuntimeBlock || mappedPolicy.AllowSchedulerFeedback {
		t.Fatalf("mapped control policy inactive: %+v", mappedPolicy)
	}
	ordinaryPolicy := service.ResolveTokenHiveResponsePolicy(ordinaryAccount, registry)
	if ordinaryPolicy.Dedicated || !ordinaryPolicy.AllowSameAccountRetry || !ordinaryPolicy.AllowAccountFailover || !ordinaryPolicy.AllowAccountMutation || !ordinaryPolicy.AllowRuntimeBlock || !ordinaryPolicy.AllowSchedulerFeedback {
		t.Fatalf("ordinary policy changed: %+v", ordinaryPolicy)
	}

	ordinaryLoaded, err := accountRepository.GetByID(ctx, ordinary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := schedulerCache.SetAccount(ctx, ordinaryLoaded); err != nil {
		t.Fatal(err)
	}
	ordinaryCached, err := schedulerCache.GetAccount(ctx, ordinary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryCached == nil || ordinaryCached.ID != ordinary.ID {
		t.Fatalf("ordinary scheduler snapshot=%+v", ordinaryCached)
	}
	if cachedPolicy := service.ResolveTokenHiveResponsePolicy(ordinaryCached, registry); cachedPolicy != ordinaryPolicy {
		t.Fatalf("cached ordinary policy=%+v database policy=%+v", cachedPolicy, ordinaryPolicy)
	}

	resetAt := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
	if err := accountRepository.SetRateLimited(ctx, ordinary.ID, resetAt); err != nil {
		t.Fatal(err)
	}
	mutated, err := accountRepository.GetByID(ctx, ordinary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mutated.RateLimitedAt == nil || mutated.RateLimitResetAt == nil || !mutated.RateLimitResetAt.Equal(resetAt) {
		t.Fatalf("ordinary mutation was suppressed: %+v", mutated)
	}
	if policy := service.ResolveTokenHiveResponsePolicy(mutated, registry); policy != ordinaryPolicy {
		t.Fatalf("ordinary policy changed after repository mutation: %+v", policy)
	}
}
