//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// The request has already captured its billing command when the owner deletes
// the key. Replaying that interleaving must charge once and keep the key revoked.
func TestUsageBillingRepositoryApply_SettlesSoftDeletedAPIKey(t *testing.T) {
	for _, subscriptionBilling := range []bool{false, true} {
		for _, limits := range []struct {
			name   string
			quota  bool
			window bool
		}{
			{name: "unlimited"},
			{name: "quota", quota: true},
			{name: "window", window: true},
			{name: "both", quota: true, window: true},
		} {
			for _, deleted := range []bool{false, true} {
				name := fmt.Sprintf("subscription=%t/%s/deleted=%t", subscriptionBilling, limits.name, deleted)
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					client := testEntClient(t)
					repo := NewUsageBillingRepository(client, integrationDB)
					keyRepo := NewAPIKeyRepository(client, integrationDB)
					user := mustCreateUser(t, client, &service.User{
						Email:   "deleted-key-billing-" + uuid.NewString() + "@example.com",
						Balance: 100,
					})
					key := &service.APIKey{UserID: user.ID, Key: "sk-test-" + uuid.NewString()}
					if limits.quota {
						key.Quota = 1 // Cross the threshold to verify status handling too.
					}
					if limits.window {
						key.RateLimit5h, key.RateLimit1d, key.RateLimit7d = 10, 20, 30
					}
					key = mustCreateApiKey(t, client, key)
					cmd := &service.UsageBillingCommand{
						RequestID: uuid.NewString(), UserID: user.ID, APIKeyID: key.ID,
						BalanceCost: 1.25,
					}
					if limits.quota {
						cmd.APIKeyQuotaCost = 1.25
					}
					if limits.window {
						cmd.APIKeyRateLimitCost = 1.25
					}
					if subscriptionBilling {
						group := mustCreateGroup(t, client, &service.Group{
							Name:             "deleted-key-billing-" + uuid.NewString(),
							Platform:         service.PlatformAnthropic,
							SubscriptionType: service.SubscriptionTypeSubscription,
						})
						sub := mustCreateSubscription(t, client, &service.UserSubscription{
							UserID: user.ID, GroupID: group.ID,
						})
						cmd.SubscriptionID = &sub.ID
						cmd.SubscriptionCost, cmd.BalanceCost = 1.25, 0
					}

					if deleted {
						require.NoError(t, keyRepo.DeleteWithAudit(ctx, key.ID))
					}
					var keyBefore, statusBefore string
					var deletedBefore sql.NullTime
					require.NoError(t, integrationDB.QueryRowContext(ctx,
						"SELECT key, status, deleted_at FROM api_keys WHERE id = $1", key.ID).
						Scan(&keyBefore, &statusBefore, &deletedBefore))

					first, err := repo.Apply(ctx, cmd)
					require.NoError(t, err)
					require.NotNil(t, first)
					require.True(t, first.Applied)
					require.Equal(t, limits.quota && !deleted, first.APIKeyQuotaExhausted)
					second, err := repo.Apply(ctx, cmd)
					require.NoError(t, err)
					require.NotNil(t, second)
					require.False(t, second.Applied, "retry must not bill a second time")

					var balance float64
					require.NoError(t, integrationDB.QueryRowContext(ctx,
						"SELECT balance FROM users WHERE id = $1", user.ID).Scan(&balance))
					expectedBalance := 98.75
					if subscriptionBilling {
						expectedBalance = 100
						var daily, weekly, monthly float64
						require.NoError(t, integrationDB.QueryRowContext(ctx,
							"SELECT daily_usage_usd, weekly_usage_usd, monthly_usage_usd FROM user_subscriptions WHERE id = $1", *cmd.SubscriptionID).
							Scan(&daily, &weekly, &monthly))
						for _, used := range []float64{daily, weekly, monthly} {
							require.InDelta(t, 1.25, used, 1e-8)
						}
					}
					require.InDelta(t, expectedBalance, balance, 1e-8)

					var quotaUsed, usage5h, usage1d, usage7d float64
					var keyAfter, statusAfter string
					var deletedAfter sql.NullTime
					require.NoError(t, integrationDB.QueryRowContext(ctx,
						"SELECT quota_used, usage_5h, usage_1d, usage_7d, key, status, deleted_at FROM api_keys WHERE id = $1", key.ID).
						Scan(&quotaUsed, &usage5h, &usage1d, &usage7d, &keyAfter, &statusAfter, &deletedAfter))
					expectedQuota, expectedWindow := 0.0, 0.0
					if limits.quota {
						expectedQuota = 1.25
					}
					if limits.window {
						expectedWindow = 1.25
					}
					require.InDelta(t, expectedQuota, quotaUsed, 1e-8)
					for _, used := range []float64{usage5h, usage1d, usage7d} {
						require.InDelta(t, expectedWindow, used, 1e-8)
					}
					require.Equal(t, keyBefore, keyAfter, "settlement must not restore credentials")
					require.Equal(t, deletedBefore, deletedAfter)
					if deleted {
						require.True(t, deletedAfter.Valid)
						require.Equal(t, statusBefore, statusAfter)
						_, err = keyRepo.GetByKeyForAuth(ctx, key.Key)
						require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
						_, err = keyRepo.GetByID(ctx, key.ID)
						require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
					} else if limits.quota {
						require.Equal(t, service.StatusAPIKeyQuotaExhausted, statusAfter)
					} else {
						require.Equal(t, statusBefore, statusAfter)
					}
					var count int
					require.NoError(t, integrationDB.QueryRowContext(ctx,
						"SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2", cmd.RequestID, key.ID).Scan(&count))
					require.Equal(t, 1, count)
				})
			}
		}
	}
}
