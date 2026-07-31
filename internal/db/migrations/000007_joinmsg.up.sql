-- 000007_joinmsg.up.sql — храним id service-сообщения «теперь в группе» (new_chat_members),
-- чтобы удалить его при кике/уходе новичка и не оставлять след входа бота в чате.
-- Идемпотентно (IF NOT EXISTS).

ALTER TABLE newcomer_gates ADD COLUMN IF NOT EXISTS service_message_id BIGINT NOT NULL DEFAULT 0;
