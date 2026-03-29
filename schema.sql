-- StreamSrv PostgreSQL schema
-- Run manually OR server auto-migrates on startup (migrateDB)

CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL    PRIMARY KEY,
    username      TEXT         UNIQUE NOT NULL,
    password_hash TEXT         NOT NULL,
    avatar_url    TEXT         NOT NULL DEFAULT '',
    bio           TEXT         NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS streams (
    id           BIGSERIAL    PRIMARY KEY,
    user_id      BIGINT       REFERENCES users(id) ON DELETE SET NULL,
    room_name    TEXT         NOT NULL,
    title        TEXT         NOT NULL DEFAULT '',
    category     TEXT         NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ended_at     TIMESTAMPTZ,           -- NULL = currently live
    peak_viewers INT          NOT NULL DEFAULT 0,
    duration_sec INT          NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         BIGSERIAL    PRIMARY KEY,
    stream_id  BIGINT       REFERENCES streams(id) ON DELETE CASCADE,
    user_id    BIGINT,                  -- NULL = guest
    username   TEXT         NOT NULL,
    role       TEXT         NOT NULL DEFAULT 'viewer',
    message    TEXT         NOT NULL,
    sent_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS room_bans (
    id         BIGSERIAL    PRIMARY KEY,
    room_name  TEXT         NOT NULL,
    username   TEXT         NOT NULL,
    banned_by  TEXT         NOT NULL,
    reason     TEXT         NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,            -- NULL = permanent
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE(room_name, username)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_streams_room  ON streams(room_name);
CREATE INDEX IF NOT EXISTS idx_streams_user  ON streams(user_id);
CREATE INDEX IF NOT EXISTS idx_streams_live  ON streams(ended_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_chat_stream   ON chat_messages(stream_id);
CREATE INDEX IF NOT EXISTS idx_chat_sent     ON chat_messages(sent_at);
CREATE INDEX IF NOT EXISTS idx_bans_room     ON room_bans(room_name);

-- Useful debug queries:
--
-- Live streams:
--   SELECT s.*,u.username FROM streams s LEFT JOIN users u ON u.id=s.user_id WHERE s.ended_at IS NULL;
--
-- Top streamers:
--   SELECT u.username, COUNT(*) streams, SUM(s.peak_viewers) total_viewers
--   FROM streams s JOIN users u ON u.id=s.user_id GROUP BY u.username ORDER BY 3 DESC LIMIT 20;
--
-- Remove a ban:
--   DELETE FROM room_bans WHERE room_name='alice' AND username='spammer';