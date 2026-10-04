package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
)

// Only control/context fields actually supplied by an upstream response may
// become defaults for later OAuth mimic requests. Identity and auth stay separate.
var claudeCodeControlHeaderNames = [...]string{
	"anthropic-usage-limit",
	"x-claude-code-prev-tool-durations",
	"x-claude-code-context-compacted",
	"x-cc-context-compacted",
	"x-claude-code-compaction",
	"x-cc-compaction-request",
}

func filterClaudeCodeControlHeaders(headers http.Header) http.Header {
	var filtered http.Header
	for key, values := range headers {
		for _, name := range claudeCodeControlHeaderNames {
			if !strings.EqualFold(key, name) {
				continue
			}
			if filtered == nil {
				filtered = make(http.Header)
			}
			if previous, exists := filtered[name]; exists {
				filtered[name] = append(previous, values...)
			} else if values == nil {
				filtered[name] = nil
			} else {
				filtered[name] = append(make([]string, 0, len(values)), values...)
			}
			break
		}
	}
	return filtered
}

func (s *GatewayService) applyClaudeCodeControlHeaders(ctx context.Context, req *http.Request, incoming http.Header, account *Account, tokenType string, mimic bool) {
	caller := filterClaudeCodeControlHeaders(incoming)
	// Copy explicit caller fields, including present-but-empty value arrays.
	// Presence, not Header.Get's nonempty result, decides precedence.
	for name, values := range caller {
		deleteHeaderAllForms(req.Header, name)
		req.Header[resolveWireCasing(name)] = values
	}
	if !mimic || tokenType != "oauth" || account == nil || !account.IsAnthropicOAuthOrSetupToken() || s.identityService == nil || s.identityService.cache == nil {
		return
	}
	cached, err := s.identityService.cache.GetClaudeCodeHeaders(ctx, account.ID)
	if err != nil {
		logger.LegacyPrintf("service.gateway", "Warning: failed to read Claude Code headers for account %d: %v", account.ID, err)
		return
	}
	for name, values := range filterClaudeCodeControlHeaders(cached) {
		if _, supplied := caller[name]; supplied {
			continue
		}
		deleteHeaderAllForms(req.Header, name)
		req.Header[resolveWireCasing(name)] = values
	}
}

// Observe every response before returning it, so error/retry responses update
// the account's defaults before a retry is built. Cache failure never changes
// the upstream response, body, status or transport error.
func (s *GatewayService) doClaudeUpstreamRequest(ctx context.Context, req *http.Request, proxyURL string, account *Account, profile *tlsfingerprint.Profile) (*http.Response, error) {
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, profile)
	if resp != nil && account.IsAnthropicOAuthOrSetupToken() && s.identityService != nil && s.identityService.cache != nil {
		observed := filterClaudeCodeControlHeaders(resp.Header)
		if len(observed) > 0 {
			if cacheErr := s.identityService.cache.UpdateClaudeCodeHeaders(ctx, account.ID, observed); cacheErr != nil {
				logger.LegacyPrintf("service.gateway", "Warning: failed to update Claude Code headers for account %d: %v", account.ID, cacheErr)
			}
		}
	}
	return resp, err
}
