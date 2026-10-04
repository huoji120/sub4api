package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSessionMaskSeedConcurrentCreationReturnsOneWinner(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewIdentityCache(client)
	ctx := context.Background()
	const callers = 24
	type result struct {
		seed string
		err  error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			seed, err := cache.GetOrCreateSessionMaskSeed(ctx, 123, fmt.Sprintf("candidate-%d", i))
			results <- result{seed: seed, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	stored, err := cache.GetSessionMaskSeed(ctx, 123)
	require.NoError(t, err)
	for result := range results {
		require.NoError(t, result.err)
		require.Equal(t, stored, result.seed)
	}
	// A losing candidate must never replace the namespace chosen by the winner.
	winner, err := cache.GetOrCreateSessionMaskSeed(ctx, 123, "late-candidate")
	require.NoError(t, err)
	require.Equal(t, stored, winner)
	other, err := cache.GetOrCreateSessionMaskSeed(ctx, 456, "other-account")
	require.NoError(t, err)
	require.Equal(t, "other-account", other)
	require.NotEqual(t, stored, other)
}

func TestSessionMaskSeedIdleExpiryAndRefresh(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := NewIdentityCache(client)
	ctx := context.Background()
	missing, err := cache.GetSessionMaskSeed(ctx, 123)
	require.NoError(t, err)
	require.Empty(t, missing)
	seed, err := cache.GetOrCreateSessionMaskSeed(ctx, 123, "initial-seed")
	require.NoError(t, err)
	require.Equal(t, "initial-seed", seed)
	server.FastForward(14 * time.Minute)
	refreshed, err := cache.GetOrCreateSessionMaskSeed(ctx, 123, "")
	require.NoError(t, err)
	require.Equal(t, seed, refreshed)
	server.FastForward(14 * time.Minute)
	active, err := cache.GetSessionMaskSeed(ctx, 123)
	require.NoError(t, err)
	require.Equal(t, seed, active)
	server.FastForward(2 * time.Minute)
	expired, err := cache.GetSessionMaskSeed(ctx, 123)
	require.NoError(t, err)
	require.Empty(t, expired)
	// An expired read cannot resurrect its old namespace through an empty refresh.
	expired, err = cache.GetOrCreateSessionMaskSeed(ctx, 123, "")
	require.NoError(t, err)
	require.Empty(t, expired)
	rotated, err := cache.GetOrCreateSessionMaskSeed(ctx, 123, "rotated-seed")
	require.NoError(t, err)
	require.NotEqual(t, seed, rotated)
	stored, err := cache.GetSessionMaskSeed(ctx, 123)
	require.NoError(t, err)
	require.Equal(t, rotated, stored)
}
