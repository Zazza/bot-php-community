-- 000016_drop_friday_fake.up.sql — удаление пятничного фейк-дайджеста и обоев
-- (фича убрана; память тем и сет показанных работ больше не нужны).
-- Идемпотентно (IF EXISTS).

DROP TABLE IF EXISTS news_fake_posts;
DROP TABLE IF EXISTS news_wallpapers;
