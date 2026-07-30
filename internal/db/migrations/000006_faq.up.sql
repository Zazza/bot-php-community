-- 000006_faq.up.sql — FAQ: курируемые вопрос-ответы с вектором вопроса для быстрого матча.
-- Идемпотентно (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS faq_items (
    id            BIGSERIAL PRIMARY KEY,
    chat_id       BIGINT NOT NULL,
    question      TEXT NOT NULL,
    answer        TEXT NOT NULL,
    question_vec  vector(1536),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_faq_chat ON faq_items (chat_id);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'faq_items' AND indexname = 'idx_faq_vec'
    ) THEN
        EXECUTE 'CREATE INDEX idx_faq_vec ON faq_items USING hnsw (question_vec vector_cosine_ops)';
    END IF;
END $$;
