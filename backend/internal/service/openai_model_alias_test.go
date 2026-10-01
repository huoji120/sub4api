package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownOpenAICodexModelGPT6Astra(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "openai/gpt-6-astra", "OPENAI/GPT-6_ASTRA", "gpt-6", "openai/gpt-6"} {
		require.Equal(t, "gpt-6-astra", normalizeKnownOpenAICodexModel(model))
	}
}

func TestNormalizeKnownOpenAICodexModel_BareGPT56RoutesToSol(t *testing.T) {
	tests := map[string]string{
		"gpt-5.6":            "gpt-5.6-sol",
		"openai/gpt-5.6":     "gpt-5.6-sol",
		"gpt5.6":             "gpt-5.6-sol",
		"gpt-5.6-high":       "gpt-5.6-sol",
		"gpt-5.6-max":        "gpt-5.6-sol",
		"gpt-5.6-2026-07-09": "gpt-5.6-sol",
		"openai/gpt-5.6-max": "gpt-5.6-sol",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, expected, normalizeKnownOpenAICodexModel(input))
		})
	}
}

func TestNormalizeKnownOpenAICodexModelGPT61Sol(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "openai/gpt-6.1-sol", "GPT-6.1_SOL-high", "gpt-6.1-sol-openai-compact"} {
		require.Equal(t, "gpt-6.1-sol", normalizeKnownOpenAICodexModel(model))
	}
	require.True(t, isOpenAIGPT6Model("gpt-6.1-sol"))
}

func TestUsageBillingModelCandidates_BareGPT56IncludesSol(t *testing.T) {
	require.Equal(t,
		[]string{"gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("gpt-5.6"),
	)
	require.Equal(t,
		[]string{"openai/gpt-5.6", "gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("openai/gpt-5.6"),
	)
}
