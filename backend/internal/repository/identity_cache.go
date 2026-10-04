package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	fingerprintKeyPrefix     = "fingerprint:"
	fingerprintTTL           = 7 * 24 * time.Hour // 7天，配合每24小时懒续期可保持活跃账号永不过期
	sessionMaskSeedKeyPrefix = "session_mask_seed:"
	sessionMaskSeedTTL       = 15 * time.Minute
)

// fingerprintKey generates the Redis key for account fingerprint cache.
func fingerprintKey(accountID int64) string {
	return fmt.Sprintf("%s%d", fingerprintKeyPrefix, accountID)
}

// sessionMaskSeedKey isolates namespace seeds from obsolete fixed session IDs.
func sessionMaskSeedKey(accountID int64) string {
	return fmt.Sprintf("%s%d", sessionMaskSeedKeyPrefix, accountID)
}

type identityCache struct {
	rdb *redis.Client
}

func NewIdentityCache(rdb *redis.Client) service.IdentityCache {
	return &identityCache{rdb: rdb}
}

func (c *identityCache) GetFingerprint(ctx context.Context, accountID int64) (*service.Fingerprint, error) {
	key := fingerprintKey(accountID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	var fp service.Fingerprint
	if err := json.Unmarshal([]byte(val), &fp); err != nil {
		return nil, err
	}
	return &fp, nil
}

func (c *identityCache) SetFingerprint(ctx context.Context, accountID int64, fp *service.Fingerprint) error {
	key := fingerprintKey(accountID)
	val, err := json.Marshal(fp)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, val, fingerprintTTL).Err()
}

func (c *identityCache) GetSessionMaskSeed(ctx context.Context, accountID int64) (string, error) {
	val, err := c.rdb.Get(ctx, sessionMaskSeedKey(accountID)).Result()
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

var getOrCreateSessionMaskSeedScript = redis.NewScript(`
local seed = redis.call('GET', KEYS[1])
if not seed then
  if ARGV[1] == '' then
    return ''
  end
  seed = ARGV[1]
  redis.call('SET', KEYS[1], seed, 'PX', ARGV[2])
else
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return seed
`)

func (c *identityCache) GetOrCreateSessionMaskSeed(ctx context.Context, accountID int64, candidate string) (string, error) {
	return getOrCreateSessionMaskSeedScript.Run(ctx, c.rdb, []string{sessionMaskSeedKey(accountID)}, candidate, sessionMaskSeedTTL.Milliseconds()).Text()
}
