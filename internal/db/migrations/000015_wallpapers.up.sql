-- 000015_wallpapers.up.sql — дедуп «Пятничных обоев» (не показывать работу,
-- что уже выходила в прошлые пятницы). Ключ wallpaper — 'theme/slug' из пула
-- (напр. 'terminal/pest-test'). Пул ~42 работы, чат один (chat_id в ключе —
-- по прецеденту news_posted). Когда пул исчерпан — приложение делает
-- DELETE WHERE chat_id и цикл начинается заново.
-- Идемпотентно (IF NOT EXISTS). Forward-only.

CREATE TABLE IF NOT EXISTS news_wallpapers (
  chat_id   BIGINT      NOT NULL,
  wallpaper TEXT        NOT NULL,
  shown_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (chat_id, wallpaper)
);
