-- +goose Up
-- R1 schema (specification section 3). Truth is one append-only CRDT log per doc;
-- every other table is a projection that AdminService.Reindex can rebuild.

CREATE EXTENSION IF NOT EXISTS ltree;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Application role -------------------------------------------------------------
-- Request-path transactions run `SET LOCAL ROLE kb_app`, which is subject to the
-- row-level security policies below. Migrations and system jobs run as the
-- connection owner, which bypasses them.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kb_app') THEN
    CREATE ROLE kb_app NOLOGIN;
  END IF;
  EXECUTE format('GRANT kb_app TO %I', current_user);
END
$$;
-- +goose StatementEnd

-- Node ---------------------------------------------------------------------------
CREATE TABLE node (
  singleton         BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
  did               TEXT NOT NULL,
  signing_pubkey    BYTEA NOT NULL,
  encryption_pubkey BYTEA NOT NULL,
  bootstrap_admin   TEXT,
  bootstrapped_at   TIMESTAMPTZ,
  key_exported_at   TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Tenancy -------------------------------------------------------------------
CREATE TABLE workspaces (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  slug                TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  name                TEXT NOT NULL,
  current_key_version INT  NOT NULL DEFAULT 1,
  fga_store_id        TEXT NOT NULL,
  fga_model_id        TEXT NOT NULL,
  scheme_version      INT  NOT NULL DEFAULT 1,
  settings            JSONB NOT NULL DEFAULT '{}',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at          TIMESTAMPTZ
);

CREATE TABLE projects (
  workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id            UUID NOT NULL DEFAULT gen_random_uuid(),
  slug          TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  name          TEXT NOT NULL,
  root_page_id  UUID,
  settings      JSONB NOT NULL DEFAULT '{}',
  archived_at   TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, slug)
);

-- Principals ----------------------------------------------------------------
CREATE TABLE principals (
  id                TEXT PRIMARY KEY,                 -- DID
  kind              TEXT NOT NULL CHECK (kind IN ('user','device','agent','link','automation','node')),
  owner_id          TEXT REFERENCES principals(id),   -- device, agent, link -> user
  display_name      TEXT,
  signing_pubkey    BYTEA,
  encryption_pubkey BYTEA,
  attestation       BYTEA,                            -- owner's signature over the two keys
  disabled_at       TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX principals_owner ON principals (owner_id) WHERE owner_id IS NOT NULL;

CREATE TABLE workspace_members (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  principal_id TEXT NOT NULL REFERENCES principals(id),
  role         TEXT NOT NULL,                         -- a role name from the workspace scheme
  invited_by   TEXT,
  joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, principal_id)
);
CREATE INDEX workspace_members_principal ON workspace_members (principal_id);

CREATE TABLE invites (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  did          TEXT NOT NULL,
  role         TEXT NOT NULL,
  created_by   TEXT NOT NULL,
  expires_at   TIMESTAMPTZ NOT NULL,
  accepted_at  TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id)
);
CREATE INDEX invites_did ON invites (did) WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE groups (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  name         TEXT NOT NULL,
  created_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, name)
);

CREATE TABLE group_members (
  workspace_id UUID NOT NULL,
  group_id     UUID NOT NULL,
  principal_id TEXT NOT NULL REFERENCES principals(id),
  added_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, group_id, principal_id),
  FOREIGN KEY (workspace_id, group_id) REFERENCES groups(workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE share_links (
  workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id              UUID NOT NULL DEFAULT gen_random_uuid(),
  resource_type   TEXT NOT NULL CHECK (resource_type IN ('workspace','project','doc')),
  resource_id     TEXT NOT NULL,
  role            TEXT NOT NULL,
  token_hash      BYTEA NOT NULL UNIQUE,
  expires_at      TIMESTAMPTZ,
  max_uses        INT,
  uses            INT NOT NULL DEFAULT 0,
  allow_anonymous BOOLEAN NOT NULL DEFAULT false,
  link_principal  TEXT REFERENCES principals(id),     -- kind = link
  created_by      TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at      TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id)
);

CREATE TABLE sessions (
  id           UUID PRIMARY KEY,
  principal_id TEXT NOT NULL REFERENCES principals(id),
  device_id    TEXT,
  refresh_hash BYTEA NOT NULL UNIQUE,
  -- previous refresh hash kept for one rotation so a reuse can be detected and the session revoked
  prev_refresh_hash BYTEA,
  expires_at   TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_principal ON sessions (principal_id) WHERE revoked_at IS NULL;
CREATE INDEX sessions_prev_refresh ON sessions (prev_refresh_hash) WHERE prev_refresh_hash IS NOT NULL;

CREATE TABLE auth_challenges (
  nonce      TEXT PRIMARY KEY,
  did        TEXT NOT NULL,
  message    TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMPTZ NOT NULL,
  used_at    TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX auth_challenges_expiry ON auth_challenges (expires_at);

CREATE TABLE auth_failures (
  did        TEXT NOT NULL,
  at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX auth_failures_did ON auth_failures (did, at);

CREATE TABLE agent_tokens (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  principal_id TEXT NOT NULL REFERENCES principals(id),   -- kind = agent
  owner_id     TEXT NOT NULL REFERENCES principals(id),
  name         TEXT NOT NULL,
  token_hash   BYTEA NOT NULL UNIQUE,
  scopes       JSONB NOT NULL,
  tools        TEXT[] NOT NULL DEFAULT '{}',
  expires_at   TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ,
  throttled_until TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id)
);
CREATE INDEX agent_tokens_principal ON agent_tokens (principal_id);

CREATE TABLE rate_limits (
  bucket     TEXT PRIMARY KEY,
  tokens     DOUBLE PRECISION NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Keys ------------------------------------------------------------------------
CREATE TABLE workspace_keys (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  key_version  INT  NOT NULL,
  recipient_id TEXT NOT NULL REFERENCES principals(id),   -- always the node
  wrapped_key  BYTEA NOT NULL,                            -- HPKE to recipient's X25519 key
  wrapped_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, key_version, recipient_id)
);

-- Truth -----------------------------------------------------------------------
CREATE TABLE docs (
  workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id              UUID NOT NULL,
  project_id      UUID NOT NULL,
  kind            TEXT NOT NULL CHECK (kind IN ('page','boundary')),
  page_id         UUID NOT NULL,          -- self for kind = page
  parent_doc_id   UUID,                   -- enclosing doc for boundaries
  portal_block_id UUID,                   -- block in the parent doc that stands for this boundary
  key_version     INT  NOT NULL,
  current_seq     BIGINT NOT NULL DEFAULT 0,
  snapshot_seq    BIGINT NOT NULL DEFAULT 0,
  indexed_seq     BIGINT NOT NULL DEFAULT 0,
  deleted_at      TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  FOREIGN KEY (workspace_id, project_id) REFERENCES projects(workspace_id, id)
) WITH (fillfactor = 80, autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_cost_delay = 2);
CREATE INDEX docs_page ON docs (workspace_id, page_id);
CREATE INDEX docs_unindexed ON docs (workspace_id, id) WHERE indexed_seq < current_seq;

CREATE TABLE doc_updates (
  workspace_id UUID NOT NULL,
  doc_id       UUID NOT NULL,
  seq          BIGINT NOT NULL,
  actor_id     TEXT NOT NULL,
  client_id    TEXT NOT NULL,
  client_seq   BIGINT NOT NULL,
  key_version  INT NOT NULL,
  ciphertext   BYTEA NOT NULL,
  size         INT NOT NULL,
  quarantined_at TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, doc_id, seq),
  UNIQUE (workspace_id, doc_id, client_id, client_seq),          -- idempotent retries
  FOREIGN KEY (workspace_id, doc_id) REFERENCES docs(workspace_id, id) ON DELETE CASCADE
);
ALTER TABLE doc_updates ALTER COLUMN ciphertext SET STORAGE EXTERNAL;
CREATE INDEX doc_updates_created ON doc_updates USING BRIN (created_at);

CREATE TABLE doc_snapshots (
  workspace_id UUID NOT NULL,
  doc_id       UUID NOT NULL,
  seq          BIGINT NOT NULL,
  key_version  INT NOT NULL,
  ciphertext   BYTEA NOT NULL,
  state_vector BYTEA NOT NULL,
  shallow      BOOLEAN NOT NULL DEFAULT false,
  named        TEXT,                                              -- user-named version, kept past retention
  created_by   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, doc_id, seq),
  FOREIGN KEY (workspace_id, doc_id) REFERENCES docs(workspace_id, id) ON DELETE CASCADE
);
ALTER TABLE doc_snapshots ALTER COLUMN ciphertext SET STORAGE EXTERNAL;

-- Outbox: range-partitioned by day; retention is DROP PARTITION.
CREATE TABLE outbox (
  id           BIGSERIAL,
  workspace_id UUID NOT NULL,
  project_id   UUID,
  doc_id       UUID,
  seq          BIGINT,
  kind         TEXT NOT NULL,
  payload      JSONB NOT NULL,
  actor_id     TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at TIMESTAMPTZ,
  PRIMARY KEY (created_at, id)
) PARTITION BY RANGE (created_at);
CREATE TABLE outbox_default PARTITION OF outbox DEFAULT;
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;
CREATE INDEX outbox_workspace_id ON outbox (workspace_id, id);

-- Schema (user-defined types) --------------------------------------------------
CREATE TABLE property_definitions (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  project_id   UUID,                                    -- NULL = workspace-wide
  name         TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('text','number','date','select','multi_select','relation','user','checkbox','url')),
  config       JSONB NOT NULL DEFAULT '{}',             -- options, relation_type_id, reflect_relation
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id)
);
CREATE UNIQUE INDEX property_definitions_name ON property_definitions (workspace_id, COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), name);

CREATE TABLE block_types (
  workspace_id       UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id                 UUID NOT NULL DEFAULT gen_random_uuid(),
  project_id         UUID NOT NULL,
  name               TEXT NOT NULL,
  extends_type_id    UUID,
  collection_capable BOOLEAN NOT NULL DEFAULT false,
  owns_doc           BOOLEAN NOT NULL DEFAULT false,
  numbered           BOOLEAN NOT NULL DEFAULT false,
  required_props     UUID[] NOT NULL DEFAULT '{}',
  allowed_props      UUID[],                            -- NULL = any
  defaults           JSONB NOT NULL DEFAULT '{}',
  icon               TEXT,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, project_id, name)
);

CREATE TABLE relation_types (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  project_id   UUID NOT NULL,
  name         TEXT NOT NULL,
  inverse_name TEXT,
  is_symmetric BOOLEAN NOT NULL DEFAULT false,
  dag          BOOLEAN NOT NULL DEFAULT false,
  source_types UUID[],
  target_types UUID[],
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, project_id, name)
);

-- Work item keys --------------------------------------------------------------
CREATE TABLE project_counters (
  workspace_id UUID NOT NULL,
  project_id   UUID NOT NULL,
  prefix       TEXT NOT NULL CHECK (prefix ~ '^[A-Z][A-Z0-9]{1,9}$'),    -- KB in KB-123
  next_number  BIGINT NOT NULL DEFAULT 1,
  PRIMARY KEY (workspace_id, project_id),
  FOREIGN KEY (workspace_id, project_id) REFERENCES projects(workspace_id, id) ON DELETE CASCADE
);

CREATE TABLE block_keys (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  key          TEXT NOT NULL,                                       -- KB-123
  block_id     UUID NOT NULL,
  current      BOOLEAN NOT NULL DEFAULT true,     -- old keys stay as aliases after a cross-project move
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, key)
);
CREATE INDEX block_keys_block ON block_keys (workspace_id, block_id) WHERE current;

-- Projections (rebuildable) --------------------------------------------------
CREATE TABLE pages (
  workspace_id   UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id             UUID NOT NULL,             -- id = the page block id
  project_id     UUID NOT NULL,
  doc_id         UUID NOT NULL,
  title          TEXT NOT NULL DEFAULT '',
  title_norm     TEXT NOT NULL DEFAULT '',
  parent_page_id UUID,
  journal_date   DATE,
  format         TEXT NOT NULL DEFAULT 'markdown',
  type_id        UUID,
  key            TEXT,
  icon           TEXT,
  attributes     JSONB NOT NULL DEFAULT '{}',
  title_conflict BOOLEAN NOT NULL DEFAULT false,
  indexed_seq    BIGINT NOT NULL,
  deleted_at     TIMESTAMPTZ,
  created_by     TEXT NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL,
  updated_at     TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, id)
) WITH (fillfactor = 80, autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_cost_delay = 2);
CREATE INDEX pages_title ON pages (workspace_id, project_id, title_norm);
CREATE INDEX pages_journal ON pages (workspace_id, project_id, journal_date) WHERE journal_date IS NOT NULL;
CREATE INDEX pages_updated ON pages (workspace_id, project_id, updated_at DESC, id);
CREATE INDEX pages_parent ON pages (workspace_id, parent_page_id) WHERE parent_page_id IS NOT NULL;

CREATE TABLE page_aliases (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  alias_norm   TEXT NOT NULL,
  page_id      UUID NOT NULL,
  PRIMARY KEY (workspace_id, alias_norm)
);

CREATE TABLE blocks (
  workspace_id       UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id                 UUID NOT NULL,
  project_id         UUID NOT NULL,
  page_id            UUID NOT NULL,
  doc_id             UUID NOT NULL,
  parent_block_id    UUID,
  rank               TEXT NOT NULL,
  wbs_path           LTREE NOT NULL,
  depth              INT NOT NULL,
  type_id            UUID,
  key                TEXT,
  markdown           TEXT NOT NULL DEFAULT '',
  text               TEXT NOT NULL DEFAULT '',
  tsv                TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', text)) STORED,
  attributes         JSONB NOT NULL DEFAULT '{}',
  invalid_attributes JSONB NOT NULL DEFAULT '{}',
  compliant          BOOLEAN NOT NULL DEFAULT true,
  has_acl            BOOLEAN NOT NULL DEFAULT false,
  portal_doc_id      UUID,
  src_line           INT,
  indexed_seq        BIGINT NOT NULL,
  created_by         TEXT NOT NULL,
  updated_by         TEXT NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL,
  updated_at         TIMESTAMPTZ NOT NULL,
  deleted_at         TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id)
) WITH (fillfactor = 80, autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_cost_delay = 2);
CREATE INDEX blocks_tree   ON blocks (workspace_id, page_id, parent_block_id, rank);
CREATE INDEX blocks_doc    ON blocks (workspace_id, doc_id);
CREATE INDEX blocks_type   ON blocks (workspace_id, project_id, type_id);
CREATE INDEX blocks_key    ON blocks (workspace_id, key) WHERE key IS NOT NULL;
CREATE INDEX blocks_updated ON blocks (workspace_id, project_id, updated_at DESC, id);
CREATE INDEX blocks_path   ON blocks USING GIST (wbs_path);
CREATE INDEX blocks_attrs  ON blocks USING GIN (attributes jsonb_path_ops);
CREATE INDEX blocks_trgm   ON blocks USING GIN (text gin_trgm_ops) WITH (fastupdate = on, gin_pending_list_limit = 8192);
CREATE INDEX blocks_tsv    ON blocks USING GIN (tsv) WITH (fastupdate = on, gin_pending_list_limit = 8192);

CREATE TABLE block_properties (
  workspace_id    UUID NOT NULL,
  block_id        UUID NOT NULL,
  property_id     UUID NOT NULL,
  ord             INT NOT NULL DEFAULT 0,
  value_text      TEXT,
  value_number    NUMERIC,
  value_date      TIMESTAMPTZ,
  value_bool      BOOLEAN,
  value_ref       UUID,
  value_principal TEXT,
  PRIMARY KEY (workspace_id, block_id, property_id, ord)
);
CREATE INDEX bp_text   ON block_properties (workspace_id, property_id, value_text);
CREATE INDEX bp_num    ON block_properties (workspace_id, property_id, value_number);
CREATE INDEX bp_date   ON block_properties (workspace_id, property_id, value_date);
CREATE INDEX bp_ref    ON block_properties (workspace_id, property_id, value_ref);
CREATE INDEX bp_princ  ON block_properties (workspace_id, property_id, value_principal);

CREATE TABLE block_edges (
  id               BIGSERIAL,
  workspace_id     UUID NOT NULL,
  source_block_id  UUID NOT NULL,
  target_block_id  UUID,                                          -- NULL while unresolved
  edge_kind        TEXT NOT NULL CHECK (edge_kind IN ('ref','tag','embed','relation')),
  relation_type_id UUID,
  origin           TEXT NOT NULL CHECK (origin IN ('inline','property')),
  target_title     TEXT,
  indexed_seq      BIGINT NOT NULL,
  PRIMARY KEY (workspace_id, id)
);
CREATE UNIQUE INDEX block_edges_unique ON block_edges (
  workspace_id, source_block_id, edge_kind,
  COALESCE(relation_type_id, '00000000-0000-0000-0000-000000000000'::uuid),
  COALESCE(target_block_id, '00000000-0000-0000-0000-000000000000'::uuid),
  COALESCE(target_title, '')
);
CREATE INDEX edges_target ON block_edges (workspace_id, target_block_id, edge_kind, relation_type_id);
CREATE INDEX edges_source ON block_edges (workspace_id, source_block_id, edge_kind, relation_type_id);
CREATE INDEX edges_unresolved ON block_edges (workspace_id, lower(target_title)) WHERE target_block_id IS NULL;

CREATE TABLE doc_access (                                       -- projected from FGA
  workspace_id UUID NOT NULL,
  doc_id       UUID NOT NULL,
  set_id       TEXT NOT NULL,
  permission   TEXT NOT NULL CHECK (permission IN ('view','edit')),
  PRIMARY KEY (workspace_id, doc_id, permission, set_id)
);
CREATE INDEX doc_access_set ON doc_access (workspace_id, set_id, permission);

CREATE TABLE principal_sets (                                   -- cached per principal
  workspace_id UUID NOT NULL,
  principal_id TEXT NOT NULL,
  set_id       TEXT NOT NULL,
  computed_at  TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, principal_id, set_id)
);

CREATE TABLE view_programs (                                    -- projection, rebuilt from view blocks
  workspace_id   UUID NOT NULL,
  view_block_id  UUID NOT NULL,
  project_id     UUID NOT NULL,
  cel            TEXT NOT NULL,
  checked_ast    BYTEA NOT NULL,
  sql_template   TEXT NOT NULL,
  schema_version INT NOT NULL,
  status         TEXT NOT NULL CHECK (status IN ('ok','needs_recheck','error')),
  error          TEXT,
  updated_at     TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, view_block_id)
);

CREATE TABLE publications (
  workspace_id     UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id               UUID NOT NULL DEFAULT gen_random_uuid(),
  page_id          UUID NOT NULL,
  slug             TEXT NOT NULL,
  mode             TEXT NOT NULL CHECK (mode IN ('live','snapshot')),
  pinned_seq       BIGINT,
  pinned_seqs      JSONB NOT NULL DEFAULT '{}',                  -- doc_id -> seq for included child docs
  include_children BOOLEAN NOT NULL DEFAULT true,
  expires_at       TIMESTAMPTZ,
  created_by       TEXT NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at       TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id)
);
CREATE UNIQUE INDEX publications_slug ON publications (workspace_id, slug) WHERE revoked_at IS NULL;
CREATE INDEX publications_page ON publications (workspace_id, page_id) WHERE revoked_at IS NULL;

CREATE TABLE publication_redirects (                          -- old slug -> new slug, 90 days
  workspace_id UUID NOT NULL,
  slug         TEXT NOT NULL,
  target_slug  TEXT NOT NULL,
  expires_at   TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, slug)
);

CREATE TABLE assets (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  sha256       BYTEA NOT NULL,
  size         BIGINT NOT NULL,
  content_type TEXT NOT NULL,
  storage_key  TEXT NOT NULL,
  key_version  INT NOT NULL,
  created_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at   TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id),
  UNIQUE (workspace_id, sha256)
);

CREATE TABLE uploads (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  purpose      TEXT NOT NULL CHECK (purpose IN ('asset','import','export')),
  content_type TEXT NOT NULL,
  size         BIGINT NOT NULL,
  sha256       BYTEA,
  token_hash   BYTEA NOT NULL UNIQUE,
  storage_key  TEXT,
  asset_id     UUID,
  created_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ,
  expires_at   TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (workspace_id, id)
);

CREATE TABLE import_jobs (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  id           UUID NOT NULL DEFAULT gen_random_uuid(),
  project_id   UUID NOT NULL,
  upload_id    UUID NOT NULL,
  options      JSONB NOT NULL DEFAULT '{}',
  state        TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','done','failed')),
  report       JSONB NOT NULL DEFAULT '{}',
  error        TEXT,
  created_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  PRIMARY KEY (workspace_id, id)
);

CREATE TABLE schemes (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  version      INT NOT NULL,
  scheme       JSONB NOT NULL,
  model_dsl    TEXT NOT NULL,
  fga_model_id TEXT NOT NULL,
  created_by   TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, version)
);

-- Audit log: range-partitioned by month; SHA-256 chain per workspace.
CREATE TABLE audit_log (
  workspace_id  UUID NOT NULL,
  id            BIGSERIAL,
  at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  principal_id  TEXT NOT NULL,
  owner_id      TEXT,
  action        TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  detail        JSONB NOT NULL DEFAULT '{}',
  prev_hash     BYTEA NOT NULL,
  hash          BYTEA NOT NULL,
  PRIMARY KEY (at, workspace_id, id)
) PARTITION BY RANGE (at);
CREATE TABLE audit_log_default PARTITION OF audit_log DEFAULT;
CREATE INDEX audit_log_workspace ON audit_log (workspace_id, id);
CREATE INDEX audit_log_at ON audit_log USING BRIN (at);

CREATE TABLE idempotency_keys (
  workspace_id UUID NOT NULL,
  principal_id TEXT NOT NULL,
  key          TEXT NOT NULL,
  request_hash BYTEA NOT NULL,
  response     BYTEA NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, principal_id, key)
);
CREATE INDEX idempotency_keys_created ON idempotency_keys USING BRIN (created_at);

-- OAuth 2.1 authorization server for MCP clients (section 10) ------------------
CREATE TABLE oauth_clients (
  client_id     TEXT PRIMARY KEY,
  metadata      JSONB NOT NULL,
  registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_codes (
  code_hash      BYTEA PRIMARY KEY,
  client_id      TEXT NOT NULL,
  redirect_uri   TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  resource       TEXT NOT NULL,
  scope          TEXT NOT NULL,
  workspace_id   UUID NOT NULL,
  agent_token_id UUID NOT NULL,
  principal_id   TEXT NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  used_at        TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_refresh_tokens (
  token_hash     BYTEA PRIMARY KEY,
  client_id      TEXT NOT NULL,
  workspace_id   UUID NOT NULL,
  agent_token_id UUID NOT NULL,
  resource       TEXT NOT NULL,
  scope          TEXT NOT NULL,
  expires_at     TIMESTAMPTZ NOT NULL,
  revoked_at     TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Row-level security ------------------------------------------------------------
-- Tenant tables: one policy on workspace_id. The API sets app.workspace_id (and
-- app.principal_id) with SET LOCAL inside every request transaction and switches
-- to the kb_app role, which cannot bypass these policies.
-- +goose StatementBegin
DO $$
DECLARE
  t TEXT;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'projects','invites','groups','group_members','share_links','agent_tokens',
    'workspace_keys','docs','doc_updates','doc_snapshots','outbox',
    'property_definitions','block_types','relation_types','project_counters','block_keys',
    'pages','page_aliases','blocks','block_properties','block_edges','doc_access',
    'principal_sets','view_programs','publications','publication_redirects','assets',
    'uploads','import_jobs','schemes','audit_log','idempotency_keys'
  ] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format(
      'CREATE POLICY tenant_isolation ON %I USING (workspace_id = NULLIF(current_setting(''app.workspace_id'', true), '''')::uuid)', t);
  END LOOP;
END
$$;
-- +goose StatementEnd

ALTER TABLE workspace_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_or_self ON workspace_members USING (
  workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
  OR principal_id = NULLIF(current_setting('app.principal_id', true), '')
);

ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_or_member ON workspaces USING (
  id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
  OR EXISTS (
    SELECT 1 FROM workspace_members m
    WHERE m.workspace_id = workspaces.id
      AND m.principal_id = NULLIF(current_setting('app.principal_id', true), '')
  )
);

-- Grants for the application role.
GRANT USAGE ON SCHEMA public TO kb_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kb_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO kb_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kb_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO kb_app;

-- +goose Down
DROP TABLE IF EXISTS oauth_refresh_tokens, oauth_codes, oauth_clients, idempotency_keys, audit_log, schemes,
  import_jobs, uploads, assets, publication_redirects, publications, view_programs, principal_sets, doc_access,
  block_edges, block_properties, blocks, page_aliases, pages, block_keys, project_counters, relation_types,
  block_types, property_definitions, outbox, doc_snapshots, doc_updates, docs, workspace_keys, rate_limits,
  agent_tokens, auth_failures, auth_challenges, sessions, share_links, group_members, groups, invites,
  workspace_members, principals, projects, workspaces, node CASCADE;
