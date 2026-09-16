-- Migration 000008 added a clone_url format CHECK on github_checkout_reservations
-- using the pattern '^https://github\\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\\.git$'.
-- That literal is a plain (non-E'') single-quoted string, so under Postgres's
-- default standard_conforming_strings=on the backslashes are NOT escape
-- sequences: the stored regex contains two literal backslash characters before
-- each dot. As a POSIX regex, '\\' matches one literal backslash, so the
-- pattern requires a literal backslash byte immediately before ".com" and
-- before ".git" -- a sequence no canonical clone URL
-- ('https://github.com/<owner>/<repo>.git', as constructed by
-- ReserveCheckout/BindRepository/CompleteHandoff) can ever contain. The
-- constraint therefore rejects every legitimately authorized checkout
-- reservation insert. This migration corrects the regex to properly escape
-- the literal dots (a single backslash is sufficient in a non-E'' literal)
-- without altering the intended validation shape.
ALTER TABLE github_checkout_reservations
  DROP CONSTRAINT github_checkout_reservations_clone_url_check;
ALTER TABLE github_checkout_reservations
  ADD CONSTRAINT github_checkout_reservations_clone_url_check
  CHECK (clone_url ~ '^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\.git$');
