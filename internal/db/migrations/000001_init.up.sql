-- 000001_init.up.sql — основные таблицы php-bot.
-- Идемпотентно (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS users (
    tg_user_id  BIGINT PRIMARY KEY,
    username    TEXT NOT NULL DEFAULT '',
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    status      TEXT NOT NULL DEFAULT 'member'  -- 'member' | 'suspect' | 'banned'
);

CREATE TABLE IF NOT EXISTS messages (
    id           BIGINT PRIMARY KEY,            -- tg message_id (уникален в чате, но для простоты PK)
    chat_id      BIGINT NOT NULL,
    user_id      BIGINT NOT NULL,
    username     TEXT NOT NULL DEFAULT '',
    text         TEXT NOT NULL DEFAULT '',
    ts           TIMESTAMPTZ NOT NULL DEFAULT now(),
    reply_to_id  BIGINT
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages (chat_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_messages_user    ON messages (user_id, ts DESC);

CREATE TABLE IF NOT EXISTS newcomer_verdicts (
    id             BIGSERIAL PRIMARY KEY,
    tg_user_id     BIGINT NOT NULL,
    chat_id        BIGINT NOT NULL,
    question       TEXT NOT NULL DEFAULT '',
    answer         TEXT NOT NULL DEFAULT '',
    verdict        TEXT NOT NULL,               -- 'bot' | 'human' | 'unclear'
    reason         TEXT NOT NULL DEFAULT '',
    admin_action   TEXT,                         -- 'kicked' | 'kept' | NULL
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_verdicts_user ON newcomer_verdicts (tg_user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS topics (
    id         BIGSERIAL PRIMARY KEY,
    text       TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'llm',     -- 'llm' | 'rss' | 'admin'
    used       BOOLEAN NOT NULL DEFAULT FALSE,
    posted_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS topic_digests (
    id            BIGSERIAL PRIMARY KEY,
    chat_id       BIGINT NOT NULL,
    period_start  TIMESTAMPTZ NOT NULL,
    period_end    TIMESTAMPTZ NOT NULL,
    summary       TEXT NOT NULL,
    posted        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
