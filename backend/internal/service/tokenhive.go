package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	UpstreamTypeOpenAICodexOAuth                = "openai_codex_oauth"
	UpstreamTypeAnthropicOAuth                  = "anthropic_oauth"
	CapabilityOpenAICodexResponsesHTTP          = "openai.codex.responses.http"
	CapabilityOpenAICodexModelsManifest         = "openai.codex.models.manifest"
	TokenHiveHeaderRequestID                    = "X-TokenHive-Request-ID"
	TokenHiveHeaderRawURL                       = "X-TokenHive-Raw-URL"
	TokenHiveHeaderUpstreamType                 = "X-TokenHive-Upstream-Type"
	TokenHiveHeaderUpstreamModel                = "X-TokenHive-Upstream-Model"
	TokenHiveHeaderTenantKey                    = "X-TokenHive-Tenant-Key"
	TokenHiveHeaderMethod                       = "X-TokenHive-Method"
	TokenHiveHeaderSourceOperation              = "X-TokenHive-Source-Operation"
	TokenHiveHeaderStream                       = "X-TokenHive-Stream"
	SourceOperationAnthropicMessagesCreate      = "anthropic.messages.create"
	SourceOperationAnthropicMessagesStream      = "anthropic.messages.stream"
	SourceOperationAnthropicMessagesCountTokens = "anthropic.messages.count_tokens"
	SourceOperationOpenAIResponsesHTTP          = "openai.responses.http"
	SourceOperationOpenAIResponsesCompact       = "openai.responses.compact"
	SourceOperationOpenAIResponsesInputTokens   = "openai.responses.input_tokens"
	SourceOperationOpenAIImagesGenerations      = "openai.images.generations"
	SourceOperationOpenAIImagesEdits            = "openai.images.edits"
	SourceOperationOpenAIAlphaSearch            = "openai.alpha_search"
	SourceOperationOpenAICodexModelsManifest    = "openai.codex.models.manifest"
	tokenHiveHeaderPrefix                       = "x-tokenhive-"
	tokenHiveTenantKeyMessagePrefix             = "tokenhive:tenant-key:v1\x00api-key-record-id\x00"
)

var tokenHiveUpstreamPlatforms = map[string]string{
	UpstreamTypeOpenAICodexOAuth: PlatformOpenAI,
	UpstreamTypeAnthropicOAuth:   PlatformAnthropic,
}

var (
	errInvalidSourceOperation          = errors.New("invalid tokenhive source operation")
	errTokenHiveV1UnsupportedTransport = errors.New("tokenhive v1 unsupported transport")
	tokenHiveSourceOperations          = map[string]struct{}{
		SourceOperationAnthropicMessagesCreate:      {},
		SourceOperationAnthropicMessagesStream:      {},
		SourceOperationAnthropicMessagesCountTokens: {},
		SourceOperationOpenAIResponsesHTTP:          {},
		SourceOperationOpenAIResponsesCompact:       {},
		SourceOperationOpenAIResponsesInputTokens:   {},
		SourceOperationOpenAIImagesGenerations:      {},
		SourceOperationOpenAIImagesEdits:            {},
		SourceOperationOpenAIAlphaSearch:            {},
		SourceOperationOpenAICodexModelsManifest:    {},
	}
)

func (s *OpenAIGatewayService) rejectTokenHiveV1Transport(ctx context.Context, account *Account, transport string) error {
	mapped, err := resolveTokenHiveAccount(s.cfg, s.tokenHiveRegistry, account)
	if err != nil {
		return fmt.Errorf("resolve tokenhive account for v1 transport: %w", err)
	}
	if mapped == nil {
		return nil
	}
	logger.FromContext(ctx).Warn(
		"TokenHive v1 transport rejected",
		zap.Int64("account_id", account.ID),
		zap.String("transport", transport),
	)
	return fmt.Errorf("%w: TokenHive v1 does not support %s", errTokenHiveV1UnsupportedTransport, transport)
}

type TokenHiveAccount struct {
	AccountID    int64
	UpstreamType string
}

type TokenHiveRegistry struct {
	accounts map[int64]TokenHiveAccount
}

func NewTokenHiveRegistry(cfg config.TokenHiveConfig, accounts []Account) (*TokenHiveRegistry, error) {
	if err := config.ValidateTokenHiveConfig(cfg); err != nil {
		return nil, err
	}
	registry := &TokenHiveRegistry{accounts: make(map[int64]TokenHiveAccount)}
	if !cfg.Enabled {
		return registry, nil
	}
	accountsByID := make(map[int64]*Account, len(accounts))
	for i := range accounts {
		accountsByID[accounts[i].ID] = &accounts[i]
	}
	for accountID, upstreamType := range cfg.Accounts {
		account := accountsByID[accountID]
		if account == nil {
			return nil, fmt.Errorf("tokenhive mapped account %d does not exist", accountID)
		}
		if account.Type != AccountTypeAPIKey {
			return nil, fmt.Errorf("tokenhive mapped account %d must have type %s", accountID, AccountTypeAPIKey)
		}
		expectedPlatform, supported := tokenHiveUpstreamPlatforms[upstreamType]
		if !supported {
			return nil, fmt.Errorf("tokenhive mapped account %d has unsupported upstream type %q", accountID, upstreamType)
		}
		if account.Platform != expectedPlatform {
			return nil, fmt.Errorf("tokenhive mapped account %d must have platform %s", accountID, expectedPlatform)
		}
		registry.accounts[accountID] = TokenHiveAccount{AccountID: accountID, UpstreamType: upstreamType}
	}
	return registry, nil
}

func (r *TokenHiveRegistry) Match(account *Account) (TokenHiveAccount, bool) {
	if r == nil || account == nil || account.Type != AccountTypeAPIKey {
		return TokenHiveAccount{}, false
	}
	mapped, ok := r.accounts[account.ID]
	if !ok || tokenHiveUpstreamPlatforms[mapped.UpstreamType] != account.Platform {
		return TokenHiveAccount{}, false
	}
	return mapped, true
}

func resolveTokenHiveAccount(cfg *config.Config, registry *TokenHiveRegistry, account *Account) (*TokenHiveAccount, error) {
	if cfg == nil || !cfg.TokenHive.Enabled || account == nil {
		return nil, nil
	}
	if registry == nil {
		if _, configured := cfg.TokenHive.Accounts[account.ID]; !configured {
			return nil, nil
		}
		var err error
		registry, err = NewTokenHiveRegistry(cfg.TokenHive, []Account{*account})
		if err != nil {
			return nil, err
		}
	}
	mapped, ok := registry.Match(account)
	if !ok {
		return nil, nil
	}
	return &mapped, nil
}

func validateTokenHiveSourceOperation(value string) error {
	if _, ok := tokenHiveSourceOperations[value]; !ok {
		return errInvalidSourceOperation
	}
	return nil
}

func BuildTokenHiveMetadata(ctx context.Context, apiKeyRecordID int64, method string, rawURL string, upstreamModel string, stream bool, sourceOperation string, account TokenHiveAccount, key []byte) (http.Header, error) {
	if apiKeyRecordID <= 0 {
		return nil, fmt.Errorf("tokenhive API key record ID must be positive")
	}
	if _, supported := tokenHiveUpstreamPlatforms[account.UpstreamType]; !supported {
		return nil, fmt.Errorf("invalid tokenhive account mapping")
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("tokenhive tenant HMAC key must be at least 32 bytes")
	}
	if method != http.MethodGet && method != http.MethodPost {
		return nil, fmt.Errorf("invalid tokenhive logical method")
	}
	if err := validateTokenHiveSourceOperation(sourceOperation); err != nil {
		return nil, err
	}
	parsedRawURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsedRawURL.Scheme == "" || parsedRawURL.Host == "" {
		return nil, fmt.Errorf("invalid tokenhive raw URL")
	}
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil, fmt.Errorf("tokenhive upstream model must not be empty")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(tokenHiveTenantKeyMessagePrefix + strconv.FormatInt(apiKeyRecordID, 10)))
	tenantKey := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	headers := make(http.Header, 8)
	headers.Set(TokenHiveHeaderRequestID, ResolveUsageBillingRequestID(ctx, ""))
	headers.Set(TokenHiveHeaderRawURL, parsedRawURL.String())
	headers.Set(TokenHiveHeaderUpstreamType, account.UpstreamType)
	headers.Set(TokenHiveHeaderUpstreamModel, upstreamModel)
	headers.Set(TokenHiveHeaderTenantKey, tenantKey)
	headers.Set(TokenHiveHeaderMethod, method)
	headers.Set(TokenHiveHeaderSourceOperation, sourceOperation)
	headers.Set(TokenHiveHeaderStream, strconv.FormatBool(stream))
	return headers, nil
}

func applyTokenHiveHandoff(ctx context.Context, cfg *config.Config, account *TokenHiveAccount, apiKeyRecordID int64, upstreamModel string, stream bool, sourceOperation string, req *http.Request) error {
	if cfg == nil || !cfg.TokenHive.Enabled || account == nil || req == nil {
		return nil
	}
	key, err := config.DecodeTokenHiveTenantHMACKey(cfg.TokenHive.TenantHMACKey)
	if err != nil {
		return err
	}
	logicalMethod := req.Method
	metadata, err := BuildTokenHiveMetadata(ctx, apiKeyRecordID, logicalMethod, req.URL.String(), upstreamModel, stream, sourceOperation, *account, key)
	if err != nil {
		return err
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), tokenHiveHeaderPrefix) {
			delete(req.Header, name)
		}
	}
	for name, values := range metadata {
		req.Header[name] = append([]string(nil), values...)
	}
	proxyURL, err := url.Parse(strings.TrimSpace(cfg.TokenHive.ProxyURL))
	if err != nil {
		return fmt.Errorf("parse tokenhive proxy URL: %w", err)
	}
	req.URL = proxyURL
	req.Method = http.MethodPost
	req.Host = ""
	requestContext := WithTokenHiveInternalHandoff(req.Context())
	requestContext = WithHTTPUpstreamRedirectsDisabled(requestContext)
	*req = *req.WithContext(requestContext)
	return nil
}

type TokenHiveResponsePolicy struct {
	Dedicated              bool
	AllowSameAccountRetry  bool
	AllowAccountFailover   bool
	AllowAccountMutation   bool
	AllowRuntimeBlock      bool
	AllowSchedulerFeedback bool
}

func ResolveTokenHiveResponsePolicy(account *Account, registry *TokenHiveRegistry) TokenHiveResponsePolicy {
	_, dedicated := registry.Match(account)
	return TokenHiveResponsePolicy{
		Dedicated:              dedicated,
		AllowSameAccountRetry:  !dedicated,
		AllowAccountFailover:   !dedicated,
		AllowAccountMutation:   !dedicated,
		AllowRuntimeBlock:      !dedicated,
		AllowSchedulerFeedback: !dedicated,
	}
}

func ordinaryTokenHiveResponsePolicy() TokenHiveResponsePolicy {
	return TokenHiveResponsePolicy{
		AllowSameAccountRetry:  true,
		AllowAccountFailover:   true,
		AllowAccountMutation:   true,
		AllowRuntimeBlock:      true,
		AllowSchedulerFeedback: true,
	}
}

func (s *OpenAIGatewayService) ResolveTokenHiveResponsePolicy(account *Account) TokenHiveResponsePolicy {
	if s == nil {
		return ordinaryTokenHiveResponsePolicy()
	}
	if s.tokenHiveRegistry != nil {
		return ResolveTokenHiveResponsePolicy(account, s.tokenHiveRegistry)
	}
	mapped, err := resolveTokenHiveAccount(s.cfg, nil, account)
	if err != nil || mapped == nil {
		return ordinaryTokenHiveResponsePolicy()
	}
	return TokenHiveResponsePolicy{Dedicated: true}
}

func (s *GatewayService) ResolveTokenHiveResponsePolicy(account *Account) TokenHiveResponsePolicy {
	if s == nil || account == nil || account.Platform != PlatformAnthropic {
		return ordinaryTokenHiveResponsePolicy()
	}
	if s.tokenHiveRegistry != nil {
		return ResolveTokenHiveResponsePolicy(account, s.tokenHiveRegistry)
	}
	mapped, err := resolveTokenHiveAccount(s.cfg, nil, account)
	if err != nil || mapped == nil {
		return ordinaryTokenHiveResponsePolicy()
	}
	return TokenHiveResponsePolicy{Dedicated: true}
}

func (s *GatewayService) allowAnthropicSchedulerFeedback(accountID int64) bool {
	if s == nil || accountID <= 0 {
		return true
	}
	if s.tokenHiveRegistry != nil {
		mapped, ok := s.tokenHiveRegistry.accounts[accountID]
		return !ok || mapped.UpstreamType != UpstreamTypeAnthropicOAuth
	}
	if s.cfg == nil || !s.cfg.TokenHive.Enabled {
		return true
	}
	return s.cfg.TokenHive.Accounts[accountID] != UpstreamTypeAnthropicOAuth
}

type tokenHiveAnthropicHandoff struct {
	request       *http.Request
	originalModel string
	upstreamModel string
	stream        bool
}

func (s *GatewayService) prepareTokenHiveAnthropicHandoff(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
	logicalRoute string,
) (*tokenHiveAnthropicHandoff, bool, error) {
	mappedAccount, err := resolveTokenHiveAccount(s.cfg, s.tokenHiveRegistry, account)
	if err != nil {
		return nil, false, fmt.Errorf("resolve tokenhive account: %w", err)
	}
	if mappedAccount == nil || mappedAccount.UpstreamType != UpstreamTypeAnthropicOAuth {
		return nil, false, nil
	}
	if parsed == nil {
		return nil, true, fmt.Errorf("parse request: empty request")
	}

	originalModel := parsed.Model
	upstreamModel := account.GetMappedModel(originalModel)
	body := parsed.Body.Bytes()
	if upstreamModel != originalModel {
		body = s.replaceModelInBody(body, upstreamModel)
		if err := parsed.ReplaceBody(body); err != nil {
			return nil, true, fmt.Errorf("apply tokenhive account model mapping: %w", err)
		}
		parsed.Model = upstreamModel
	}

	targetURL := ""
	switch logicalRoute {
	case "messages":
		targetURL = claudeAPIURL
	case "count_tokens":
		targetURL = claudeAPICountTokensURL
	default:
		return nil, true, fmt.Errorf("unsupported tokenhive anthropic logical route %q", logicalRoute)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, true, err
	}
	if c != nil && c.Request != nil {
		for name, values := range c.Request.Header {
			if !allowedHeaders[strings.ToLower(name)] {
				continue
			}
			wireName := resolveWireCasing(name)
			for _, value := range values {
				addHeaderRaw(req.Header, wireName, value)
			}
		}
	}
	stripTokenHiveProviderCredentials(req.Header)
	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	sourceOperation := SourceOperationAnthropicMessagesCountTokens
	if logicalRoute == "messages" {
		sourceOperation = SourceOperationAnthropicMessagesCreate
		if parsed.Stream {
			sourceOperation = SourceOperationAnthropicMessagesStream
		}
	}
	stream := logicalRoute == "messages" && parsed.Stream
	if err := applyTokenHiveHandoff(ctx, s.cfg, mappedAccount, getAPIKeyIDFromContext(c), upstreamModel, stream, sourceOperation, req); err != nil {
		return nil, true, fmt.Errorf("build tokenhive anthropic handoff: %w", err)
	}
	return &tokenHiveAnthropicHandoff{request: req, originalModel: originalModel, upstreamModel: upstreamModel, stream: parsed.Stream}, true, nil
}

func stripTokenHiveProviderCredentials(headers http.Header) {
	deleteHeaderAllForms(headers, "authorization")
	deleteHeaderAllForms(headers, "x-api-key")
	deleteHeaderAllForms(headers, "cookie")
}

func (s *GatewayService) forwardTokenHiveAnthropicMessages(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
) (*ForwardResult, bool, error) {
	handoff, mapped, err := s.prepareTokenHiveAnthropicHandoff(ctx, c, account, parsed, "messages")
	if err != nil || !mapped {
		return nil, mapped, err
	}
	startTime := time.Now()
	resp, err := s.httpUpstream.Do(handoff.request, "", account.ID, account.Concurrency)
	if err != nil {
		return nil, true, fmt.Errorf("tokenhive anthropic request failed: %w", err)
	}
	if resp == nil || resp.Body == nil {
		return nil, true, errors.New("tokenhive anthropic request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()
	policy := s.ResolveTokenHiveResponsePolicy(account)
	if resp.StatusCode >= http.StatusBadRequest {
		result, err := s.handleErrorResponseWithPolicy(ctx, resp, c, account, policy, handoff.upstreamModel)
		return result, true, err
	}
	if parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}
	if handoff.stream {
		streamResult, streamErr := s.handleStreamingResponseWithPolicy(ctx, resp, c, account, startTime, handoff.originalModel, handoff.upstreamModel, false, policy)
		if streamErr != nil {
			var sseErr *sseStreamErrorEventError
			if errors.As(streamErr, &sseErr) {
				writeTokenHiveAnthropicSSEError(c, sseErr.RawData)
			}
			return partialStreamUsageResult(c, resp, streamResult, handoff.originalModel, handoff.upstreamModel, startTime, streamErr), true, streamErr
		}
		return &ForwardResult{RequestID: resp.Header.Get("x-request-id"), Usage: *streamResult.usage, Model: handoff.originalModel, UpstreamModel: handoff.upstreamModel, Stream: true, Duration: time.Since(startTime), FirstTokenMs: streamResult.firstTokenMs, ClientDisconnect: streamResult.clientDisconnect}, true, nil
	}
	usage, err := s.handleNonStreamingResponseWithPolicy(ctx, resp, c, account, handoff.originalModel, handoff.upstreamModel, policy)
	if err != nil {
		return nil, true, err
	}
	return &ForwardResult{RequestID: resp.Header.Get("x-request-id"), Usage: *usage, Model: handoff.originalModel, UpstreamModel: handoff.upstreamModel, Duration: time.Since(startTime)}, true, nil
}

func writeTokenHiveAnthropicSSEError(c *gin.Context, rawData string) {
	if c == nil {
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	MarkResponseCommitted(c)
	_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", rawData)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *GatewayService) forwardTokenHiveAnthropicCountTokens(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
) (bool, error) {
	handoff, mapped, err := s.prepareTokenHiveAnthropicHandoff(ctx, c, account, parsed, "count_tokens")
	if err != nil || !mapped {
		return mapped, err
	}
	resp, err := s.httpUpstream.Do(handoff.request, "", account.ID, account.Concurrency)
	if err != nil {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Request failed")
		return true, fmt.Errorf("tokenhive anthropic count_tokens request failed: %w", err)
	}
	if resp == nil || resp.Body == nil {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Failed to read response")
		return true, errors.New("tokenhive anthropic count_tokens request failed: empty response")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, func(c *gin.Context) {
		s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Upstream response too large")
	})
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			s.countTokensError(c, http.StatusBadGateway, "upstream_error", "Failed to read response")
		}
		return true, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if handled, executionErr := handleTokenHiveExecutionErrorResponse(resp, c, account, body, s.ResolveTokenHiveResponsePolicy(account), true); handled {
			return true, executionErr
		}
		s.countTokensError(c, resp.StatusCode, "upstream_error", "Upstream request failed")
		return true, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	c.Data(resp.StatusCode, "application/json", body)
	return true, nil
}

func handleTokenHiveExecutionErrorResponse(resp *http.Response, c *gin.Context, account *Account, body []byte, policy TokenHiveResponsePolicy, anthropicEnvelope bool) (bool, error) {
	source, code, ok := trustedTokenHiveExecutionError(resp, body)
	if !policy.Dedicated || !ok {
		return false, nil
	}
	const clientMessage = "TokenHive execution failed"
	setOpsUpstreamError(c, 0, clientMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Platform: account.Platform, AccountID: account.ID, AccountName: account.Name, Kind: "request_error", Stage: "tokenhive_execution", Scope: source, Reason: code, Message: clientMessage})
	MarkResponseCommitted(c)
	errorBody := gin.H{"message": clientMessage, "source": source, "type": code}
	if anthropicEnvelope {
		c.JSON(resp.StatusCode, gin.H{"type": "error", "error": errorBody})
	} else {
		c.JSON(resp.StatusCode, gin.H{"error": errorBody})
	}
	return true, fmt.Errorf("tokenhive execution error: source=%s code=%s", source, code)
}
