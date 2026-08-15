package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const tokenHiveCodexModelsLogicalURL = "https://api.openai.com/v1/models"

// FetchCodexModelsManifestForClient routes only configured TokenHive virtual
// accounts through the generic handoff. Ordinary accounts retain the existing
// OAuth or custom API-key implementation unchanged.
func (s *OpenAIGatewayService) FetchCodexModelsManifestForClient(
	ctx context.Context,
	account *Account,
	clientVersion string,
	ifNoneMatch string,
	apiKeyRecordID int64,
) (*CodexModelsManifest, error) {
	mapped, err := resolveTokenHiveAccount(s.cfg, s.tokenHiveRegistry, account)
	if err != nil {
		return nil, err
	}
	if mapped == nil {
		return s.FetchCodexModelsManifest(ctx, account, clientVersion, ifNoneMatch)
	}

	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_MODELS_CLIENT_VERSION_REQUIRED", "client_version is required for the TokenHive Codex models manifest")
	}
	logicalURL, err := url.Parse(tokenHiveCodexModelsLogicalURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_REQUEST_FAILED", "parse TokenHive Codex models URL: %v", err)
	}
	query := logicalURL.Query()
	query.Set("client_version", clientVersion)
	logicalURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, logicalURL.String(), nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_REQUEST_FAILED", "create TokenHive Codex models request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if value := strings.TrimSpace(ifNoneMatch); value != "" {
		req.Header.Set("If-None-Match", value)
	}
	if err := applyTokenHiveHandoff(ctx, s.cfg, mapped, apiKeyRecordID, CapabilityOpenAICodexModelsManifest, req); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_MODELS_TOKENHIVE_HANDOFF_FAILED", "prepare TokenHive Codex models handoff: %v", err)
	}
	if s.httpUpstream == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_CODEX_MODELS_UPSTREAM_NOT_CONFIGURED", "Codex models upstream HTTP client is not configured")
	}
	resp, err := s.httpUpstream.Do(req, "", account.ID, account.Concurrency)
	if err != nil {
		return nil, newTokenHiveCodexModelsError(fmt.Errorf("TokenHive Codex models request failed: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return &CodexModelsManifest{ETag: resp.Header.Get("ETag"), NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, newTokenHiveCodexModelsError(fmt.Errorf("TokenHive Codex models upstream error %d: %s", resp.StatusCode, message))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, codexModelsManifestBodyLimit))
	if err != nil {
		return nil, newTokenHiveCodexModelsError(fmt.Errorf("read TokenHive Codex models response: %w", err))
	}
	if err := validateCodexModelsManifestEnvelope(body); err != nil {
		return nil, newTokenHiveCodexModelsError(fmt.Errorf("TokenHive Codex models returned an invalid envelope: %w", err))
	}
	return &CodexModelsManifest{Body: body, ETag: resp.Header.Get("ETag")}, nil
}

func newTokenHiveCodexModelsError(err error) error {
	return &codexModelsManifestUpstreamError{
		err:       infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_MODELS_UPSTREAM_FAILED", "%v", err),
		retryable: false,
	}
}
