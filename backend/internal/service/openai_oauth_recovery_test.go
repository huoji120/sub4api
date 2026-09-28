//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openAIRecoveryRepo struct {
	AccountRepository
	mu       sync.Mutex
	account  *Account
	setError int
}

func (r *openAIRecoveryRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), nil
}

func (r *openAIRecoveryRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account = snapshotOAuthRefreshAccount(r.account)
	r.account.Credentials = shallowCopyMap(credentials)
	return nil
}

func (r *openAIRecoveryRepo) SetError(context.Context, int64, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setError++
	return nil
}

type openAIRecoveryExecutor struct {
	calls   atomic.Int32
	refresh func(context.Context, *Account) (map[string]any, error)
}

func (e *openAIRecoveryExecutor) CanRefresh(account *Account) bool {
	return (&OpenAITokenRefresher{}).CanRefresh(account)
}

func (e *openAIRecoveryExecutor) NeedsRefresh(account *Account, window time.Duration) bool {
	return (&OpenAITokenRefresher{}).NeedsRefresh(account, window)
}

func (e *openAIRecoveryExecutor) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

func (e *openAIRecoveryExecutor) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	e.calls.Add(1)
	if e.refresh != nil {
		return e.refresh(ctx, account)
	}
	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = "repaired-access"
	credentials["refresh_token"] = "rotated-refresh"
	credentials["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return credentials, nil
}

func newOpenAIRecoveryFixture() (*Account, *openAIRecoveryRepo, *openAIRecoveryExecutor, *OpenAITokenProvider) {
	account := &Account{ID: 9021, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 4, Credentials: map[string]any{
		"access_token": "rejected-access", "refresh_token": "old-refresh",
		"chatgpt_account_id": "same-account", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}
	repo := &openAIRecoveryRepo{account: snapshotOAuthRefreshAccount(account)}
	executor := &openAIRecoveryExecutor{}
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil), executor)
	return account, repo, executor, provider
}

func TestOpenAIOAuthRecovery_ProactiveRefreshWithinFiveMinutes(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	account.Credentials["expires_at"] = time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339)
	repo.account = snapshotOAuthRefreshAccount(account)
	token, err := provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "repaired-access", token)
	require.Equal(t, int32(1), executor.calls.Load())
}

func TestOpenAIOAuthRecovery_PermanentRejectionFailsFastAndPreservesCredentials(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	executor.refresh = func(context.Context, *Account) (map[string]any, error) {
		return nil, openai.NewRefreshTokenError(400, []byte(`{"error":{"code":"refresh_token_reused","message":"secret-refresh-token"}}`))
	}
	for range 2 {
		token, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
		var rejection *openai.RefreshTokenError
		require.ErrorAs(t, err, &rejection)
		require.True(t, rejection.Permanent)
		require.Empty(t, token)
		require.NotContains(t, err.Error(), "secret")
	}
	_, err := provider.GetAccessToken(context.Background(), account)
	require.Error(t, err)
	require.Equal(t, int32(1), executor.calls.Load())
	require.Equal(t, account.Credentials, repo.account.Credentials)
	require.Zero(t, repo.setError, "request path rejection must not disable a newer credential version")
}

func TestOpenAIOAuthRecovery_TransientFailureDoesNotLatchOrDisable(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	executor.refresh = func(context.Context, *Account) (map[string]any, error) {
		return nil, errors.New("temporary transport failure secret-refresh-token")
	}
	for range 2 {
		_, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
	require.Equal(t, int32(2), executor.calls.Load())
	require.Nil(t, provider.refreshAPI.openAIRefreshFailure(account))
	require.Equal(t, account.Credentials, repo.account.Credentials)
	require.Zero(t, repo.setError)
	token, err := provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "rejected-access", token)
}

func TestOpenAIOAuthRecovery_ConcurrentUnauthorizedRefreshesOnce(t *testing.T) {
	account, _, executor, provider := newOpenAIRecoveryFixture()
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan string, 12)
	errorsCh := make(chan error, 12)
	for range cap(results) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
			results <- token
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}
	for token := range results {
		require.Equal(t, "repaired-access", token)
	}
	require.Equal(t, int32(1), executor.calls.Load())
}

func TestOpenAIOAuthRecovery_RefreshRaceUsesRotatedToken(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	executor.refresh = func(ctx context.Context, used *Account) (map[string]any, error) {
		credentials := shallowCopyMap(used.Credentials)
		credentials["access_token"] = "winner-access"
		credentials["refresh_token"] = "winner-refresh"
		require.NoError(t, repo.UpdateCredentials(ctx, used.ID, credentials))
		return nil, openai.NewRefreshTokenError(401, []byte(`{"error":{"code":"refresh_token_reused"}}`))
	}
	token, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
	require.NoError(t, err)
	require.Equal(t, "winner-access", token)
	require.Nil(t, provider.refreshAPI.openAIRefreshFailure(repo.account))
	require.Zero(t, repo.setError)
}

func TestOpenAIOAuthRecovery_SecondUnauthorizedBlocksOnlyRejectedVersion(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	token, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
	require.NoError(t, err)
	provider.RecordUnauthorizedAfterRefresh(context.Background(), account, token)
	_, err = provider.RefreshAfterUnauthorized(context.Background(), account, token)
	require.Error(t, err)
	_, err = provider.GetAccessToken(context.Background(), account)
	require.Error(t, err)
	require.Equal(t, int32(1), executor.calls.Load())
	credentials := shallowCopyMap(repo.account.Credentials)
	credentials["access_token"] = "reauthorized-access"
	credentials["refresh_token"] = "reauthorized-refresh"
	require.NoError(t, repo.UpdateCredentials(context.Background(), account.ID, credentials))
	provider.RecordUnauthorizedAfterRefresh(context.Background(), account, token)
	token, err = provider.GetAccessToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "reauthorized-access", token)
	require.Zero(t, repo.setError)
}

func TestOpenAIOAuthRecovery_DoesNotRefreshAPIKeySetupTokenOrPAT(t *testing.T) {
	for _, authType := range []string{AccountTypeAPIKey, AccountTypeSetupToken, "pat", "legacy-pat"} {
		t.Run(authType, func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			switch authType {
			case "pat":
				account.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
			case "legacy-pat":
				account.Credentials["access_token"] = "at-imported-pat"
				account.Credentials["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
				delete(account.Credentials, "refresh_token")
			default:
				account.Type = authType
			}
			repo.account = snapshotOAuthRefreshAccount(account)
			_, err := provider.RefreshAfterUnauthorized(context.Background(), account, account.GetOpenAIAccessToken())
			require.Error(t, err)
			require.Zero(t, executor.calls.Load())
			if authType == "legacy-pat" {
				token, err := provider.GetAccessToken(context.Background(), account)
				require.NoError(t, err)
				require.Equal(t, "at-imported-pat", token)
				require.Zero(t, repo.setError)
			}
		})
	}
}

func TestOpenAIOAuthRecovery_ChangedAccountIdentityIsNotReplayed(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	repo.account.Credentials["chatgpt_account_id"] = "different-login"
	_, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
	require.Error(t, err)
	require.Zero(t, executor.calls.Load())
	require.Zero(t, repo.setError)
}

func TestOpenAIRefreshErrorClassification_DoesNotDisableTransientGrantResponses(t *testing.T) {
	require.False(t, isNonRetryableRefreshError(openai.NewRefreshTokenError(503, []byte(`{"error":"invalid_grant"}`))))
	require.True(t, isNonRetryableRefreshError(openai.NewRefreshTokenError(401, []byte(`{"error":"unknown"}`))))
	require.True(t, isSharedProviderRefreshError(openai.NewRefreshTokenError(400, []byte(`{"error":"invalid_client"}`))))
}
