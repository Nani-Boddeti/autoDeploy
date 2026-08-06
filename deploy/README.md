# Deployment bootstrap

This directory contains the local/bootstrap skeleton. Production credentials are mounted as
owner-readable files outside the repository; credential values are never supplied through process
environment variables.

Required database configuration:

- `AUTODEPLOY_REPOSITORY_ROOT`: absolute source/workspace root that credential paths must not enter.
- `AUTODEPLOY_DATABASE_URL_FILE`: absolute path to an owner-readable PostgreSQL URL file.
- `AUTODEPLOY_CREDENTIAL_OWNER_UID`: optional expected numeric file owner; defaults to the service
  effective UID.

Password reset also requires an exact 32-byte raw username-throttle key ring:

- `AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_VERSION`
- `AUTODEPLOY_USERNAME_THROTTLE_ACTIVE_KEY_FILE`
- `AUTODEPLOY_USERNAME_THROTTLE_RETAINED_KEY_FILES`: optional comma-separated
  `version=/absolute/path` entries, at most four.

Run migrations with `go run ./cmd/migrate`. Bootstrap or recover the administrator only from an
interactive terminal:

```text
go run ./cmd/admin bootstrap --username administrator
go run ./cmd/admin reset-password --username administrator
```

Credential directories must be trusted and non-writable by group/world. Files must be regular,
owned by the configured UID, owner-readable, and inaccessible to group/world. Rotate by preparing a
complete file and atomically renaming it within the mounted directory; never rewrite in place.
