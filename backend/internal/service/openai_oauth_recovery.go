package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

func (s *OpenAIGatewayService) openAIOAuthCredentialOwner(ctx context.Context, account *Account) (*Account, error) {
	if account != nil && account.IsShadow() {
		if s.accountRepo == nil {
			return nil, errors.New("OpenAI OAuth credential owner is unavailable")
		}
		return resolveCredentialAccount(ctx, s.accountRepo, account)
	}
	return account, nil
}

type openAIRefreshRejection struct {
	credentials [sha256.Size]byte
	err         *openai.RefreshTokenError
}

func openAIRefreshFingerprint(account *Account) [sha256.Size]byte {
	// Do not retain raw tokens in failure state. Include auth identity as well as
	// both tokens so reauthorization, even for the same account, restores access.
	value, _ := json.Marshal([]string{account.Platform, account.Type, account.GetCredential("access_token"), account.GetCredential("refresh_token"), account.GetCredential("client_id"), account.GetCredential("chatgpt_account_id"), account.GetCredential(openAIAuthModeCredentialKey), account.GetCredential(openAIAuthModeLegacyCredentialKey)})
	return sha256.Sum256(value)
}

func (api *OAuthRefreshAPI) openAIRefreshFailure(account *Account) *openai.RefreshTokenError {
	if api == nil || account == nil || !account.IsOpenAIOAuth() {
		return nil
	}
	value, ok := api.openAIFailures.Load(account.ID)
	if !ok {
		return nil
	}
	failure := value.(openAIRefreshRejection)
	if failure.credentials == openAIRefreshFingerprint(account) {
		return failure.err
	}
	return nil
}

func (api *OAuthRefreshAPI) recordOpenAIRefreshFailure(account *Account, err *openai.RefreshTokenError) {
	if api == nil || account == nil || !account.IsOpenAIOAuth() || err == nil || !err.Permanent {
		return
	}
	api.openAIFailures.Store(account.ID, openAIRefreshRejection{credentials: openAIRefreshFingerprint(account), err: err})
}

func canRecoverOpenAIOAuth(account *Account) bool {
	return account != nil && account.IsOpenAIOAuth() && !account.IsOpenAIPersonalAccessToken() &&
		!account.IsOpenAIAgentIdentity() && !strings.HasPrefix(strings.TrimSpace(account.GetOpenAIAccessToken()), "at-") &&
		strings.TrimSpace(account.GetOpenAIRefreshToken()) != ""
}

type openAIUnauthorizedRefreshExecutor struct {
	OAuthRefreshExecutor
	rejectedToken string
	accountID     string
}

func (e *openAIUnauthorizedRefreshExecutor) CanRefresh(account *Account) bool {
	return canRecoverOpenAIOAuth(account) && e.OAuthRefreshExecutor.CanRefresh(account) && account.GetCredential("chatgpt_account_id") == e.accountID
}

func (e *openAIUnauthorizedRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	// A concurrent refresh already repaired this same account. Reuse the durable
	// access token instead of consuming its newly rotated refresh token again.
	return account.GetOpenAIAccessToken() == e.rejectedToken
}

// RefreshAfterUnauthorized performs at most one token-endpoint call, independent
// of expiry. It shares the proactive/background lock and rereads credentials
// before acting. Callers may replay the rejected HTTP request only once.
func (p *OpenAITokenProvider) RefreshAfterUnauthorized(ctx context.Context, account *Account, rejectedAccessToken string) (string, error) {
	if p == nil || p.refreshAPI == nil || p.executor == nil || !canRecoverOpenAIOAuth(account) || strings.TrimSpace(rejectedAccessToken) == "" {
		return "", errors.New("OpenAI OAuth unauthorized recovery is unavailable")
	}
	executor := &openAIUnauthorizedRefreshExecutor{OAuthRefreshExecutor: p.executor, rejectedToken: rejectedAccessToken, accountID: account.GetCredential("chatgpt_account_id")}
	result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), account, executor, 0)
	if err != nil {
		var rejection *openai.RefreshTokenError
		if errors.As(err, &rejection) {
			return "", rejection
		}
		return "", errors.New("OpenAI OAuth unauthorized recovery temporarily failed")
	}
	if result == nil || result.LockHeld || result.Account == nil {
		return "", errors.New("OpenAI OAuth refresh is already in progress")
	}
	token := result.Account.GetOpenAIAccessToken()
	if strings.TrimSpace(token) == "" {
		return "", errors.New("OpenAI OAuth refresh returned no access token")
	}
	if p.tokenCache != nil {
		// Delete the previous access token; the ordinary provider path will cache
		// the durable replacement with its expiry-derived TTL.
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(result.Account))
	}
	return token, nil
}

// RecordUnauthorizedAfterRefresh suppresses further refresh attempts only for
// the exact credential version used by the rejected replay. Never clear stored
// tokens or disable a concurrently reauthorized account.
func (p *OpenAITokenProvider) RecordUnauthorizedAfterRefresh(ctx context.Context, account *Account, rejectedAccessToken string) {
	if p == nil || p.refreshAPI == nil || p.accountRepo == nil || !canRecoverOpenAIOAuth(account) {
		return
	}
	mu := p.refreshAPI.getLocalLock(OpenAITokenCacheKey(account))
	if err := mu.Lock(ctx); err != nil {
		return
	}
	defer mu.Unlock()
	latest, err := p.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil || latest.ID != account.ID || !canRecoverOpenAIOAuth(latest) || latest.GetCredential("chatgpt_account_id") != account.GetCredential("chatgpt_account_id") || latest.GetOpenAIAccessToken() != rejectedAccessToken {
		return
	}
	p.refreshAPI.recordOpenAIRefreshFailure(latest, &openai.RefreshTokenError{StatusCode: http.StatusUnauthorized, Permanent: true})
	if p.tokenCache != nil {
		_ = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(latest))
	}
}
