package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func (s *OpenAIGatewayService) recoverOpenAIOAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, *Account, bool) {
	if s.openAITokenProvider == nil {
		return headers, nil, false
	}
	owner, err := s.openAIOAuthCredentialOwner(ctx, account)
	if err != nil || !canRecoverOpenAIOAuth(owner) {
		return headers, nil, false
	}
	authorization := headers.Get("Authorization")
	rejected := strings.TrimPrefix(authorization, "Bearer ")
	if rejected == "" || rejected == authorization {
		return headers, nil, false
	}
	token, err := s.openAITokenProvider.RefreshAfterUnauthorized(ctx, owner, rejected)
	if err != nil {
		return headers, owner, false
	}
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+token)
	return updated, owner, true
}

// refreshOpenAIWSAuthHeaders also runs for delayed prewarming. Never retain an
// old OAuth bearer in a future dial after another request rotates credentials.
func (s *OpenAIGatewayService) refreshOpenAIWSAuthHeaders(ctx context.Context, account *Account, headers http.Header) (http.Header, error) {
	if s.isAgentIdentityAccount(ctx, account) {
		return s.refreshOpenAIAgentIdentityHeaders(ctx, account, headers)
	}
	if s.openAITokenProvider == nil {
		return headers, nil
	}
	owner, err := s.openAIOAuthCredentialOwner(ctx, account)
	if err != nil {
		return nil, err
	}
	if !canRecoverOpenAIOAuth(owner) {
		return headers, nil
	}
	if s.openAITokenProvider.accountRepo != nil {
		latest, readErr := s.openAITokenProvider.accountRepo.GetByID(ctx, owner.ID)
		if readErr != nil || latest == nil || latest.ID != owner.ID || !latest.IsActive() || !canRecoverOpenAIOAuth(latest) || latest.GetCredential("chatgpt_account_id") != owner.GetCredential("chatgpt_account_id") {
			return nil, errors.New("OpenAI OAuth credential state is unavailable")
		}
		owner = latest
	}
	token, err := s.openAITokenProvider.GetAccessToken(ctx, owner)
	if err != nil {
		return nil, err
	}
	updated := headers.Clone()
	updated.Set("Authorization", "Bearer "+token)
	return updated, nil
}

func (s *OpenAIGatewayService) acquireOpenAIWSWithAuthRecovery(ctx context.Context, pool *openAIWSConnPool, request *openAIWSAcquireRequest) (*openAIWSConnLease, error) {
	lease, err := pool.Acquire(ctx, *request)
	var dialErr *openAIWSDialError
	if err == nil || !errors.As(err, &dialErr) || dialErr.StatusCode != http.StatusUnauthorized || ctx.Err() != nil {
		return lease, err
	}
	headers, owner, recovered := s.recoverOpenAIOAuthHeaders(ctx, request.Account, request.Headers)
	if !recovered {
		return nil, err
	}
	request.Headers = headers
	// Do not reuse another connection authenticated with the rejected token.
	// Active leases belong to other turns and must not be closed by this retry.
	retry := cloneOpenAIWSAcquireRequest(*request)
	retry.ForceNewConn = true
	lease, err = pool.Acquire(ctx, retry)
	if err != nil && errors.As(err, &dialErr) && dialErr.StatusCode == http.StatusUnauthorized {
		s.openAITokenProvider.RecordUnauthorizedAfterRefresh(ctx, owner, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "))
	}
	return lease, err
}
