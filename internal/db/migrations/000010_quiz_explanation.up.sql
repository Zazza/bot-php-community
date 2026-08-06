-- 000010_quiz_explanation.up.sql — объяснение к вопросу викторины (показывается в тосте
-- ответа/раскрытия). Идемпотентно (IF NOT EXISTS). Forward-only.

ALTER TABLE quizzes ADD COLUMN IF NOT EXISTS explanation TEXT NOT NULL DEFAULT '';
