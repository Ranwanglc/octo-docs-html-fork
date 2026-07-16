-- octo-doc PostgreSQL schema. Applied automatically by the postgres adapter at
-- startup and by `octo-doc migrate`. This file is the canonical reference; the
-- adapter embeds the same idempotent DDL (internal/storage/postgres). Records are
-- stored as JSONB.

CREATE TABLE IF NOT EXISTS meta (
  slug        TEXT PRIMARY KEY,
  json        JSONB NOT NULL,        -- doc metadata: { title, slug, versions:[{n,created}] }
  updated_at  BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS comments (
  slug        TEXT PRIMARY KEY,
  json        JSONB NOT NULL,        -- the full event-log comment array for the slug
  updated_at  BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  sid         TEXT PRIMARY KEY,
  json        JSONB NOT NULL,        -- { login, avatar_url, name, created }
  expires_at  BIGINT NOT NULL
);

-- Sessions are swept by expiry on every write; index the predicate so that sweep
-- is not a full table scan under load.
CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE IF NOT EXISTS tokens (
  token       TEXT PRIMARY KEY,
  json        JSONB NOT NULL,        -- { token, created, label }
  created_at  BIGINT NOT NULL
);
