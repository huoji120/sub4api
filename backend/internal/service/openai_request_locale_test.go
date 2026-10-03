package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func localeTestBody(texts []string, kinds []string) []byte {
	content := make([]any, 0, len(texts))
	for _, text := range texts {
		content = append(content, map[string]any{"type": "input_text", "text": text})
	}
	body, _ := json.Marshal(map[string]any{
		"input": []any{map[string]any{
			"role": "user", "content": content,
			"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": kinds},
		}},
		"tools": []any{map[string]any{"type": "web_search", "user_location": map[string]any{
			"country": "US", "city": "New York", "timezone": "America/New_York",
		}}},
	})
	return body
}

func TestOpenAIRequestTimezoneCatalogAndValidation(t *testing.T) {
	options := OpenAIRequestTimezoneOptions()
	require.Greater(t, len(options), 30)
	require.True(t, sort.StringsAreSorted(options))
	require.Equal(t, "Asia/Singapore", DefaultOpenAIRequestTimezone)
	require.Contains(t, options, DefaultOpenAIRequestTimezone)
	seen := make(map[string]struct{}, len(options))
	for _, name := range options {
		require.NotContains(t, seen, name, "duplicate timezone")
		seen[name] = struct{}{}
		_, err := time.LoadLocation(name)
		require.NoError(t, err, "invalid IANA timezone: %s", name)
	}
	for _, allowed := range []string{"Asia/Shanghai", "Asia/Urumqi", "Asia/Hong_Kong", "Asia/Macau", "Asia/Taipei", "Asia/Chongqing", "Europe/Oslo", "Africa/Accra", "Asia/Kathmandu", "Asia/Tokyo", "America/New_York"} {
		require.Contains(t, options, allowed)
		require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{openAIRequestTimezoneExtraKey: allowed}))
		require.Equal(t, allowed, (&Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIRequestTimezoneExtraKey: allowed}}).OpenAIRequestTimezone())
	}
	for _, invalid := range []string{"Invalid/Timezone", " Asia/Tokyo", "Asia/Tokyo "} {
		require.NotContains(t, options, invalid)
		require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{openAIRequestTimezoneExtraKey: invalid}))
		require.Equal(t, DefaultOpenAIRequestTimezone, (&Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIRequestTimezoneExtraKey: invalid}}).OpenAIRequestTimezone())
	}
	require.NoError(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{openAIRequestTimezoneExtraKey: ""}))
	require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformOpenAI, map[string]any{openAIRequestTimezoneExtraKey: 8}))
	require.Error(t, ValidateOpenAIRequestTimezoneExtra(PlatformAnthropic, map[string]any{openAIRequestTimezoneExtraKey: "Asia/Singapore"}))
	require.Equal(t, DefaultOpenAIRequestTimezone, (&Account{Platform: PlatformOpenAI}).OpenAIRequestTimezone())
	require.Equal(t, DefaultOpenAIRequestTimezone, (&Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIRequestTimezoneExtraKey: ""}}).OpenAIRequestTimezone())
}

func TestRewriteOpenAIRequestEnvironmentOnlyTimezone(t *testing.T) {
	for name, text := range map[string]string{
		"historical date":           "<environment_context><current_date>2001-01-01</current_date><timezone>Asia/Shanghai</timezone></environment_context>",
		"unavailable date":          `<environment_context><current_date status="unavailable" /><timezone>Asia/Shanghai</timezone></environment_context>`,
		"closing tag whitespace":    "<environment_context><current_date>2001-01-01</current_date \t><timezone>Asia/Shanghai</timezone \r\n></environment_context  >",
		"timezone only delta":       "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>",
		"opaque unescaped fields":   "<environment_context>\n<subagents>agent & colleague <unnamed</subagents>\n<network enabled=\"true\"><allowed>a&b.example,<example</allowed></network>\n<timezone>Asia/Shanghai</timezone>\n<cwd>/workspace/a&b<c</cwd>\n</environment_context>",
		"attributes and whitespace": "\n <environment_context version=\"test\">\r\n  <timezone source='client > clock'>\n \tAsia/Shanghai\u00a0\n</timezone>\n</environment_context> \t",
		"opaque nested fields":      "<environment_context><filesystem><workspace_roots><root>/workspace</root></workspace_roots></filesystem><timezone>Asia/Shanghai</timezone></environment_context>",
	} {
		t.Run(name, func(t *testing.T) {
			out, previous, reason := rewriteOpenAIRequestEnvironment(text, "America/New_York")
			require.Equal(t, "replaced", reason)
			require.Equal(t, "Asia/Shanghai", previous)
			require.Equal(t, strings.Replace(text, "Asia/Shanghai", "America/New_York", 1), out)
			again, _, reason := rewriteOpenAIRequestEnvironment(out, "America/New_York")
			require.Equal(t, "already_target", reason)
			require.Equal(t, out, again)
		})
	}
}

func TestRewriteOpenAIRequestEnvironmentAmbiguousOrQuoted(t *testing.T) {
	for name, text := range map[string]string{
		"ordinary prose":                    "Timezone: Asia/Shanghai, date: 2001-01-01",
		"inline example":                    "Example: <environment_context><timezone>Asia/Shanghai</timezone></environment_context>",
		"code fence":                        "```xml\n<environment_context><timezone>Asia/Shanghai</timezone></environment_context>\n```",
		"quoted block":                      "> <environment_context><timezone>Asia/Shanghai</timezone></environment_context>",
		"memory history prose":              "Earlier environment:\n<environment_context><timezone>Asia/Shanghai</timezone></environment_context>",
		"missing timezone":                  "<environment_context><current_date>2001-01-01</current_date></environment_context>",
		"empty timezone":                    "<environment_context><timezone> \n </timezone></environment_context>",
		"self closing timezone":             "<environment_context><timezone /></environment_context>",
		"duplicate timezone":                "<environment_context><timezone>Asia/Shanghai</timezone><timezone>Asia/Tokyo</timezone></environment_context>",
		"timezone followed by self closing": "<environment_context><timezone>Asia/Shanghai</timezone><timezone /></environment_context>",
		"timezone with nested tag":          "<environment_context><timezone><value>Asia/Shanghai</value></timezone></environment_context>",
		"nested timezone":                   "<environment_context><example><timezone>Asia/Shanghai</timezone></example></environment_context>",
		"incomplete timezone":               "<environment_context><timezone>Asia/Shanghai</environment_context>",
		"missing root close":                "<environment_context><timezone>Asia/Shanghai</timezone>",
		"self closing root":                 "<environment_context/>",
		"nested root":                       "<environment_context><environment_context><timezone>Asia/Shanghai</timezone></environment_context></environment_context>",
		"two roots":                         "<environment_context><timezone>Asia/Shanghai</timezone></environment_context><environment_context><timezone>Asia/Tokyo</timezone></environment_context>",
		"unclear field boundary":            "<environment_context><timezone>Asia/Shanghai</timezone> some prose </environment_context>",
		"wrong opaque field close":          "<environment_context><timezone>Asia/Shanghai</timezone><network>domain&name</wrong></environment_context>",
	} {
		t.Run(name, func(t *testing.T) {
			out, _, reason := rewriteOpenAIRequestEnvironment(text, "America/New_York")
			require.NotEqual(t, "replaced", reason)
			require.Equal(t, text, out)
		})
	}
}

func TestNormalizeOpenAIRequestLocaleAndDebugLog(t *testing.T) {
	first := "<environment_context>\n  <current_date>2026-09-22</current_date>\n  <timezone>Asia/Shanghai</timezone>\n  <cwd>/app</cwd>\n</environment_context>"
	currentDate := time.Now().In(mustLoadLocation(t, "Asia/Singapore")).Format(time.DateOnly)
	secondInput := "<environment_context><timezone>Asia/Singapore</timezone></environment_context>"
	secondExpected := "<environment_context><timezone>Asia/Singapore</timezone>\n  <current_date>" + currentDate + "</current_date>\n</environment_context>"
	body := localeTestBody([]string{first, secondInput, first}, []string{"environments.environment_context", "environments.environment_context", "user.text"})
	core, observed := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	out := normalizeOpenAIRequestLocale(ctx, &Account{ID: 42, Platform: PlatformOpenAI}, body, "http")
	for _, path := range []string{"input.0.content.0.text", "input.0.content.2.text"} {
		require.Equal(t, strings.Replace(first, "Asia/Shanghai", "Asia/Singapore", 1), gjson.GetBytes(out, path).String())
	}
	require.Equal(t, secondExpected, gjson.GetBytes(out, "input.0.content.1.text").String())
	require.Equal(t, gjson.GetBytes(body, "input.0.internal_chat_message_metadata_passthrough").Raw, gjson.GetBytes(out, "input.0.internal_chat_message_metadata_passthrough").Raw)
	require.Equal(t, "US", gjson.GetBytes(out, "tools.0.user_location.country").String())
	require.Equal(t, "New York", gjson.GetBytes(out, "tools.0.user_location.city").String())
	require.Equal(t, "Asia/Singapore", gjson.GetBytes(out, "tools.0.user_location.timezone").String())
	require.Len(t, observed.All(), 1)
	fields, err := json.Marshal(observed.All()[0].ContextMap())
	require.NoError(t, err)
	require.Contains(t, string(fields), `"timezone_replaced":true`)
	require.Contains(t, string(fields), `"environment_fields_inserted":true`)
	require.Contains(t, string(fields), `"matched_count":3`)
	require.Contains(t, string(fields), `"replaced_count":3`)
	require.Contains(t, string(fields), `"inserted_count":1`)
	require.Contains(t, string(fields), `"web_search_timezone_before":["America/New_York"]`)
	require.Contains(t, string(fields), `"timezone_before":["Asia/Shanghai","Asia/Singapore","Asia/Shanghai"]`)
	require.NotContains(t, string(fields), "/app")
	require.NotContains(t, string(fields), "2026-09-22")
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	require.NoError(t, err)
	return location
}

func TestNormalizeOpenAIRequestLocaleNoReplacementStillLogs(t *testing.T) {
	cases := []struct {
		name, text, reason string
		changed            bool
	}{
		{"already target", "<environment_context><timezone>Asia/Singapore</timezone></environment_context>", "inserted", true},
		{"no timezone", "<environment_context><current_date>old</current_date></environment_context>", "inserted", true},
		{"nested value", "<environment_context><timezone><nested/></timezone></environment_context>", "invalid_environment_context", false},
		{"self closing", "<environment_context><timezone/></environment_context>", "invalid_environment_context", false},
		{"ordinary text", "Today in Asia/Shanghai", "no_environment_context", false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			core, observed := observer.New(zap.DebugLevel)
			body := localeTestBody([]string{item.text}, nil)
			body, err := sjson.DeleteBytes(body, "tools")
			require.NoError(t, err)
			out := normalizeOpenAIRequestLocale(logger.IntoContext(context.Background(), zap.New(core)), &Account{Platform: PlatformOpenAI}, body, "ws")
			require.Len(t, observed.All(), 1)
			require.Equal(t, item.reason, observed.All()[0].ContextMap()["reason"])
			require.Equal(t, false, observed.All()[0].ContextMap()["timezone_replaced"])
			if item.changed {
				text := gjson.GetBytes(out, "input.0.content.0.text").String()
				require.Contains(t, text, "<timezone>Asia/Singapore</timezone>")
				require.Contains(t, text, "<current_date>")
			} else {
				require.Equal(t, body, out)
			}
		})
	}
	core, observed := observer.New(zap.InfoLevel)
	normalizeOpenAIRequestLocale(logger.IntoContext(context.Background(), zap.New(core)), &Account{Platform: PlatformOpenAI}, localeTestBody(nil, nil), "http")
	require.Empty(t, observed.All())
}

func TestNormalizeOpenAIRequestLocaleWithoutKindPreservesHistory(t *testing.T) {
	env := "<environment_context><current_date>2001-01-01</current_date><timezone>Asia/Shanghai</timezone></environment_context>"
	for name, kinds := range map[string]any{"absent": nil, "empty": []string{}, "unmarked": []string{"user.text"}, "incomplete": []string{"environments.environment_context"}, "invalid type": "user.text"} {
		t.Run(name, func(t *testing.T) {
			body := localeTestBody([]string{env, strings.Replace(env, "2001-01-01", "2002-06-30", 1)}, nil)
			body, _ = sjson.DeleteBytes(body, "tools")
			if kinds == nil {
				body, _ = sjson.DeleteBytes(body, "input.0.internal_chat_message_metadata_passthrough")
			} else {
				body, _ = sjson.SetBytes(body, "input.0.internal_chat_message_metadata_passthrough.content_item_kinds", kinds)
			}
			body, _ = sjson.SetBytes(body, "client_metadata.turn_started_at_unix_ms", 1772938800000)
			account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAIRequestTimezoneExtraKey: "America/New_York"}}
			out := normalizeOpenAIRequestLocale(context.Background(), account, body, "http")
			for i, date := range []string{"2001-01-01", "2002-06-30"} {
				text := gjson.GetBytes(out, "input.0.content").Array()[i].Get("text").String()
				require.Contains(t, text, "<current_date>"+date+"</current_date>")
				require.Contains(t, text, "<timezone>America/New_York</timezone>")
			}
			require.Equal(t, gjson.GetBytes(body, "client_metadata").Raw, gjson.GetBytes(out, "client_metadata").Raw)
			require.Equal(t, gjson.GetBytes(body, "input.0.internal_chat_message_metadata_passthrough").Raw, gjson.GetBytes(out, "input.0.internal_chat_message_metadata_passthrough").Raw)
			// There is no clock input: replaying a historical prefix through either
			// transport, including on a later day, produces exactly the same bytes.
			require.Equal(t, out, normalizeOpenAIRequestLocale(context.Background(), account, body, "ws"))
			require.Equal(t, out, normalizeOpenAIRequestLocale(context.Background(), account, out, "http"))
		})
	}
}

func TestNormalizeOpenAIRequestLocaleOnlyUserText(t *testing.T) {
	env := "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"
	body, err := json.Marshal(map[string]any{"input": []any{
		map[string]any{"role": "user", "content": env},
		map[string]any{"role": "developer", "content": env},
		map[string]any{"role": "assistant", "content": env},
		map[string]any{"role": "tool", "content": env},
		map[string]any{"type": "function_call_output", "output": env},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "output_text", "text": env}, map[string]any{"type": "input_text", "text": 42}}},
	}})
	require.NoError(t, err)
	out := normalizeOpenAIRequestLocale(context.Background(), &Account{Platform: PlatformOpenAI}, body, "http")
	currentDate := time.Now().In(mustLoadLocation(t, "Asia/Singapore")).Format(time.DateOnly)
	expected := strings.Replace(env, "Asia/Shanghai", "Asia/Singapore", 1)
	expected = strings.Replace(expected, "</environment_context>", "\n  <current_date>"+currentDate+"</current_date>\n</environment_context>", 1)
	require.Equal(t, expected, gjson.GetBytes(out, "input.0.content").String())
	original, rewritten := gjson.GetBytes(body, "input").Array(), gjson.GetBytes(out, "input").Array()
	for i := 1; i < len(original); i++ {
		require.Equal(t, original[i].Raw, rewritten[i].Raw)
	}
	for _, account := range []*Account{nil, {Platform: PlatformAnthropic}} {
		require.Equal(t, body, normalizeOpenAIRequestLocale(context.Background(), account, body, "http"))
	}
}

func TestForwardOpenAIRequestLocaleUsesFormalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, language := range []string{"", "zh-CN,zh;q=0.9"} {
			recorder := httptest.NewRecorder()
			client, _ := gin.CreateTestContext(recorder)
			environment := `<environment_context><current_date status="unavailable" /><timezone>Asia/Shanghai</timezone><network><allowed>a&b.example</allowed></network></environment_context>`
			body := localeTestBody([]string{environment, environment}, []string{"environments.environment_context", "user.text"})
			body, _ = sjson.SetBytes(body, "model", "gpt-5.4")
			body, _ = sjson.SetBytes(body, "stream", false)
			body, _ = sjson.SetBytes(body, "instructions", "test")
			body, _ = sjson.SetBytes(body, "input.0.type", "message")
			body, _ = sjson.DeleteBytes(body, "tools")
			client.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			client.Request.Header.Set("Content-Type", "application/json")
			if language != "" {
				client.Request.Header.Set("Accept-Language", language)
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"resp_locale","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			if passthrough {
				upstream.resp.Header.Set("Content-Type", "text/event-stream")
				upstream.resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_locale\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
			}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Status: StatusActive, Schedulable: true,
				Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"}, Extra: map[string]any{"openai_passthrough": passthrough},
			}
			result, err := svc.Forward(context.Background(), client, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, language, upstream.lastReq.Header.Get("Accept-Language"))
			for _, part := range gjson.GetBytes(upstream.lastBody, "input.0.content").Array() {
				require.Equal(t, strings.Replace(environment, "Asia/Shanghai", "Asia/Singapore", 1), part.Get("text").String())
			}
		}
	}
}
