CREATE TABLE IF NOT EXISTS spam_flags (
    id              BIGSERIAL PRIMARY KEY,
    chat_id         BIGINT NOT NULL,
    message_id      BIGINT NOT NULL,
    tg_user_id      BIGINT NOT NULL,
    username        TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    action          TEXT NOT NULL DEFAULT 'warn',
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    restrict_until  TIMESTAMPTZ,
    released_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_spam_flags_msg ON spam_flags (chat_id, message_id);
CREATE INDEX IF NOT EXISTS idx_spam_flags_user_time ON spam_flags (tg_user_id, detected_at);
CREATE INDEX IF NOT EXISTS idx_spam_restricts ON spam_flags (restrict_until)
    WHERE action = 'restrict' AND released_at IS NULL;
