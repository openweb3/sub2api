package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTokenHiveMappedResponsesWebSocketRejectsBeforeHTTPOrWSDial(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			result := runTokenHiveResponsesWebSocketIngress(t, true, mode)

			require.ErrorContains(t, result.err, "TokenHive v1 does not support Responses WebSocket")
			require.Equal(t, 0, result.httpCalls)
			require.Equal(t, 0, result.wsDials)
		})
	}
}

func TestTokenHiveOrdinaryResponsesWebSocketKeepsExistingUpstreamPath(t *testing.T) {
	tests := []struct {
		mode      string
		httpCalls int
		wsDials   int
		wantError bool
	}{
		{mode: OpenAIWSIngressModePassthrough, httpCalls: 0, wsDials: 1},
		{mode: OpenAIWSIngressModeHTTPBridge, httpCalls: 1, wsDials: 0, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			result := runTokenHiveResponsesWebSocketIngress(t, false, test.mode)

			if test.wantError {
				require.Error(t, result.err)
			} else {
				require.NoError(t, result.err)
			}
			require.Equal(t, test.httpCalls, result.httpCalls)
			require.Equal(t, test.wsDials, result.wsDials)
		})
	}
}

type tokenHiveResponsesWebSocketResult struct {
	err       error
	httpCalls int
	wsDials   int
}

func runTokenHiveResponsesWebSocketIngress(t *testing.T, mapped bool, mode string) tokenHiveResponsesWebSocketResult {
	t.Helper()
	gin.SetMode(gin.TestMode)

	account := &Account{
		ID:          9501,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    mode,
		},
	}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 1

	var registry *TokenHiveRegistry
	if mapped {
		cfg.TokenHive = tokenHiveConfigForTest(account.ID)
		registry = tokenHiveTransportTestRegistry(t, cfg.TokenHive, *account)
	} else {
		mappedAccount := *account
		mappedAccount.ID++
		cfg.TokenHive = tokenHiveConfigForTest(mappedAccount.ID)
		registry = tokenHiveTransportTestRegistry(t, cfg.TokenHive, *account, mappedAccount)
	}

	httpUpstream := &httpUpstreamRecorder{err: errors.New("ordinary HTTP upstream result")}
	wsDialer := &openAIWSCaptureDialer{
		conn: &openAIWSCaptureConn{},
	}
	service := &OpenAIGatewayService{
		cfg:                       cfg,
		tokenHiveRegistry:         registry,
		httpUpstream:              httpUpstream,
		cache:                     &stubGatewayCache{},
		openaiWSResolver:          NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:             NewCodexToolCorrector(),
		openaiWSPassthroughDialer: wsDialer,
	}

	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		conn, err := coderws.Accept(w, request, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(request.Context(), time.Second)
		_, firstMessage, err := conn.Read(readCtx)
		cancelRead()
		if err != nil {
			errCh <- err
			return
		}
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = request
		errCh <- service.ProxyResponsesWebSocketFromClient(
			request.Context(), ginContext, conn, account, "sk-test", firstMessage, nil,
		)
	}))
	defer server.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), time.Second)
	require.NoError(t, client.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)))
	cancelWrite()
	_ = client.CloseNow()

	var proxyErr error
	select {
	case proxyErr = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Responses WebSocket proxy result")
	}
	return tokenHiveResponsesWebSocketResult{
		err:       proxyErr,
		httpCalls: len(httpUpstream.requests),
		wsDials:   wsDialer.DialCount(),
	}
}

func tokenHiveTransportTestRegistry(t *testing.T, cfg config.TokenHiveConfig, accounts ...Account) *TokenHiveRegistry {
	t.Helper()
	registry, err := NewTokenHiveRegistry(cfg, accounts)
	require.NoError(t, err)
	return registry
}

func TestTokenHiveMappedLiveRejectsBeforeHTTPOrWSDial(t *testing.T) {
	logSink, restoreLog := captureStructuredLog(t)
	defer restoreLog()

	account := tokenHiveLiveTestAccount()
	cfg := &config.Config{TokenHive: tokenHiveConfigForTest(account.ID)}
	registry, err := NewTokenHiveRegistry(cfg.TokenHive, []Account{*account})
	require.NoError(t, err)
	httpUpstream := &liveHTTPUpstreamStub{}
	wsDialer := &openAIWSCaptureDialer{conn: &openAIWSCaptureConn{}}
	service := &OpenAIGatewayService{
		cfg:                       cfg,
		tokenHiveRegistry:         registry,
		httpUpstream:              httpUpstream,
		openaiWSPassthroughDialer: wsDialer,
	}

	_, err = service.createUpstreamLiveCall(context.Background(), account, tokenHiveLiveTestRequest(), "test-attestation")

	require.ErrorContains(t, err, "TokenHive v1 does not support Live")
	require.False(t, service.shouldFailoverLiveCreateError(err))
	require.Nil(t, httpUpstream.request)
	require.Equal(t, 0, wsDialer.DialCount())
	require.True(t, logSink.ContainsMessageAtLevel("TokenHive v1 transport rejected", "warn"))
	require.True(t, logSink.ContainsFieldValue("account_id", "9502"))
	require.True(t, logSink.ContainsFieldValue("transport", "Live"))
	for _, field := range []string{"token", "authorization", "cookie", "tenant_key", "body", "frame"} {
		require.False(t, logSink.ContainsField(field))
	}
}

func TestTokenHiveOrdinaryLiveKeepsExistingUpstreamResult(t *testing.T) {
	account := tokenHiveLiveTestAccount()
	mappedAccount := *account
	mappedAccount.ID++
	cfg := &config.Config{TokenHive: tokenHiveConfigForTest(mappedAccount.ID)}
	httpUpstream := &liveHTTPUpstreamStub{}
	service := &OpenAIGatewayService{
		cfg:               cfg,
		tokenHiveRegistry: tokenHiveTransportTestRegistry(t, cfg.TokenHive, *account, mappedAccount),
		httpUpstream:      httpUpstream,
	}

	created, err := service.createUpstreamLiveCall(context.Background(), account, tokenHiveLiveTestRequest(), "test-attestation")

	require.NoError(t, err)
	require.Equal(t, "call_test", created.CallID)
	require.NotNil(t, httpUpstream.request)
}

func tokenHiveLiveTestAccount() *Account {
	return &Account{
		ID:          9502,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":            "sk-test",
			"chatgpt_account_id": "acct-test",
		},
	}
}

func tokenHiveLiveTestRequest() *LiveCallRequest {
	return &LiveCallRequest{
		SDP:     "v=offer\r\n",
		Session: []byte(`{"model":"gpt-live-test"}`),
	}
}
