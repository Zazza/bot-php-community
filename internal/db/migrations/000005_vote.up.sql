CREATE TABLE IF NOT EXISTS kick_votes (
    id              BIGSERIAL PRIMARY KEY,
    chat_id         BIGINT NOT NULL,
    target_user_id  BIGINT NOT NULL,
    target_username TEXT NOT NULL DEFAULT '',
    reason          TEXT NOT NULL DEFAULT '',
    message_id      BIGINT NOT NULL DEFAULT 0,
    for_count       INT NOT NULL DEFAULT 0,
    against_count   INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closes_at       TIMESTAMPTZ NOT NULL,
    resolved_at     TIMESTAMPTZ,
    outcome         TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_votes_due ON kick_votes (closes_at) WHERE outcome = 'open';
CREATE UNIQUE INDEX IF NOT EXISTS uq_votes_active_target
    ON kick_votes (chat_id, target_user_id) WHERE outcome = 'open';

CREATE TABLE IF NOT EXISTS vote_ballots (
    vote_id  BIGINT NOT NULL REFERENCES kick_votes(id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL,
    choice   TEXT NOT NULL DEFAULT 'for',
    voted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (vote_id, user_id),
    CHECK (choice IN ('for', 'against'))
);
