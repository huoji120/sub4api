package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ccVersionInBillingRe matches the semver part of cc_version (X.Y.Z).
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+`)

var ccVersionWithFingerprintInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+\.[0-9a-fA-F]{3}\b`)

const claudeBillingSourceTextKey = "claude_billing_source_text"

// Capture before proxy system migration. Never infer provenance from text prefixes.
func captureClaudeBillingSource(c *gin.Context, body []byte) {
	if c != nil {
		c.Set(claudeBillingSourceTextKey, extractFirstUserText(body))
	}
}

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent.
// Recompute any recognized fingerprint suffix because its input includes the version.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".

// effectiveRequestBillingUserAgent selects the version source for billing
// synchronization. Real Claude Code requests use the OAuth account fingerprint
// because the upstream account, not the client-side subscription, owns the
// outbound billing attribution. Mimicry uses the proxy compatibility UA.
func effectiveRequestBillingUserAgent(mimicUserAgent, clientUserAgent, tokenType string, mimicClaudeCode bool, fingerprint *Fingerprint) string {
	if tokenType != "oauth" {
		return ""
	}
	if mimicClaudeCode {
		return mimicUserAgent
	}
	if fingerprint != nil && ExtractCLIVersion(fingerprint.UserAgent) != "" {
		return fingerprint.UserAgent
	}
	if ExtractCLIVersion(clientUserAgent) != "" {
		return clientUserAgent
	}
	return ""
}
func syncBillingHeaderVersion(body []byte, userAgent string, c *gin.Context) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	firstText := extractFirstUserText(body)
	if c != nil {
		if saved, ok := c.Get(claudeBillingSourceTextKey); ok {
			if savedText, ok := saved.(string); ok {
				firstText = savedText
			}
		}
	}
	fingerprint := computeClaudeCodeFingerprintFromText(firstText, version)
	replacement := "cc_version=" + version
	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			fingerprintedReplacement := replacement + "." + fingerprint
			newText := ccVersionWithFingerprintInBillingRe.ReplaceAllString(text.String(), fingerprintedReplacement)
			newText = ccVersionInBillingRe.ReplaceAllString(newText, replacement)
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})

	return body
}
