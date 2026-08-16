package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTokenHiveProvenance_GatewayReplaceModelInResponseBodyExactMatch(t *testing.T) {
	svc := &GatewayService{}

	tests := []struct {
		name string
		body string
		from string
		to   string
		want string
	}{
		{
			name: "exact upstream model is restored",
			body: `{"id":"msg_1","model":"claude-upstream","content":[]}`,
			from: "claude-upstream",
			to:   "claude-requested",
			want: `{"id":"msg_1","model":"claude-requested","content":[]}`,
		},
		{
			name: "non matching model is preserved",
			body: `{"id":"msg_1","model":"claude-other","content":[]}`,
			from: "claude-upstream",
			to:   "claude-requested",
			want: `{"id":"msg_1","model":"claude-other","content":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.replaceModelInResponseBody([]byte(tt.body), tt.from, tt.to)
			require.Equal(t, tt.want, string(got))
		})
	}
}

func TestTokenHiveProvenance_AnthropicStreamEventIsTerminal(t *testing.T) {
	tests := []struct {
		name      string
		eventName string
		data      string
		want      bool
	}{
		{name: "done sentinel", data: " [DONE] ", want: true},
		{name: "message stop event", eventName: " message_stop ", want: true},
		{name: "message stop data", data: `{"type":"message_stop"}`, want: true},
		{name: "content delta is not terminal", eventName: "content_block_delta", data: `{"type":"content_block_delta"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, anthropicStreamEventIsTerminal(tt.eventName, tt.data))
		})
	}
}

func TestTokenHiveProvenance_ParseSSEUsageIntString(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		want   int
		wantOK bool
	}{
		{name: "integer", value: "42", want: 42, wantOK: true},
		{name: "surrounding whitespace", value: " 17\t", want: 17, wantOK: true},
		{name: "negative integer", value: "-3", want: -3, wantOK: true},
		{name: "fraction is rejected", value: "1.5"},
		{name: "non numeric is rejected", value: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSSEUsageInt(tt.value)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTokenHiveProvenance_HandleNonStreamingResponseGJSONUsageProjection(t *testing.T) {
	t.Run("string cached tokens are reconciled", func(t *testing.T) {
		body := `{"usage":{"input_tokens":1,"cached_tokens":"7"}}`
		usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)

		require.Equal(t, 7, usage.CacheReadInputTokens)
		require.Equal(t, gjson.String, gjson.GetBytes(wire, "usage.cached_tokens").Type)
		require.Equal(t, "7", gjson.GetBytes(wire, "usage.cache_read_input_tokens").Raw)
	})

	t.Run("overflow cached tokens are not reconciled", func(t *testing.T) {
		body := `{"usage":{"input_tokens":1,"cached_tokens":9223372036854775808}}`
		usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)

		require.Zero(t, usage.CacheReadInputTokens)
		require.False(t, gjson.GetBytes(wire, "usage.cache_read_input_tokens").Exists())
		require.JSONEq(t, body, string(wire))
	})

	t.Run("string cache creation details are projected", func(t *testing.T) {
		body := `{"usage":{"input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":"4","ephemeral_1h_input_tokens":"5"}}}`
		usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)

		require.Equal(t, 4, usage.CacheCreation5mTokens)
		require.Equal(t, 5, usage.CacheCreation1hTokens)
		require.Equal(t, gjson.String, gjson.GetBytes(wire, "usage.cache_creation.ephemeral_5m_input_tokens").Type)
		require.Equal(t, gjson.String, gjson.GetBytes(wire, "usage.cache_creation.ephemeral_1h_input_tokens").Type)
	})

	t.Run("overflow nested cache creation projects minimum int64", func(t *testing.T) {
		body := `{"usage":{"input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":9223372036854775808}}}`
		usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)

		require.Equal(t, int64(-9223372036854775808), int64(usage.CacheCreation5mTokens))
		require.JSONEq(t, body, string(wire))
	})
}

func TestTokenHiveProvenance_ExtractUpstreamErrorMessageGJSONProjection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "numeric error message", body: `{"error":{"message":7}}`, want: "7"},
		{name: "boolean detail", body: `{"detail":false}`, want: "false"},
		{name: "top level message object", body: `{"message":{"reason":"denied"}}`, want: `{"reason":"denied"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractUpstreamErrorMessage([]byte(tt.body)))
		})
	}
}

func runTokenHiveProvenanceNonStreamingResponse(t *testing.T, body string) (*ClaudeUsage, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	svc := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}

	usage, err := svc.handleNonStreamingResponse(context.Background(), response, ctx, &Account{ID: 1}, "claude-source", "claude-source")
	require.NoError(t, err)
	require.NotNil(t, usage)
	return usage, recorder.Body.Bytes()
}
