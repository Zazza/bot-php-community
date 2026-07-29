-- 000002_vector.up.sql — pgvector: расширение, таблица эмбеддингов, HNSW-индекс.
-- Идемпотентно.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS embeddings (
    message_id  BIGINT PRIMARY KEY,
    embedding   vector(1536) NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- HNSW для косинусного поиска (vector_cosine_ops). Если индекс с другим opclass
-- существует — ON CONFLICT не поможет, поэтому проверяем через DO блок.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_indexes
        WHERE tablename = 'embeddings' AND indexname = 'idx_embeddings_hnsw'
    ) THEN
        EXECUTE 'CREATE INDEX idx_embeddings_hnsw ON embeddings USING hnsw (embedding vector_cosine_ops)';
    END IF;
END $$;
