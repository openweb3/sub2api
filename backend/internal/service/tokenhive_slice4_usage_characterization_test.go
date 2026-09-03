package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	tokenHiveSlice4OutcomeNormal      = "normal"
	tokenHiveSlice4OutcomeProviderErr = "provider_error"
	tokenHiveSlice4OutcomeInterrupted = "interrupted"
	tokenHiveSlice4FixtureHMACByte    = byte(0x42)
	tokenHiveSlice4FixtureGroupID     = int64(91)
	tokenHiveSlice4FixtureUserID      = int64(12)
	tokenHiveSlice4FixtureAPIKeyID    = int64(11)
)

type tokenHiveSlice4UsageWriterSpy struct {
	service.UsageLogRepository

	mu              sync.Mutex
	bestEffortErr   error
	bestEffortCalls int
	createCalls     int
	ctxErrors       []error
	logs            []*service.UsageLog
}

func (s *tokenHiveSlice4UsageWriterSpy) CreateBestEffort(ctx context.Context, log *service.UsageLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bestEffortCalls++
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	s.logs = append(s.logs, log)
	return s.bestEffortErr
}

func (s *tokenHiveSlice4UsageWriterSpy) Create(ctx context.Context, log *service.UsageLog) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	s.logs = append(s.logs, log)
	return true, nil
}

func (s *tokenHiveSlice4UsageWriterSpy) counts() (bestEffort, create int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bestEffortCalls, s.createCalls
}

func (s *tokenHiveSlice4UsageWriterSpy) contextErrors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.ctxErrors...)
}

type tokenHiveSlice4AccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r *tokenHiveSlice4AccountRepo) ListSchedulableByGroupID(context.Context, int64) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) ListSchedulableByGroupIDAndPlatforms(context.Context, int64, []string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *tokenHiveSlice4AccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

type tokenHiveSlice4UserRepo struct {
	service.UserRepository
}

func (r *tokenHiveSlice4UserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	return &service.User{ID: id, Balance: 100, Status: service.StatusActive}, nil
}

func (r *tokenHiveSlice4UserRepo) DeductBalance(context.Context, int64, float64) error { return nil }

type tokenHiveSlice4SubscriptionRepo struct {
	service.UserSubscriptionRepository
}

func (r *tokenHiveSlice4SubscriptionRepo) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*service.UserSubscription, error) {
	return nil, nil
}

func (r *tokenHiveSlice4SubscriptionRepo) IncrementUsage(context.Context, int64, float64) error {
	return nil
}

type tokenHiveSlice4ReadThenError struct {
	reader io.Reader
	err    error
}

type tokenHiveSlice4CancelAtUpstreamKey struct{}

func (r *tokenHiveSlice4ReadThenError) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) && r.err != nil {
		err = r.err
		r.err = nil
	}
	return n, err
}

func (r *tokenHiveSlice4ReadThenError) Close() error { return nil }

type tokenHiveSlice4Upstream struct {
	service.HTTPUpstream

	mu         sync.Mutex
	outcome    string
	operations []string
}

func (u *tokenHiveSlice4Upstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	operation := strings.TrimSpace(req.Header.Get(service.TokenHiveHeaderSourceOperation))
	u.mu.Lock()
	u.operations = append(u.operations, operation)
	u.mu.Unlock()
	if cancel, ok := req.Context().Value(tokenHiveSlice4CancelAtUpstreamKey{}).(context.CancelFunc); ok {
		cancel()
	}

	if u.outcome == tokenHiveSlice4OutcomeProviderErr {
		return tokenHiveSlice4JSONResponse(http.StatusBadRequest, `{"error":{"type":"fixture_error","message":"provider rejected request"}}`), nil
	}
	if u.outcome == tokenHiveSlice4OutcomeInterrupted {
		return tokenHiveSlice4InterruptedResponse(operation)
	}

	switch operation {
	case service.SourceOperationAnthropicMessagesCreate:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"id":"msg_slice4","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`), nil
	case service.SourceOperationAnthropicMessagesStream:
		return tokenHiveSlice4SSEResponse(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_slice4","type":"message","role":"assistant","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":5}}}`,
			"",
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			"",
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			"",
		}, "\n")), nil
	case service.SourceOperationAnthropicMessagesCountTokens:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"input_tokens":17}`), nil
	case service.SourceOperationOpenAIResponsesHTTP:
		return tokenHiveSlice4SSEResponse("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_slice4\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\ndata: [DONE]\n\n"), nil
	case service.SourceOperationOpenAIResponsesCompact:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"id":"cmp_slice4","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"compaction","encrypted_content":"fixture"}],"usage":{"input_tokens":6,"output_tokens":2,"total_tokens":8}}`), nil
	case service.SourceOperationOpenAIResponsesInputTokens:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"input_tokens":23}`), nil
	case service.SourceOperationOpenAIImagesGenerations, service.SourceOperationOpenAIImagesEdits:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"created":1710000021,"data":[{"b64_json":"Zml4dHVyZQ==","size":"1024x1024"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`), nil
	case service.SourceOperationOpenAIAlphaSearch:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"output":"fixture alpha result"}`), nil
	case service.SourceOperationOpenAICodexModelsManifest:
		return tokenHiveSlice4JSONResponse(http.StatusOK, `{"models":[{"slug":"gpt-5.6-sol"}]}`), nil
	default:
		return nil, errors.New("fixture observed an unknown source operation")
	}
}

func (u *tokenHiveSlice4Upstream) observedOperation(t *testing.T) string {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.Len(t, u.operations, 1)
	return u.operations[0]
}

func tokenHiveSlice4JSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func tokenHiveSlice4SSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func tokenHiveSlice4InterruptedResponse(operation string) (*http.Response, error) {
	switch operation {
	case service.SourceOperationAnthropicMessagesStream:
		partial := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"cache_creation_input_tokens\":4}}}\n\n"
		resp := tokenHiveSlice4SSEResponse("")
		resp.Body = &tokenHiveSlice4ReadThenError{reader: strings.NewReader(partial), err: io.ErrUnexpectedEOF}
		return resp, nil
	case service.SourceOperationOpenAIResponsesHTTP:
		partial := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		resp := tokenHiveSlice4SSEResponse("")
		resp.Body = &tokenHiveSlice4ReadThenError{reader: strings.NewReader(partial), err: io.ErrUnexpectedEOF}
		return resp, nil
	case service.SourceOperationOpenAIResponsesCompact,
		service.SourceOperationOpenAIImagesGenerations,
		service.SourceOperationOpenAIImagesEdits:
		resp := tokenHiveSlice4JSONResponse(http.StatusOK, "")
		resp.Body = &tokenHiveSlice4ReadThenError{reader: strings.NewReader(`{"status":"in_progress"}`), err: io.ErrUnexpectedEOF}
		return resp, nil
	default:
		return nil, context.Canceled
	}
}

func tokenHiveSlice4Config(accountID int64, upstreamType string) *config.Config {
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxAccountSwitches = 1
	cfg.TokenHive = config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{tokenHiveSlice4FixtureHMACByte}, 32)),
		Accounts:      map[int64]string{accountID: upstreamType},
	}
	return cfg
}

type tokenHiveSlice4HandlerFixture struct {
	openAI    *handler.OpenAIGatewayHandler
	anthropic *handler.GatewayHandler
	writer    *tokenHiveSlice4UsageWriterSpy
	upstream  *tokenHiveSlice4Upstream
}

func tokenHiveSlice4OpenAIHandlerFixture(t *testing.T, outcome string, bestEffortErr error) tokenHiveSlice4HandlerFixture {
	t.Helper()
	account := service.Account{
		ID: 1, Name: "slice4-openai", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{tokenHiveSlice4FixtureGroupID},
		Credentials: map[string]any{"api_key": "external-key", "base_url": "https://api.openai.com"},
	}
	cfg := tokenHiveSlice4Config(account.ID, service.UpstreamTypeOpenAICodexOAuth)
	writer := &tokenHiveSlice4UsageWriterSpy{bestEffortErr: bestEffortErr}
	userRepo := &tokenHiveSlice4UserRepo{}
	subRepo := &tokenHiveSlice4SubscriptionRepo{}
	upstream := &tokenHiveSlice4Upstream{outcome: outcome}
	billingCache := service.NewBillingCacheService(nil, userRepo, subRepo, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewOpenAIGatewayService(
		&tokenHiveSlice4AccountRepo{accounts: []service.Account{account}}, writer, nil, userRepo, subRepo, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return tokenHiveSlice4HandlerFixture{
		openAI: handler.NewOpenAIGatewayHandler(
			gateway, service.NewConcurrencyService(nil), billingCache,
			service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg,
		),
		writer:   writer,
		upstream: upstream,
	}
}

func tokenHiveSlice4AnthropicHandlerFixture(t *testing.T, outcome string, bestEffortErr error) tokenHiveSlice4HandlerFixture {
	t.Helper()
	account := service.Account{
		ID: 42, Name: "slice4-anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{tokenHiveSlice4FixtureGroupID}, Concurrency: 1,
		Credentials: map[string]any{"api_key": "virtual-account-key"},
	}
	cfg := tokenHiveSlice4Config(account.ID, service.UpstreamTypeAnthropicOAuth)
	writer := &tokenHiveSlice4UsageWriterSpy{bestEffortErr: bestEffortErr}
	userRepo := &tokenHiveSlice4UserRepo{}
	subRepo := &tokenHiveSlice4SubscriptionRepo{}
	upstream := &tokenHiveSlice4Upstream{outcome: outcome}
	billingCache := service.NewBillingCacheService(nil, userRepo, subRepo, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	gateway := service.NewGatewayService(
		&tokenHiveSlice4AccountRepo{accounts: []service.Account{account}}, nil,
		writer, nil, userRepo, subRepo, nil, nil, cfg,
		nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return tokenHiveSlice4HandlerFixture{
		anthropic: handler.NewGatewayHandler(
			gateway, nil, nil, nil, nil, service.NewConcurrencyService(nil), billingCache,
			nil, nil, nil, nil, nil, nil, cfg, nil,
		),
		writer:   writer,
		upstream: upstream,
	}
}

func tokenHiveSlice4APIKey(platform string, allowImages bool) (*service.APIKey, *service.Group, *service.User) {
	group := &service.Group{
		ID: tokenHiveSlice4FixtureGroupID, Platform: platform, Status: service.StatusActive, Hydrated: true,
		RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
		AllowImageGeneration: allowImages, AllowMessagesDispatch: true,
	}
	user := &service.User{ID: tokenHiveSlice4FixtureUserID, Balance: 100, Status: service.StatusActive}
	apiKey := &service.APIKey{
		ID: tokenHiveSlice4FixtureAPIKeyID, UserID: user.ID, GroupID: &group.ID,
		Group: group, User: user, Status: service.StatusActive,
	}
	return apiKey, group, user
}

func tokenHiveSlice4GinContext(method, path, contentType string, body io.Reader, apiKey *service.APIKey, group *service.Group, requestContexts ...context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(method, path, body)
	if len(requestContexts) > 0 {
		req = req.WithContext(requestContexts[0])
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if group != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	}
	c.Request = req
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})
	return c, recorder
}

func tokenHiveSlice4RunOperation(t *testing.T, operation, outcome string, bestEffortErr error, requestContexts ...context.Context) tokenHiveSlice4HandlerFixture {
	t.Helper()
	switch operation {
	case service.SourceOperationAnthropicMessagesCreate,
		service.SourceOperationAnthropicMessagesStream,
		service.SourceOperationAnthropicMessagesCountTokens:
		fixture := tokenHiveSlice4AnthropicHandlerFixture(t, outcome, bestEffortErr)
		apiKey, group, _ := tokenHiveSlice4APIKey(service.PlatformAnthropic, false)
		stream := operation == service.SourceOperationAnthropicMessagesStream
		path := "/v1/messages"
		body := `{"model":"claude-sonnet-4","stream":` + tokenHiveSlice4BoolString(stream) + `,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
		if operation == service.SourceOperationAnthropicMessagesCountTokens {
			path = "/v1/messages/count_tokens"
		}
		c, _ := tokenHiveSlice4GinContext(http.MethodPost, path, "application/json", strings.NewReader(body), apiKey, group, requestContexts...)
		if operation == service.SourceOperationAnthropicMessagesCountTokens {
			fixture.anthropic.CountTokens(c)
		} else {
			fixture.anthropic.Messages(c)
		}
		return fixture
	default:
		fixture := tokenHiveSlice4OpenAIHandlerFixture(t, outcome, bestEffortErr)
		apiKey, group, _ := tokenHiveSlice4APIKey(service.PlatformOpenAI, true)
		method := http.MethodPost
		path := "/v1/responses"
		contentType := "application/json"
		var body io.Reader = strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":"hello"}`)
		invoke := fixture.openAI.Responses

		switch operation {
		case service.SourceOperationOpenAIResponsesCompact:
			path = "/v1/responses/compact"
			body = strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`)
		case service.SourceOperationOpenAIResponsesInputTokens:
			path = "/v1/messages/count_tokens"
			body = strings.NewReader(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`)
			invoke = fixture.openAI.CountTokens
		case service.SourceOperationOpenAIImagesGenerations:
			path = "/v1/images/generations"
			body = strings.NewReader(`{"model":"gpt-image-2","prompt":"draw","n":1,"response_format":"b64_json"}`)
			invoke = fixture.openAI.Images
		case service.SourceOperationOpenAIImagesEdits:
			path = "/v1/images/edits"
			var multipartBody bytes.Buffer
			writer := multipart.NewWriter(&multipartBody)
			require.NoError(t, writer.WriteField("model", "gpt-image-2"))
			require.NoError(t, writer.WriteField("prompt", "edit"))
			require.NoError(t, writer.WriteField("n", "1"))
			part, err := writer.CreateFormFile("image", "fixture.png")
			require.NoError(t, err)
			_, err = part.Write([]byte("fixture image bytes"))
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			contentType = writer.FormDataContentType()
			body = bytes.NewReader(multipartBody.Bytes())
			invoke = fixture.openAI.Images
		case service.SourceOperationOpenAIAlphaSearch:
			path = "/v1/alpha/search"
			body = strings.NewReader(`{"id":"search-fixture","model":"gpt-5.6-sol","commands":{"search_query":[{"q":"news"}]}}`)
			invoke = fixture.openAI.AlphaSearch
		case service.SourceOperationOpenAICodexModelsManifest:
			method = http.MethodGet
			path = "/v1/models?client_version=0.145.0"
			contentType = ""
			body = nil
			invoke = fixture.openAI.CodexModels
		}

		c, _ := tokenHiveSlice4GinContext(method, path, contentType, body, apiKey, group, requestContexts...)
		invoke(c)
		return fixture
	}
}

func tokenHiveSlice4BoolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestTokenHiveSlice4OperationUsageCharacterization(t *testing.T) {
	tests := []struct {
		operation             string
		normalBestEffort      int
		providerBestEffort    int
		interruptedBestEffort int
	}{
		{service.SourceOperationAnthropicMessagesCreate, 1, 0, 0},
		{service.SourceOperationAnthropicMessagesStream, 1, 0, 1},
		{service.SourceOperationAnthropicMessagesCountTokens, 0, 0, 0},
		{service.SourceOperationOpenAIResponsesHTTP, 1, 0, 0},
		{service.SourceOperationOpenAIResponsesCompact, 1, 0, 0},
		{service.SourceOperationOpenAIResponsesInputTokens, 0, 0, 0},
		{service.SourceOperationOpenAIImagesGenerations, 1, 0, 0},
		{service.SourceOperationOpenAIImagesEdits, 1, 0, 0},
		{service.SourceOperationOpenAIAlphaSearch, 1, 0, 0},
		{service.SourceOperationOpenAICodexModelsManifest, 0, 0, 0},
	}
	outcomes := []struct {
		name string
		want func(test struct {
			operation             string
			normalBestEffort      int
			providerBestEffort    int
			interruptedBestEffort int
		}) int
	}{
		{tokenHiveSlice4OutcomeNormal, func(test struct {
			operation                                                   string
			normalBestEffort, providerBestEffort, interruptedBestEffort int
		}) int {
			return test.normalBestEffort
		}},
		{tokenHiveSlice4OutcomeProviderErr, func(test struct {
			operation                                                   string
			normalBestEffort, providerBestEffort, interruptedBestEffort int
		}) int {
			return test.providerBestEffort
		}},
		{tokenHiveSlice4OutcomeInterrupted, func(test struct {
			operation                                                   string
			normalBestEffort, providerBestEffort, interruptedBestEffort int
		}) int {
			return test.interruptedBestEffort
		}},
	}

	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			for _, outcome := range outcomes {
				t.Run(outcome.name, func(t *testing.T) {
					fixture := tokenHiveSlice4RunOperation(t, test.operation, outcome.name, nil)
					require.Equal(t, test.operation, fixture.upstream.observedOperation(t))
					bestEffort, create := fixture.writer.counts()
					require.Equal(t, outcome.want(test), bestEffort)
					require.Zero(t, create)
				})
			}
		})
	}
}

func TestTokenHiveSlice4UsageWriterFallsBackToCreateOnce(t *testing.T) {
	fixture := tokenHiveSlice4RunOperation(
		t,
		service.SourceOperationOpenAIResponsesHTTP,
		tokenHiveSlice4OutcomeNormal,
		errors.New("best-effort queue unavailable"),
	)
	require.Equal(t, service.SourceOperationOpenAIResponsesHTTP, fixture.upstream.observedOperation(t))
	bestEffort, create := fixture.writer.counts()
	require.Equal(t, 1, bestEffort)
	require.Equal(t, 1, create)
	require.Equal(t, []error{nil, nil}, fixture.writer.contextErrors())
}

func TestTokenHiveSlice4UsageWriterDetachesCanceledRequestContext(t *testing.T) {
	requestContext, cancel := context.WithCancel(context.Background())
	requestContext = context.WithValue(requestContext, tokenHiveSlice4CancelAtUpstreamKey{}, context.CancelFunc(cancel))
	fixture := tokenHiveSlice4RunOperation(
		t,
		service.SourceOperationOpenAIResponsesHTTP,
		tokenHiveSlice4OutcomeNormal,
		nil,
		requestContext,
	)
	bestEffort, create := fixture.writer.counts()
	require.Equal(t, 1, bestEffort)
	require.Zero(t, create)
	require.Equal(t, []error{nil}, fixture.writer.contextErrors())
}

func TestTokenHiveSlice4AsyncUsageCanExposeHigherSequenceFirst(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:      2,
		QueueSize:        4,
		TaskTimeout:      time.Second,
		OverflowPolicy:   config.UsageRecordOverflowPolicyDrop,
		AutoScaleEnabled: false,
	})
	t.Cleanup(pool.Stop)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondVisible := make(chan struct{})
	allVisible := make(chan struct{})
	var mu sync.Mutex
	visible := make([]int64, 0, 2)

	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(firstStarted)
		<-releaseFirst
		mu.Lock()
		visible = append(visible, 1)
		if len(visible) == 2 {
			close(allVisible)
		}
		mu.Unlock()
	}))
	<-firstStarted
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		mu.Lock()
		visible = append(visible, 2)
		mu.Unlock()
		close(secondVisible)
	}))

	select {
	case <-secondVisible:
	case <-time.After(time.Second):
		t.Fatal("higher sequence task did not become visible")
	}
	mu.Lock()
	require.Equal(t, []int64{2}, visible)
	mu.Unlock()

	close(releaseFirst)
	select {
	case <-allVisible:
	case <-time.After(time.Second):
		t.Fatal("lower sequence task did not become visible")
	}
	mu.Lock()
	require.Equal(t, []int64{2, 1}, visible)
	mu.Unlock()
}
