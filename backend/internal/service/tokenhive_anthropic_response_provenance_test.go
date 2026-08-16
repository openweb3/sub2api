package service

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestTokenHiveProvenance_HandleNonStreamingResponseTopLevelNull(t *testing.T) {
	usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, "null")

	require.Equal(t, ClaudeUsage{}, *usage)
	require.Equal(t, "null", string(wire))
}

func TestTokenHiveProvenance_HandleNonStreamingResponseGJSONNonFiniteProjection(t *testing.T) {
	positiveBoundary := int64(math.Inf(1))
	negativeBoundary := int64(math.Inf(-1))
	for _, tt := range []struct {
		name string
		raw  string
		want int64
	}{
		{name: "positive overflow", raw: "1e309", want: positiveBoundary},
		{name: "negative overflow", raw: "-1e309", want: negativeBoundary},
	} {
		t.Run("cached tokens "+tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, gjson.Parse(tt.raw).Int())
			body := `{"usage":{"input_tokens":1,"cached_tokens":` + tt.raw + `}}`
			usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)

			if tt.want > 0 {
				require.Equal(t, int(tt.want), usage.CacheReadInputTokens)
				require.Equal(t, tt.want, gjson.GetBytes(wire, "usage.cache_read_input_tokens").Int())
			} else {
				require.Zero(t, usage.CacheReadInputTokens)
				require.False(t, gjson.GetBytes(wire, "usage.cache_read_input_tokens").Exists())
				require.Equal(t, body, string(wire))
			}
		})
	}

	body := `{"usage":{"input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":1e309,"ephemeral_1h_input_tokens":-1e309}}}`
	usage, wire := runTokenHiveProvenanceNonStreamingResponse(t, body)
	require.Equal(t, int(positiveBoundary), usage.CacheCreation5mTokens)
	require.Equal(t, int(negativeBoundary), usage.CacheCreation1hTokens)
	require.Equal(t, body, string(wire))
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

func TestTokenHiveProvenance_ExtractUpstreamErrorMessageNonFiniteAndUnderflow(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "positive overflow", body: `{"error":{"message":1e309}}`, want: "+Inf"},
		{name: "negative overflow", body: `{"error":{"message":-1e309}}`, want: "-Inf"},
		{name: "positive underflow", body: `{"error":{"message":1e-400}}`, want: "0"},
		{name: "negative underflow", body: `{"error":{"message":-1e-400}}`, want: "-0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, extractUpstreamErrorMessage([]byte(tt.body)))
		})
	}
}

func TestTokenHiveProvenance_ExtractUpstreamErrorMessageInnerJSONFirstCompleteMessage(t *testing.T) {
	tests := []struct {
		name  string
		inner string
	}{
		{name: "trailing complete text", inner: `{"error":{"message":"first"}} trailing text`},
		{name: "duplicate error key", inner: `{"error":{"message":"first"},"error":{"message":"second"}}`},
		{name: "invalid suffix after message", inner: `{"error":{"message":"first" garbage}}`},
		{name: "truncated parent object", inner: `{"error":{"message":"first"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]any{"message": tt.inner}})
			require.NoError(t, err)
			require.Equal(t, "first", extractUpstreamErrorMessage(body))
		})
	}
}

func TestTokenHiveProvenance_StreamingRoundsLargeDefaultFloatOnWireAndUsage(t *testing.T) {
	payload := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"mapped-model","usage":{"input_tokens":9007199254740993}}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	result, wire, err := runTokenHiveProvenanceStreamingResponse(t, payload, "client-model", "mapped-model", nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 9007199254740992, result.usage.InputTokens)
	require.Contains(t, string(wire), `"input_tokens":9007199254740992`)
	require.NotContains(t, string(wire), `"input_tokens":9007199254740993`)
}

func TestTokenHiveProvenance_StreamingOrdinaryNonJSONRunsToolReverseAtCallsite(t *testing.T) {
	rw := &ToolNameRewrite{ReverseOrdered: [][2]string{{"dynamic_fake", "dynamic_real"}}}
	payload := "data: opaque dynamic_fake cc_sess_list\n\n" + "data: [DONE]\n\n"

	result, wire, err := runTokenHiveProvenanceStreamingResponse(t, payload, "model", "model", rw)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "data: opaque dynamic_real sessions_list\n\ndata: [DONE]\n\n", string(wire))
}

func TestTokenHiveProvenance_StreamingEOFPendingTerminalWithoutDelimiterIsIncomplete(t *testing.T) {
	for _, tt := range []struct {
		name    string
		payload string
	}{
		{name: "done sentinel", payload: "data: [DONE]"},
		{name: "message stop", payload: "event: message_stop\ndata: {\"type\":\"message_stop\"}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, wire, err := runTokenHiveProvenanceStreamingResponse(t, tt.payload, "model", "model", nil)
			require.ErrorContains(t, err, "missing terminal event")
			require.NotNil(t, result)
			require.Empty(t, wire)
		})
	}
}

func TestTokenHiveProvenance_StreamingWhitespaceSeparatorDispatchesPendingEvent(t *testing.T) {
	payload := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n \t \n"

	result, wire, err := runTokenHiveProvenanceStreamingResponse(t, payload, "model", "model", nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", string(wire))
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

func runTokenHiveProvenanceStreamingResponse(t *testing.T, payload, originalModel, mappedModel string, rw *ToolNameRewrite) (*streamingResult, []byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if rw != nil {
		ctx.Set(toolNameRewriteKey, rw)
	}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}

	result, err := newStreamingResponseTestGatewayService().handleStreamingResponse(
		context.Background(), response, ctx, &Account{ID: 1}, time.Now(), originalModel, mappedModel, false,
	)
	return result, recorder.Body.Bytes(), err
}
