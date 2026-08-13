-- Durable, credential-free checkout admission. A reservation is invalidated by
-- any authority revision change and can never contain a provider token.
ALTER TABLE github_repositories ADD COLUMN clone_url TEXT;
UPDATE github_repositories SET clone_url='https://github.com/' || full_name || '.git' WHERE clone_url IS NULL;
ALTER TABLE github_repositories ALTER COLUMN clone_url SET NOT NULL;
ALTER TABLE github_repositories ADD CONSTRAINT github_repositories_clone_url_canonical
  CHECK (clone_url = 'https://github.com/' || full_name || '.git');
CREATE UNIQUE INDEX github_repositories_repository_installation_owner_key
  ON github_repositories(repository_id, installation_id, owner_id);
ALTER TABLE github_project_bindings ADD CONSTRAINT github_project_bindings_repository_installation_owner_fkey
  FOREIGN KEY (repository_id, installation_id, owner_id)
  REFERENCES github_repositories(repository_id, installation_id, owner_id);
CREATE TABLE github_checkout_reservations (
  id UUID PRIMARY KEY,
  owner_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  installation_id BIGINT NOT NULL,
  repository_id BIGINT NOT NULL,
  clone_url TEXT NOT NULL,
  commit_sha TEXT NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
  binding_revision BIGINT NOT NULL,
  repository_revision BIGINT NOT NULL,
  installation_revision BIGINT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('reserved','minting','active','running','consumed','invalidated','expired')),
  attempts SMALLINT NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1),
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
  consumed_at TIMESTAMPTZ,
  FOREIGN KEY(owner_id,project_id) REFERENCES github_project_bindings(owner_id,project_id),
  FOREIGN KEY(repository_id,installation_id,owner_id) REFERENCES github_repositories(repository_id,installation_id,owner_id),
  CHECK (clone_url ~ '^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$'),
  CHECK (expires_at > created_at)
);
CREATE UNIQUE INDEX github_checkout_one_live_per_binding
  ON github_checkout_reservations(owner_id,project_id)
  WHERE state IN ('reserved','minting','active','running');
CREATE INDEX github_checkout_expiry ON github_checkout_reservations(expires_at) WHERE state IN ('reserved','minting','active','running');
