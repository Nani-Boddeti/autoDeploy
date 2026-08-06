-- Web login has not shipped, so old ordinary pair buckets cannot be attributed
-- safely. Remove only those pre-existing rows before association is enabled.
DELETE FROM auth_throttle_buckets WHERE kind = 'pair';

-- Pair buckets for authenticated, known users retain only the opaque user ID.
-- This lets an operator reset clear every client-specific recovery bucket
-- without storing a submitted username or touching network-wide evidence.
ALTER TABLE auth_throttle_buckets
  ADD COLUMN recovery_user_id TEXT REFERENCES users(id);

ALTER TABLE auth_throttle_buckets
  ADD CONSTRAINT auth_throttle_buckets_recovery_user_pair_check
  CHECK (kind = 'pair' OR recovery_user_id IS NULL);

CREATE INDEX auth_throttle_buckets_recovery_user_pair_idx
  ON auth_throttle_buckets(recovery_user_id)
  WHERE kind = 'pair' AND recovery_user_id IS NOT NULL;
