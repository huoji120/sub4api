package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// fingerprintSalt matches the supplied Claude Code 2.1.283 Vbe implementation.
const fingerprintSalt = "59cf53e54c78"

// computeClaudeCodeFingerprint 复刻真实 Claude Code CLI 的 cc_version 指纹算法。
// 官方 JavaScript 以 UTF-16 code unit 访问 text[4], text[7], text[20]；Go
// 字符串下标是 UTF-8 byte，因此不能直接用 firstText[i]。
func computeClaudeCodeFingerprint(body []byte, version string) string {
	return computeClaudeCodeFingerprintFromText(extractFirstUserText(body), version)
}

func computeClaudeCodeFingerprintFromText(firstText, version string) string {
	var units [3]uint16
	for n, index := range [...]int{4, 7, 20} {
		unit, ok := javascriptUTF16CodeUnitAt(firstText, index)
		if !ok {
			unit = '0'
		}
		units[n] = unit
	}
	input := make([]byte, 0, len(fingerprintSalt)+len(version)+12)
	input = append(input, fingerprintSalt...)
	// JS joins sampled code units before UTF-8 encoding: adjacent sampled
	// surrogate halves can form a pair even if separated in the source text.
	for i := 0; i < len(units); i++ {
		r := rune(units[i])
		if r >= 0xD800 && r <= 0xDBFF && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF {
			r = utf16.DecodeRune(r, rune(units[i+1]))
			i++
		} else if utf16.IsSurrogate(r) {
			r = utf8.RuneError
		}
		input = utf8.AppendRune(input, r)
	}
	input = append(input, version...)
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])[:3]
}

func javascriptUTF16CodeUnitAt(text string, index int) (uint16, bool) {
	if index < 0 {
		return 0, false
	}
	position := 0
	for _, r := range text {
		if r <= 0xFFFF {
			if position == index {
				return uint16(r), true
			}
			position++
			continue
		}
		value := r - 0x10000
		high := uint16(0xD800 + (value >> 10))
		low := uint16(0xDC00 + (value & 0x3FF))
		if position == index {
			return high, true
		}
		position++
		if position == index {
			return low, true
		}
		position++
	}
	return 0, false
}

// extractFirstUserText 提取 messages 中第一条非 meta user 消息的首段 text 内容。
// 兼容 string 和 []block 两种 content 格式；isMeta/is_meta 消息与官方 CLI 一样跳过。
func extractFirstUserText(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return ""
	}
	first := ""
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" || msg.Get("isMeta").Bool() || msg.Get("is_meta").Bool() {
			return true
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			first = content.String()
			return false
		}
		if content.IsArray() {
			content.ForEach(func(_, block gjson.Result) bool {
				if block.Get("type").String() == "text" {
					first = block.Get("text").String()
					return false
				}
				return true
			})
			return false
		}
		return false
	})
	return first
}

// buildBillingAttributionText 构造 system 数组的 billing attribution 文本。
//
// Claude Code 2.1.283 的 first-party/Vertex 请求包含条件性的 cch=00000
// 字段；OAuth mimic 是向 Anthropic first-party 发送的兼容路径，因此保留该
// 字段以避免生成缺失字段的非对称 billing block。这里不生成遥测或设备标识。
func buildBillingAttributionText(body []byte, cliVersion string) (string, error) {
	if cliVersion == "" {
		return "", fmt.Errorf("cliVersion required")
	}
	fp := computeClaudeCodeFingerprint(body, cliVersion)
	return fmt.Sprintf(
		"x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=00000;",
		cliVersion, fp,
	), nil
}
