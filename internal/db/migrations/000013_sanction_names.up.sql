-- 000013_sanction_names.up.sql — идентификация нарушителя в санкционных сообщениях.
-- @username есть не у всех, числовой TG ID в постах не виден: в spam_flags/kick_votes
-- появляется человекочитаемое имя (first_name [+ last_name]) на момент нарушения —
-- публичные посты рендерят «@username → имя → fallback», ЛС админам — всегда + (id N).
-- Идемпотентно (IF NOT EXISTS). Forward-only.

ALTER TABLE spam_flags
    ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT '';

ALTER TABLE kick_votes
    ADD COLUMN IF NOT EXISTS target_name TEXT NOT NULL DEFAULT '';
