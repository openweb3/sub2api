package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const tokenHiveSlice4NoUsage = "none"
const tokenHiveSlice4BestEffort = "CreateBestEffort"

type tokenHiveSlice4UsageExpectation struct {
	Operation         string
	Normal            string
	ProviderError     string
	Interrupted       string
	SourceSymbol      string
	SourceTest        string
	NoUsageSourceTest string
}

func tokenHiveSlice4UsageExpectations() []tokenHiveSlice4UsageExpectation {
	return []tokenHiveSlice4UsageExpectation{
		{SourceOperationAnthropicMessagesCreate, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "GatewayHandler.handleMessages", "TestGatewayHandlerSubmitUsageRecordTask_WithPool", ""},
		{SourceOperationAnthropicMessagesStream, tokenHiveSlice4BestEffort, "CreateBestEffort when ForwardResult carries partial usage", "CreateBestEffort when ForwardResult carries partial usage", "GatewayHandler.handleMessages", "TestGatewayHandlerSubmitUsageRecordTask_WithPool", ""},
		{SourceOperationAnthropicMessagesCountTokens, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "GatewayHandler.CountTokens", "", "TestTokenHiveAnthropicCountTokensDoesNotRecordUsage"},
		{SourceOperationOpenAIResponsesHTTP, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, "CreateBestEffort only after a terminal result was parsed", "OpenAIGatewayHandler.handleOpenAIRequest", "TestTokenHiveResponsePolicyPreservesRealHandlerUsageBilling", ""},
		{SourceOperationOpenAIResponsesCompact, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "OpenAIGatewayHandler.handleOpenAIRequest", "TestTokenHiveResponsePolicyPreservesRealHandlerUsageBilling", ""},
		{SourceOperationOpenAIResponsesInputTokens, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "OpenAIGatewayHandler.CountTokens", "", "TestTokenHiveResponsesInputTokensDoesNotRecordUsage"},
		{SourceOperationOpenAIImagesGenerations, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, "CreateBestEffort only when ForwardImages returns a result", "OpenAIGatewayHandler.handleImages", "TestTokenHiveAuxiliaryHandlersApplyDedicatedSchedulerPolicy", ""},
		{SourceOperationOpenAIImagesEdits, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, "CreateBestEffort only when ForwardImages returns a result", "OpenAIGatewayHandler.handleImages", "TestTokenHiveAuxiliaryHandlersApplyDedicatedSchedulerPolicy", ""},
		{SourceOperationOpenAIAlphaSearch, tokenHiveSlice4BestEffort, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "OpenAIGatewayHandler.AlphaSearch", "TestTokenHiveAuxiliaryHandlersApplyDedicatedSchedulerPolicy", ""},
		{SourceOperationOpenAICodexModelsManifest, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, tokenHiveSlice4NoUsage, "OpenAIGatewayHandler.CodexModels", "", "TestTokenHiveCodexModelsManifestDoesNotRecordUsage"},
	}
}

func TestTokenHiveSlice4OperationUsageCharacterization(t *testing.T) {
	wantOperations := []string{
		"anthropic.messages.create",
		"anthropic.messages.stream",
		"anthropic.messages.count_tokens",
		"openai.responses.http",
		"openai.responses.compact",
		"openai.responses.input_tokens",
		"openai.images.generations",
		"openai.images.edits",
		"openai.alpha_search",
		"openai.codex.models.manifest",
	}

	expectations := tokenHiveSlice4UsageExpectations()
	require.Len(t, expectations, len(wantOperations))
	for i, expectation := range expectations {
		require.Equal(t, wantOperations[i], expectation.Operation)
		require.NotEmpty(t, expectation.SourceSymbol)
		if expectation.Normal == tokenHiveSlice4NoUsage {
			require.NotEmpty(t, expectation.NoUsageSourceTest, "%s must be backed by a real-handler zero-call test", expectation.Operation)
			require.Empty(t, expectation.SourceTest)
		} else {
			require.Equal(t, tokenHiveSlice4BestEffort, expectation.Normal)
			require.NotEmpty(t, expectation.SourceTest)
		}
		require.NotEmpty(t, expectation.ProviderError)
		require.NotEmpty(t, expectation.Interrupted)
	}
}

type tokenHiveSlice4UsageWriterSpy struct {
	UsageLogRepository

	mu              sync.Mutex
	bestEffortErr   error
	bestEffortCalls int
	createCalls     int
	ctxErrors       []error
}

func (s *tokenHiveSlice4UsageWriterSpy) CreateBestEffort(ctx context.Context, _ *UsageLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bestEffortCalls++
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	return s.bestEffortErr
}

func (s *tokenHiveSlice4UsageWriterSpy) Create(ctx context.Context, _ *UsageLog) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.ctxErrors = append(s.ctxErrors, ctx.Err())
	return true, nil
}

func TestTokenHiveSlice4UsageWriterMethodAndFallback(t *testing.T) {
	t.Run("best effort is the primary write and request cancellation is detached", func(t *testing.T) {
		repo := &tokenHiveSlice4UsageWriterSpy{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		writeUsageLogBestEffort(ctx, repo, &UsageLog{RequestID: "slice4-primary"}, "slice4")

		require.Equal(t, 1, repo.bestEffortCalls)
		require.Zero(t, repo.createCalls)
		require.Equal(t, []error{nil}, repo.ctxErrors)
	})

	t.Run("best effort failure synchronously falls back to Create", func(t *testing.T) {
		repo := &tokenHiveSlice4UsageWriterSpy{bestEffortErr: errors.New("queue unavailable")}
		writeUsageLogBestEffort(context.Background(), repo, &UsageLog{RequestID: "slice4-fallback"}, "slice4")

		require.Equal(t, 1, repo.bestEffortCalls)
		require.Equal(t, 1, repo.createCalls)
		require.Equal(t, []error{nil, nil}, repo.ctxErrors)
	})
}

func TestTokenHiveSlice4AsyncUsageCanExposeHigherSequenceFirst(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
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

	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
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
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
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
