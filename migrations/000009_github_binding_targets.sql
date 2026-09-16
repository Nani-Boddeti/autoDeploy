-- Target authority is deliberately limited to GitHub bindings until the
-- project/environment/server aggregates exist. Legacy rows remain valid for
-- checkout but cannot authorize automatic deployment without every target.
ALTER TABLE github_project_bindings
  ADD COLUMN deploy_ref TEXT,
  ADD COLUMN environment_id TEXT,
  ADD COLUMN server_id TEXT;

ALTER TABLE github_project_bindings
  ADD CONSTRAINT github_project_bindings_deploy_ref_check CHECK (
    deploy_ref IS NULL OR (
      octet_length(deploy_ref) BETWEEN 12 AND 255
      AND deploy_ref LIKE 'refs/heads/%'
      AND deploy_ref !~ '[[:space:]~^:?*\[\\]'
      AND deploy_ref !~ E'[\\x00-\\x20\\x7F]'
      AND deploy_ref !~ '//' AND deploy_ref !~ '/$'
      AND deploy_ref !~ '\.\.' AND deploy_ref !~ '@\{'
      AND deploy_ref !~ '(^|/)\.' AND deploy_ref !~ '\.lock($|/)'
      AND deploy_ref !~ '\.($|/)'
    )
  ),
  ADD CONSTRAINT github_project_bindings_environment_id_check CHECK (
    environment_id IS NULL OR octet_length(environment_id) BETWEEN 1 AND 128
  ),
  ADD CONSTRAINT github_project_bindings_server_id_check CHECK (
    server_id IS NULL OR octet_length(server_id) BETWEEN 1 AND 128
  ),
  ADD CONSTRAINT github_project_bindings_target_complete_check CHECK (
    (deploy_ref IS NULL AND environment_id IS NULL AND server_id IS NULL)
    OR (deploy_ref IS NOT NULL AND environment_id IS NOT NULL AND server_id IS NOT NULL)
  );
