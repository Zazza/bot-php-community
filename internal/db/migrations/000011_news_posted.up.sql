-- 000011_news_posted.up.sql — дедуп PHP-новостей (не постить ссылку, что уже постили).
-- Идемпотентно (IF NOT EXISTS). Forward-only.

CREATE TABLE IF NOT EXISTS news_posted (
  chat_id   BIGINT      NOT NULL,
  url_hash  CHAR(40)    NOT NULL,
  posted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (chat_id, url_hash)
);
