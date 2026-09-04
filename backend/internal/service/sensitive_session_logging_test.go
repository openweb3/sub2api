package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityLogPrefixDoesNotExposeSessionID(t *testing.T) {
	const sessionID = "session-request-body-feature-bearer-token-secret"

	require.Equal(t,
		"[antigravity-Forward] session=[present] account=account-safe",
		logPrefix(sessionID, "account-safe"),
	)
	require.Equal(t,
		"[antigravity-Forward] account=account-safe",
		logPrefix("", "account-safe"),
	)
	require.NotContains(t, logPrefix(sessionID, "account-safe"), sessionID)
}

func TestClaudeMimicDebugLineDoesNotExposeMetadataOrSystemContent(t *testing.T) {
	const (
		metadataUserID = "metadata-device-account-session-bearer-token-secret"
		systemContent  = "system-request-body-feature-sk-ant-test-secret"
	)
	req, err := http.NewRequest(
		http.MethodPost,
		"https://url-userinfo-secret@api.anthropic.com/path-request-body-feature-secret?access_token=query-bearer-token-secret",
		nil,
	)
	require.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.78")
	req.Header.Set("Authorization", "Bearer auth-secret")
	body := []byte(`{"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},"system":` + strconvQuote(systemContent) + `}`)

	line := buildClaudeMimicDebugLine(req, body, &Account{ID: 42, Name: "safe-account"}, "oauth", true)

	require.Contains(t, line, "meta.user_id.present=true")
	require.Contains(t, line, "system.present=true")
	require.NotContains(t, line, "meta.user_id=")
	require.NotContains(t, line, "system.preview=")
	for _, secret := range []string{
		metadataUserID,
		systemContent,
		"bearer-token-secret",
		"sk-ant-test-secret",
		"auth-secret",
		"url-userinfo-secret",
		"path-request-body-feature-secret",
		"query-bearer-token-secret",
		"access_token=",
	} {
		require.NotContains(t, line, secret)
	}
}

func TestGatewayDebugSnapshotDoesNotPersistSensitiveValues(t *testing.T) {
	const (
		sessionID      = "session-header-bearer-token-secret"
		metadataUserID = "metadata-device-account-session-secret"
		bodyContent    = "user-request-body-feature-sk-ant-test-secret"
	)
	sink, restore := captureStructuredLog(t)
	defer restore()

	path := filepath.Join(t.TempDir(), "gateway-debug-secret-path.log")
	svc := &GatewayService{}
	svc.initDebugGatewayBodyFile(path)
	f := svc.debugGatewayBodyFile.Load()
	require.NotNil(t, f)
	t.Cleanup(func() { _ = f.Close() })

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.False(t, sink.ContainsField("path"))
	require.True(t, sink.ContainsFieldValue("path_present", "true"))

	headers := http.Header{}
	headers.Set("Authorization", "Bearer auth-header-secret")
	headers.Set("X-Api-Key", "api-key-header-secret")
	headers.Set("Cookie", "cookie=session-cookie-secret")
	headers.Set("Session_Id", sessionID)
	headers.Set("X-Claude-Code-Session-Id", "claude-session-header-secret")
	headers.Set("X-Custom-Token", "custom-token-header-secret")
	headers.Set("X-Trace-Context", "opaque-header-bearer-token-secret")
	headers.Set("Content-Type", "application/json")
	body := []byte(`{"metadata":{"user_id":` + strconvQuote(metadataUserID) + `},"system":"system-secret","messages":[{"role":"user","content":` + strconvQuote(bodyContent) + `}]}`)
	svc.debugLogGatewaySnapshot("CLIENT_ORIGINAL", headers, body, map[string]string{
		"model":        "claude-model-request-body-secret",
		"stream":       "true",
		"url":          "https://api.example.test/v1/messages?access_token=query-secret",
		"unsafe_extra": "extra-bearer-token-secret",
	})
	require.NoError(t, f.Sync())
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	logText := string(written)

	require.Contains(t, logText, "valid_json: true")
	require.Contains(t, logText, "metadata_user_id_present: true")
	require.Contains(t, logText, "system_present: true")
	require.Contains(t, logText, "messages_present: true")
	require.Contains(t, logText, "unsafe_extra: [present]")
	require.Contains(t, logText, "model: [present]")
	require.Contains(t, logText, "X-Trace-Context: [present]")
	require.NotContains(t, logText, "access_token=")
	for _, secret := range []string{
		sessionID,
		metadataUserID,
		bodyContent,
		"auth-header-secret",
		"api-key-header-secret",
		"session-cookie-secret",
		"claude-session-header-secret",
		"custom-token-header-secret",
		"opaque-header-bearer-token-secret",
		"claude-model-request-body-secret",
		"system-secret",
		"query-secret",
		"extra-bearer-token-secret",
		"sk-ant-test-secret",
	} {
		require.NotContains(t, logText, secret)
	}
}

func TestIdentityFingerprintCreationLogDoesNotExposeClientID(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)
	sink, restore := captureStructuredLog(t)
	defer restore()

	fingerprint, err := svc.GetOrCreateFingerprint(context.Background(), 321, http.Header{"User-Agent": []string{"claude-cli/2.1.78"}})
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint.ClientID)
	require.True(t, sink.ContainsMessage("Created new fingerprint for account 321 with client_id_present=true"))
	require.False(t, sink.ContainsMessage(fingerprint.ClientID))
}

func TestIdentityMaskingLogsDoNotExposeSessionOrMetadataUserID(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)
	originalUserID := FormatMetadataUserID(
		"device-request-body-feature",
		"account-bearer-token-secret",
		"7578cf37-aaca-46e4-a45c-71285d9dbb83",
		"2.1.78",
	)
	body := []byte(`{"messages":[],"metadata":{"user_id":` + strconvQuote(originalUserID) + `}}`)
	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"session_id_masking_enabled": true,
		},
	}

	rewritten, err := svc.RewriteUserID(body, account.ID, "account-bearer-token-secret", "client-sk-ant-test-secret", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	beforeMasking := gjson.GetBytes(rewritten, "metadata.user_id").String()
	require.NotEmpty(t, beforeMasking)

	sink, restore := captureStructuredLog(t)
	defer restore()
	result, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, "account-bearer-token-secret", "client-sk-ant-test-secret", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	afterMasking := gjson.GetBytes(result, "metadata.user_id").String()
	require.NotEmpty(t, afterMasking)
	require.NotEmpty(t, cache.maskedSessionID)

	sink.mu.Lock()
	events := append([]*logger.LogEvent(nil), sink.events...)
	sink.mu.Unlock()
	var maskingFields map[string]any
	var serialized strings.Builder
	for _, event := range events {
		if event == nil {
			continue
		}
		_, _ = fmt.Fprintf(&serialized, "%s %v\n", event.Message, event.Fields)
		if event.Message == "session_id_masking_applied" {
			maskingFields = event.Fields
		}
	}

	require.True(t, sink.ContainsMessage("Generated new masked session ID for account 123 (present=true)"))
	require.Equal(t, true, maskingFields["metadata_user_id_before_present"])
	require.Equal(t, true, maskingFields["metadata_user_id_after_present"])
	require.Equal(t, true, maskingFields["metadata_user_id_changed"])
	require.NotContains(t, maskingFields, "before")
	require.NotContains(t, maskingFields, "after")

	logs := serialized.String()
	for _, secret := range []string{
		originalUserID,
		beforeMasking,
		afterMasking,
		cache.maskedSessionID,
		"device-request-body-feature",
		"account-bearer-token-secret",
		"client-sk-ant-test-secret",
		"bearer-token-secret",
	} {
		require.NotContains(t, logs, secret)
	}
}
