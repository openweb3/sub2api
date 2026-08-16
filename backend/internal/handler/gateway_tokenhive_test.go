package handler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAnthropicTokenHiveHandlerPolicyRejectsFailover(t *testing.T) {
	cfg := config.TokenHiveConfig{
		Enabled:       true,
		ProxyURL:      "http://127.0.0.1:18081/internal/v1/proxy",
		TenantHMACKey: "c2xpY2UtMi10ZXN0LXRlbmFudC1obWFjLWtleS0zMmJ5dGVz",
		Accounts:      map[int64]string{42: service.UpstreamTypeAnthropicOAuth},
	}
	account := &service.Account{ID: 42, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey}
	registry, err := service.NewTokenHiveRegistry(cfg, []service.Account{*account})
	require.NoError(t, err)
	policy := service.ResolveTokenHiveResponsePolicy(account, registry)
	failoverErr := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}

	got, ok := gatewayFailoverForPolicy(failoverErr, policy)
	require.False(t, ok)
	require.Nil(t, got)
}

func TestOrdinaryAnthropicHandlerPolicyPreservesFailover(t *testing.T) {
	policy := service.TokenHiveResponsePolicy{
		AllowSameAccountRetry:  true,
		AllowAccountFailover:   true,
		AllowAccountMutation:   true,
		AllowRuntimeBlock:      true,
		AllowSchedulerFeedback: true,
	}
	want := &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}
	err := errors.Join(errors.New("forward failed"), want)

	got, ok := gatewayFailoverForPolicy(err, policy)
	require.True(t, ok)
	require.Same(t, want, got)
}
