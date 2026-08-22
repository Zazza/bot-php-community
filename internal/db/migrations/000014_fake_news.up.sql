-- 000014_fake_news.up.sql — память пятничных фейк-выпусков (анти-повтор тем):
-- история опубликованных фейк-дайджестов, чтобы LLM не повторял темы прошлых выпусков.
-- Идемпотентно (IF NOT EXISTS). Forward-only.

CREATE TABLE IF NOT EXISTS news_fake_posts (
  id        BIGSERIAL   PRIMARY KEY,
  chat_id   BIGINT      NOT NULL,
  rubric    TEXT        NOT NULL DEFAULT '',
  body      TEXT        NOT NULL,
  posted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_news_fake_posts_chat ON news_fake_posts (chat_id, id DESC);
