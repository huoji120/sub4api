-- Durable, bounded request/completion audit records. Raw credentials are redacted before insertion.
CREATE TABLE IF NOT EXISTS user_request_audits (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    group_id BIGINT REFERENCES groups(id) ON DELETE SET NULL,
    group_name VARCHAR(255) NOT NULL DEFAULT '',
    protocol VARCHAR(64) NOT NULL CHECK (protocol IN ('anthropic_messages','openai_responses','openai_chat_completions','openai_responses_ws')),
    endpoint VARCHAR(512) NOT NULL DEFAULT '',
    requested_model VARCHAR(255) NOT NULL DEFAULT '',
    upstream_model VARCHAR(255) NOT NULL DEFAULT '',
    client_request_id VARCHAR(255) NOT NULL DEFAULT '',
    response_id VARCHAR(255) NOT NULL DEFAULT '',
    previous_response_id VARCHAR(255) NOT NULL DEFAULT '',
    fallback_hash CHAR(64) NOT NULL DEFAULT '',
    conversation_key VARCHAR(160) NOT NULL DEFAULT '',
    request_chatml TEXT NOT NULL DEFAULT '',
    response_chatml TEXT NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'received',
    input_usage JSONB,
    output_usage JSONB,
    cache_usage JSONB,
    last_error VARCHAR(1024) NOT NULL DEFAULT '',
    metadata JSONB,
    logical_key VARCHAR(64) NOT NULL UNIQUE,
    CONSTRAINT user_request_audits_expiry_order CHECK (expires_at >= created_at),
    CONSTRAINT user_request_audits_ids_positive CHECK (user_id > 0 AND api_key_id > 0)
);

-- This migration may be applied over the original audit table by installations that
-- created it before conversation association and UUID request keys were introduced.
ALTER TABLE user_request_audits ADD COLUMN IF NOT EXISTS conversation_key VARCHAR(160) NOT NULL DEFAULT '';
ALTER TABLE user_request_audits ALTER COLUMN logical_key TYPE VARCHAR(64) USING btrim(logical_key);
UPDATE user_request_audits
SET conversation_key = CASE WHEN fallback_hash <> '' THEN 'fallback:' || btrim(fallback_hash) ELSE 'request:' || btrim(logical_key) END
WHERE conversation_key = '';

CREATE INDEX IF NOT EXISTS idx_user_request_audits_expires_at ON user_request_audits(expires_at);
CREATE INDEX IF NOT EXISTS idx_user_request_audits_user_created ON user_request_audits(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_request_audits_group_created ON user_request_audits(group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_request_audits_response_id ON user_request_audits(response_id);
CREATE INDEX IF NOT EXISTS idx_user_request_audits_fallback_hash ON user_request_audits(fallback_hash);
CREATE INDEX IF NOT EXISTS idx_user_request_audits_conversation_scope ON user_request_audits(user_id, group_id, protocol, conversation_key, created_at, id);

INSERT INTO settings(key, value, updated_at)
VALUES ('user_request_audit_retention_days', '7', NOW())
ON CONFLICT (key) DO NOTHING;
