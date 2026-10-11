-- 000017_oncall.up.sql — игра «дежурный по вызову»: сессия с фазами и дедлайнами,
-- раунды с планами (JSONB) и голосованием за план, бюллетени — один голос
-- участника на раунд сессии (паттерн quiz_ballots).
-- Идемпотентно (IF NOT EXISTS). Forward-only.

CREATE TABLE IF NOT EXISTS oncall_sessions (
    id               BIGSERIAL PRIMARY KEY,
    chat_id          BIGINT      NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    round            SMALLINT    NOT NULL DEFAULT 1,
    phase            TEXT        NOT NULL DEFAULT 'brief',
    phase_started    TIMESTAMPTZ NOT NULL DEFAULT now(),
    phase_deadline   TIMESTAMPTZ NOT NULL,
    phase_message_id BIGINT      NOT NULL DEFAULT 0,
    post_attempts    SMALLINT    NOT NULL DEFAULT 0,
    title            TEXT        NOT NULL DEFAULT '',
    scenario         TEXT        NOT NULL DEFAULT '',
    state            TEXT        NOT NULL DEFAULT '',
    silent_rounds    SMALLINT    NOT NULL DEFAULT 0,
    status           TEXT        NOT NULL DEFAULT 'active',
    finished_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_oncall_sessions_active
    ON oncall_sessions (chat_id) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_oncall_sessions_chat_started
    ON oncall_sessions (chat_id, started_at DESC);

CREATE TABLE IF NOT EXISTS oncall_rounds (
    id          BIGSERIAL   PRIMARY KEY,
    session_id  BIGINT      NOT NULL REFERENCES oncall_sessions(id) ON DELETE CASCADE,
    chat_id     BIGINT      NOT NULL,
    round       SMALLINT    NOT NULL,
    title       TEXT        NOT NULL DEFAULT '',
    scenario    TEXT        NOT NULL DEFAULT '',
    plans       JSONB       NOT NULL DEFAULT '[]',
    chosen_plan SMALLINT    NOT NULL DEFAULT -1,
    outcome     TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, round)
);
CREATE INDEX IF NOT EXISTS idx_oncall_rounds_chat_created
    ON oncall_rounds (chat_id, created_at DESC);

CREATE TABLE IF NOT EXISTS oncall_ballots (
    session_id BIGINT      NOT NULL REFERENCES oncall_sessions(id) ON DELETE CASCADE,
    round      SMALLINT    NOT NULL,
    user_id    BIGINT      NOT NULL,
    plan_idx   SMALLINT    NOT NULL,
    voted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, round, user_id)
);
