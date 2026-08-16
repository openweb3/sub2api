package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type anthropicApplyCallLog struct {
	calls []string
}

func (l *anthropicApplyCallLog) add(call string) {
	l.calls = append(l.calls, call)
}

type anthropicApplyAdminService struct {
	*stubAdminService
	log *anthropicApplyCallLog

	existingAccount *service.Account
	updatedAccount  *service.Account
	clearedAccount  *service.Account
	updateInput     *service.UpdateAccountInput

	getCalls    int
	updateCalls int
	clearCalls  int
	getID       int64
	updateID    int64
	clearID     int64
}

func (s *anthropicApplyAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	s.log.add("get")
	s.getCalls++
	s.getID = id
	return s.existingAccount, nil
}

func (s *anthropicApplyAdminService) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.log.add("update")
	s.updateCalls++
	s.updateID = id
	s.updateInput = input
	s.updatedAccount = &service.Account{
		ID:           id,
		Name:         s.existingAccount.Name,
		Platform:     s.existingAccount.Platform,
		Type:         input.Type,
		Credentials:  input.Credentials,
		Status:       service.StatusError,
		ErrorMessage: s.existingAccount.ErrorMessage,
	}
	return s.updatedAccount, nil
}

func (s *anthropicApplyAdminService) ClearAccountError(_ context.Context, id int64) (*service.Account, error) {
	s.log.add("clear")
	s.clearCalls++
	s.clearID = id
	cleared := *s.updatedAccount
	cleared.ID = id
	cleared.Status = service.StatusActive
	cleared.ErrorMessage = ""
	s.clearedAccount = &cleared
	return s.clearedAccount, nil
}

type anthropicApplyTokenInvalidator struct {
	log     *anthropicApplyCallLog
	calls   int
	account *service.Account
}

func (i *anthropicApplyTokenInvalidator) InvalidateToken(_ context.Context, account *service.Account) error {
	i.log.add("invalidate")
	i.calls++
	i.account = account
	return nil
}

func TestApplyOAuthCredentialsAnthropicOAuthPersistsClearsErrorAndInvalidatesTokenCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log := &anthropicApplyCallLog{}
	adminService := &anthropicApplyAdminService{
		stubAdminService: newStubAdminService(),
		log:              log,
		existingAccount: &service.Account{
			ID:           42,
			Name:         "anthropic-oauth",
			Platform:     service.PlatformAnthropic,
			Type:         service.AccountTypeOAuth,
			Credentials:  map[string]any{"access_token": "old-access", "refresh_token": "old-refresh"},
			Status:       service.StatusError,
			ErrorMessage: "stale authentication error",
		},
	}
	invalidator := &anthropicApplyTokenInvalidator{log: log}
	handler := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, invalidator)
	router := gin.New()
	router.POST("/accounts/:id/apply-oauth-credentials", handler.ApplyOAuthCredentials)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/accounts/42/apply-oauth-credentials", bytes.NewBufferString(
		`{"type":"oauth","credentials":{"access_token":"new-access","refresh_token":"new-refresh"}}`,
	))
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"get", "update", "clear", "invalidate"}, log.calls)
	require.Equal(t, 1, adminService.getCalls)
	require.Equal(t, 1, adminService.updateCalls)
	require.Equal(t, 1, adminService.clearCalls)
	require.Equal(t, 1, invalidator.calls)
	require.Equal(t, int64(42), adminService.getID)
	require.Equal(t, int64(42), adminService.updateID)
	require.Equal(t, int64(42), adminService.clearID)
	require.Equal(t, service.AccountTypeOAuth, adminService.updateInput.Type)
	wantCredentials := map[string]any{"access_token": "new-access", "refresh_token": "new-refresh"}
	require.Equal(t, wantCredentials, adminService.updateInput.Credentials)
	require.Same(t, adminService.clearedAccount, invalidator.account)
	require.Equal(t, service.StatusActive, invalidator.account.Status)
	require.Empty(t, invalidator.account.ErrorMessage)
	require.Equal(t, wantCredentials, invalidator.account.Credentials)

	var responseBody struct {
		Data struct {
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseBody))
	require.Equal(t, service.StatusActive, responseBody.Data.Status)
	require.Empty(t, responseBody.Data.ErrorMessage)
}
