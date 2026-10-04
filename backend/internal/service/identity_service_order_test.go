package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type identityCacheStub struct {
	seeds     map[int64]string
	getErr    error
	createErr error
	readSeed  string
}

func (s *identityCacheStub) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	return nil, nil
}
func (s *identityCacheStub) SetFingerprint(_ context.Context, _ int64, _ *Fingerprint) error {
	return nil
}
func (s *identityCacheStub) GetSessionMaskSeed(_ context.Context, accountID int64) (string, error) {
	if s.readSeed != "" {
		return s.readSeed, s.getErr
	}
	return s.seeds[accountID], s.getErr
}
func (s *identityCacheStub) GetOrCreateSessionMaskSeed(_ context.Context, accountID int64, candidate string) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	if s.seeds == nil {
		s.seeds = make(map[int64]string)
	}
	if s.seeds[accountID] == "" {
		s.seeds[accountID] = candidate
	}
	return s.seeds[accountID], nil
}
func (s *identityCacheStub) GetClaudeCodeHeaders(_ context.Context, _ int64) (http.Header, error) {
	return nil, nil
}
func (s *identityCacheStub) UpdateClaudeCodeHeaders(_ context.Context, _ int64, _ http.Header) error {
	return nil
}

func TestIdentityService_RewriteUserID_PreservesTopLevelFieldOrder(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)

	originalUserID := `{"device_id":"d61f76d0730d2b920763648949bad5c79742155c27037fc77ac3f9805cb90169","account_uuid":"","session_id":"7578cf37-aaca-46e4-a45c-71285d9dbb83","parent_session_id":"parent-uuid","tk":"opaque-token"}`
	body := []byte(`{"alpha":1,"messages":[],"metadata":{"user_id":` + strconvQuote(originalUserID) + `},"max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"stream":true}`)

	result, err := svc.RewriteUserID(body, 123, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	resultStr := string(result)

	userID := gjson.Get(resultStr, "metadata.user_id").String()
	parsed := ParseMetadataUserID(userID)
	require.NotNil(t, parsed)
	parentBody := metadataSessionBody("parent-uuid", "")
	parentResult, err := svc.RewriteUserID(parentBody, 123, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	parent := ParseMetadataUserID(gjson.GetBytes(parentResult, "metadata.user_id").String())
	require.Equal(t, parent.SessionID, gjson.Parse(string(parsed.ExtraFields["parent_session_id"])).String())
	require.Equal(t, "opaque-token", gjson.Parse(string(parsed.ExtraFields["tk"])).String())
	require.Equal(t, "client-xyz", parsed.DeviceID)
	require.Equal(t, "acc-uuid", parsed.AccountUUID)
	require.NotEqual(t, originalUserID, userID)
}

func TestIdentityService_RewriteUserIDWithMasking_PreservesTopLevelFieldOrder(t *testing.T) {
	cache := &identityCacheStub{seeds: map[int64]string{123: "11111111-2222-4333-8444-555555555555"}}
	svc := NewIdentityService(cache)

	originalUserID := `{"device_id":"d61f76d0730d2b920763648949bad5c79742155c27037fc77ac3f9805cb90169","account_uuid":"","session_id":"7578cf37-aaca-46e4-a45c-71285d9dbb83","parent_session_id":"parent-uuid","tk":"opaque-token"}`
	body := []byte(`{"alpha":1,"messages":[],"metadata":{"user_id":` + strconvQuote(originalUserID) + `},"max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"stream":true}`)

	account := &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"session_id_masking_enabled": true,
		},
	}

	result, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	resultStr := string(result)

	userID := gjson.Get(resultStr, "metadata.user_id").String()
	parsed := ParseMetadataUserID(userID)
	require.NotNil(t, parsed)
	parentResult, err := svc.RewriteUserIDWithMasking(context.Background(), metadataSessionBody("parent-uuid", ""), account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	parent := ParseMetadataUserID(gjson.GetBytes(parentResult, "metadata.user_id").String())
	require.Equal(t, parent.SessionID, gjson.Parse(string(parsed.ExtraFields["parent_session_id"])).String())
	require.Equal(t, "opaque-token", gjson.Parse(string(parsed.ExtraFields["tk"])).String())
	require.Equal(t, "client-xyz", parsed.DeviceID)
	require.Equal(t, "acc-uuid", parsed.AccountUUID)
	require.True(t, strings.Contains(resultStr, `"metadata":{"user_id":"`))
}

func strconvQuote(v string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`) + `"`
}

func metadataSessionBody(session, parent string) []byte {
	userID := `{"device_id":"original-device","account_uuid":"","session_id":` + strconvQuote(session) + `,"parent_session_id":` + strconvQuote(parent) + `,"tk":"opaque-token"}`
	return []byte(`{"messages":[],"metadata":{"user_id":` + strconvQuote(userID) + `},"thinking":{"type":"adaptive"}}`)
}

func TestIdentityServiceMaskedSessionsKeepRelationships(t *testing.T) {
	cache := &identityCacheStub{}
	svc := NewIdentityService(cache)
	ctx := context.Background()
	account := &Account{ID: 123, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{"session_id_masking_enabled": true}}
	rewrite := func(session, parent string) *ParsedUserID {
		body, err := svc.RewriteUserIDWithMasking(ctx, metadataSessionBody(session, parent), account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)
		parsed := ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String())
		require.NotNil(t, parsed)
		require.Equal(t, "opaque-token", gjson.Parse(string(parsed.ExtraFields["tk"])).String())
		require.Equal(t, "adaptive", gjson.GetBytes(body, "thinking.type").String())
		return parsed
	}
	parent := rewrite("parent-session", "")
	child := rewrite("child-session", "parent-session")
	independent := rewrite("independent-session", "")
	require.Equal(t, parent.SessionID, gjson.Parse(string(child.ExtraFields["parent_session_id"])).String())
	require.NotEqual(t, parent.SessionID, child.SessionID)
	require.NotEqual(t, child.SessionID, independent.SessionID)
	require.NotEqual(t, parent.SessionID, independent.SessionID)
	require.Equal(t, child, rewrite("child-session", "parent-session"))
	account.ID = 456
	otherAccount := rewrite("child-session", "parent-session")
	require.NotEqual(t, child.SessionID, otherAccount.SessionID)
	require.NotEqual(t, string(child.ExtraFields["parent_session_id"]), string(otherAccount.ExtraFields["parent_session_id"]))
	// Even an equal stored seed must stay isolated by account.
	cache.seeds[456] = cache.seeds[123]
	require.NotEqual(t, child.SessionID, rewrite("child-session", "parent-session").SessionID)
	account.ID = 123
	cache.seeds[123] = "rotated-namespace"
	rotatedParent := rewrite("parent-session", "")
	rotatedChild := rewrite("child-session", "parent-session")
	require.NotEqual(t, parent.SessionID, rotatedParent.SessionID)
	require.NotEqual(t, child.SessionID, rotatedChild.SessionID)
	require.Equal(t, rotatedParent.SessionID, gjson.Parse(string(rotatedChild.ExtraFields["parent_session_id"])).String())
	require.Equal(t, rotatedChild, rewrite("child-session", "parent-session"))
}

func TestIdentityServiceMaskCacheFailureKeepsAccountRemapping(t *testing.T) {
	ctx := context.Background()
	for _, cache := range []*identityCacheStub{
		{getErr: errors.New("cache unavailable")},
		{createErr: errors.New("cannot persist namespace")},
		{seeds: map[int64]string{123: "cached-seed"}, createErr: errors.New("cannot refresh namespace")},
	} {
		svc := NewIdentityService(cache)
		account := &Account{ID: 123, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{"session_id_masking_enabled": true}}
		body := metadataSessionBody("child-session", "parent-session")
		normal, err := svc.RewriteUserID(body, 123, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)
		masked, err := svc.RewriteUserIDWithMasking(ctx, body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)
		require.Equal(t, string(normal), string(masked))
	}
}

func TestIdentityServiceMaskingOffPreservesLegacyMapping(t *testing.T) {
	svc := NewIdentityService(&identityCacheStub{getErr: errors.New("must not require mask cache")})
	account := &Account{ID: 123}
	deviceID := strings.Repeat("a", 64)
	accountUUID := "123e4567-e89b-12d3-a456-426614174000"
	body := []byte(`{"metadata":{"user_id":"user_` + strings.Repeat("b", 64) + `_account__session_123e4567-e89b-12d3-a456-426614174111"}}`)
	normal, err := svc.RewriteUserID(body, account.ID, accountUUID, deviceID, "claude-cli/2.1.19 (external, cli)")
	require.NoError(t, err)
	masked, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, accountUUID, deviceID, "claude-cli/2.1.19 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, string(normal), string(masked))
	parsed := ParseMetadataUserID(gjson.GetBytes(masked, "metadata.user_id").String())
	require.NotNil(t, parsed)
	require.Equal(t, deviceID, parsed.DeviceID)
	require.Equal(t, accountUUID, parsed.AccountUUID)
	require.False(t, parsed.IsNewFormat)
}

func TestIdentityServiceUsesAuthoritativeMaskNamespaceAfterReadRace(t *testing.T) {
	ctx := context.Background()
	account := &Account{ID: 123, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{"session_id_masking_enabled": true}}
	cache := &identityCacheStub{seeds: map[int64]string{123: "winning-seed"}, readSeed: "stale-read-seed"}
	svc := NewIdentityService(cache)
	body := metadataSessionBody("child-session", "parent-session")
	raced, err := svc.RewriteUserIDWithMasking(ctx, body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	cache.readSeed = ""
	authoritative, err := svc.RewriteUserIDWithMasking(ctx, body, account, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, string(authoritative), string(raced))
}
