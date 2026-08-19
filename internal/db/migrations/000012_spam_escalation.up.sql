-- 000012_spam_escalation.up.sql — эскалация спам-предупреждений: голосование участников
-- по флагу (spam_ballots + денормализованные атомарные счётчики spam_count/ok_count,
-- зеркало vote_ballots/kick_votes), взаимоисключающие терминальные состояния
-- false_positive/escalated_at (claim через UPDATE ... WHERE, без констрейнтов),
-- решение админа после эскалации (admin_action), warn_message_id — пост-предупреждение
-- для правки при эскалации/снятии. Свипер рестриктов больше не фильтрует по action
-- (эскалация ставит restrict_until на action='warn'), поэтому старый partial индекс
-- idx_spam_restricts (action='restrict') заменён на idx_spam_restricts_due.
-- Идемпотентно (IF NOT EXISTS / IF EXISTS). Forward-only.

ALTER TABLE spam_flags
    ADD COLUMN IF NOT EXISTS warn_message_id   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS spam_count        INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS ok_count          INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS false_positive    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS false_positive_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS escalated_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS admin_action      TEXT;             -- 'banned' | 'restored' | NULL

CREATE TABLE IF NOT EXISTS spam_ballots (
    flag_id  BIGINT NOT NULL REFERENCES spam_flags(id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL,
    choice   TEXT NOT NULL,
    voted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (flag_id, user_id),
    CHECK (choice IN ('spam', 'ok'))
);

-- Свипер рестриктов: restrict_until IS NOT NULL AND restrict_until < now()
-- AND released_at IS NULL — без фильтра action. Имя новое (-due, как idx_votes_due):
-- по имени видно, что миграция применена; старый индекс не покрывал warn-строки.
DROP INDEX IF EXISTS idx_spam_restricts;
CREATE INDEX IF NOT EXISTS idx_spam_restricts_due ON spam_flags (restrict_until)
    WHERE restrict_until IS NOT NULL AND released_at IS NULL;
