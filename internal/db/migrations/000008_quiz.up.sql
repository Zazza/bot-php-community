-- 000008_quiz.up.sql — викторина: вопросы и голоса участников.
-- Идемпотентно (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS quizzes (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     BIGINT NOT NULL,
    kind        TEXT NOT NULL,             -- whoTop | whoFirst | stat | mentioned
    question    TEXT NOT NULL,
    opt1        TEXT NOT NULL DEFAULT '',
    opt2        TEXT NOT NULL DEFAULT '',
    opt3        TEXT NOT NULL DEFAULT '',
    opt4        TEXT NOT NULL DEFAULT '',
    correct_opt SMALLINT NOT NULL,         -- 0..3 — индекс верной опции
    message_id  BIGINT NOT NULL DEFAULT 0, -- id сообщения с кнопками (для редакта live-tally)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_quizzes_chat_created ON quizzes (chat_id, created_at DESC);

CREATE TABLE IF NOT EXISTS quiz_ballots (
    quiz_id     BIGINT NOT NULL REFERENCES quizzes(id) ON DELETE CASCADE,
    user_id     BIGINT NOT NULL,
    choice      SMALLINT NOT NULL,
    answered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (quiz_id, user_id)         -- один голос на участника
);
