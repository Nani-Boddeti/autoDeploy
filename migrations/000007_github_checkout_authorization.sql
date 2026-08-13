-- Checkout authorization uses the existing binding revision as its durable
-- compare-and-swap value.  This index makes both capture and revalidation
-- owner-scoped and cheap without persisting any credential material.
CREATE INDEX github_project_bindings_checkout_authorization_idx
  ON github_project_bindings(owner_id, project_id, authorization_revision, repository_id, installation_id)
  WHERE active;
