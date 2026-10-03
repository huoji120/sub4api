import { describe, expect, it } from "vitest";

import {
  appendAuthSourceDefaultsToUpdateRequest,
  buildAuthSourceDefaultsState,
  normalizePlatformQuotasMap,
  sanitizePlatformQuotasMap,
  type UpdateSettingsRequest,
  type DefaultPlatformQuotasMap,
} from "@/api/admin/settings";

describe("admin settings auth source defaults helpers", () => {
  it("defaults grant-on-signup to disabled when settings are missing", () => {
    const state = buildAuthSourceDefaultsState({});

    expect(state.email.grant_on_signup).toBe(false);
    expect(state.linuxdo.grant_on_signup).toBe(false);
    expect(state.oidc.grant_on_signup).toBe(false);
    expect(state.wechat.grant_on_signup).toBe(false);
  });

  it("reads nested platform_quotas from settings into auth source state", () => {
    const state = buildAuthSourceDefaultsState({
      auth_source_default_email_platform_quotas: {
        anthropic: { daily: 10, weekly: 50, monthly: 200 },
        openai:    { daily: null, weekly: null, monthly: null },
      } as DefaultPlatformQuotasMap,
    });

    // anthropic 填写的值应被保留
    expect(state.email.platform_quotas.anthropic).toEqual({ daily: 10, weekly: 50, monthly: 200 });
    // openai 全 null 应被保留
    expect(state.email.platform_quotas.openai).toEqual({ daily: null, weekly: null, monthly: null });
    // 未出现的平台（gemini/antigravity）归一化为 null
    expect(state.email.platform_quotas.gemini).toEqual({ daily: null, weekly: null, monthly: null });
    expect(state.email.platform_quotas.antigravity).toEqual({ daily: null, weekly: null, monthly: null });
  });

  it("appends sanitized nested platform_quotas with non-null values in update payload", () => {
    const payload: UpdateSettingsRequest = {};
    appendAuthSourceDefaultsToUpdateRequest(payload, {
      email: {
        balance: 0,
        concurrency: 5,
        subscriptions: [],
        grant_on_signup: false,
        grant_on_first_bind: false,
        platform_quotas: {
          anthropic: { daily: 10, weekly: 50, monthly: 200 },
          openai:    { daily: 0, weekly: null, monthly: null },
        },
      },
      linuxdo: { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
      oidc:    { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
      wechat:  { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
      github:  { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
      google:  { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
      dingtalk: { balance: 0, concurrency: 5, subscriptions: [], grant_on_signup: false, grant_on_first_bind: false, platform_quotas: {} },
    });

    const emailQuotas = (payload as Record<string, unknown>)["auth_source_default_email_platform_quotas"] as DefaultPlatformQuotasMap;
    expect(emailQuotas.anthropic).toEqual({ daily: 10, weekly: 50, monthly: 200 });
    // 0 是显式禁用，与 null（不设限额）不同，保留。
    expect(emailQuotas.openai?.daily).toBe(0);
    // 缺失平台归一化为全 null
    expect(emailQuotas.gemini).toEqual({ daily: null, weekly: null, monthly: null });
    expect(emailQuotas.antigravity).toEqual({ daily: null, weekly: null, monthly: null });
  });
});

describe("normalizePlatformQuotasMap", () => {
  it("preserves configured limits while filling absent providers and windows with null", () => {
    const result = normalizePlatformQuotasMap({
      kimi: { daily: 5, weekly: 0, monthly: null },
      typesafe: { daily: 0 } as DefaultPlatformQuotasMap["typesafe"],
    });
    expect(result.kimi).toEqual({ daily: 5, weekly: 0, monthly: null });
    expect(result.typesafe).toEqual({ daily: 0, weekly: null, monthly: null });
    expect(result.opencode_go).toEqual({ daily: null, weekly: null, monthly: null });
  });

  it("非 number 类型的值归一化为 null", () => {
    const result = normalizePlatformQuotasMap({
      anthropic: { daily: "50" as unknown as number, weekly: undefined as unknown as number, monthly: null },
    });
    expect(result.anthropic).toEqual({ daily: null, weekly: null, monthly: null });
  });
});

describe("sanitizePlatformQuotasMap", () => {
  it("保留合法的正数和零值", () => {
    const result = sanitizePlatformQuotasMap({
      anthropic: { daily: 10.5, weekly: 0, monthly: null },
    });
    expect(result.anthropic?.daily).toBe(10.5);
    expect(result.anthropic?.weekly).toBe(0);
    expect(result.anthropic?.monthly).toBe(null);
  });

  it("空字符串（v-model.number 空输入）清洗为 null", () => {
    const result = sanitizePlatformQuotasMap({
      anthropic: { daily: "" as unknown as number, weekly: null, monthly: null },
    });
    expect(result.anthropic?.daily).toBe(null);
  });

  it("负数清洗为 null", () => {
    const result = sanitizePlatformQuotasMap({
      openai: { daily: -1, weekly: null, monthly: null },
    });
    expect(result.openai?.daily).toBe(null);
  });

  it("NaN/Infinity 清洗为 null", () => {
    const result = sanitizePlatformQuotasMap({
      gemini: { daily: NaN, weekly: Infinity, monthly: null },
    });
    expect(result.gemini?.daily).toBe(null);
    expect(result.gemini?.weekly).toBe(null);
  });

});
