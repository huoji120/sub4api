// Platforms with hosted Responses search or the legacy Anthropic search shortcut.
export function supportsWebSearchEmulation(platform: string | undefined): boolean {
  return platform === 'anthropic' || platform === 'antigravity' ||
    platform === 'openai' || platform === 'openai_bps' || platform === 'grok' ||
    platform === 'kimi' || platform === 'zhipu' || platform === 'deepseek' ||
    platform === 'minimax' || platform === 'opencode_go'
}
