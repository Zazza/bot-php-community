-- 000003_gate.up.sql — анти-спам гейт новичков (капча + линк-провация).
-- Идемпотентно (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS newcomer_gates (
    id                 BIGSERIAL PRIMARY KEY,
    chat_id            BIGINT NOT NULL,
    tg_user_id         BIGINT NOT NULL,
    username           TEXT NOT NULL DEFAULT '',
    captcha_message_id BIGINT NOT NULL DEFAULT 0,
    correct_option     SMALLINT NOT NULL DEFAULT 0,
    attempts           SMALLINT NOT NULL DEFAULT 0,
    state              TEXT NOT NULL DEFAULT 'pending',  -- 'pending' | 'solved' | 'kicked' | 'cancelled'
    joined_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline           TIMESTAMPTZ NOT NULL,             -- крайний срок решения капчи
    probation_until    TIMESTAMPTZ,                       -- до когда действует «только текст»
    released_at        TIMESTAMPTZ                        -- когда провация снята
);

-- Свипер: ищет просроченные pending (кик) и не-просроченные pending (cleanup).
CREATE INDEX IF NOT EXISTS idx_gates_pending_deadline
    ON newcomer_gates (deadline) WHERE state = 'pending';
-- Свипер: снимает провацию по probation_until.
CREATE INDEX IF NOT EXISTS idx_gates_probation
    ON newcomer_gates (probation_until) WHERE state = 'solved' AND released_at IS NULL;
