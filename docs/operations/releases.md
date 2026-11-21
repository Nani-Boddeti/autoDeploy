# Signed releases

Plan 24-A. A tag `vMAJOR.MINOR.PATCH[-pre]` on a commit reachable from `main` runs
`.github/workflows/release.yml`: it builds static git (fresh, no cache; the git source tarball is
hash-pinned in `static-git/Dockerfile`) and the host binaries (`--gates disabled`), packages them,
writes `SHA256SUMS` over every asset, signs it with `ssh-keygen -Y sign` (namespace
`autodeploy-release`, principal `release@autodeploy`), re-verifies the downloaded artifact against the
committed public key, and publishes a GitHub Release.

Assets: `autodeploy-host-kit-<version>.tar.gz` (a `git archive` of the tagged commit restricted to an
allowlist in `package-release.sh`: install/preflight/enroll/uninstall scripts, `stage-lib.sh`, systemd
units, traefik config, libexec, README; NOT tests, `*.bak*`, `static-git/`, `build-*.sh`,
`smoke.env.example`, `package-release.sh`, and NOT `verify-release.sh` or `release-signing/`),
`autodeploy-binaries-<version>.tar.gz` (`binaries/` = the `BINDIR` for `install-*.sh`, with its own
`SHA256SUMS`), `VERSION` (the tag, signed), `SHA256SUMS`, `SHA256SUMS.sig`. `install.sh` is never a
release asset: it is fetched from the git tag ref (plan 24-B).
`gitcheckout.test` is in the binaries tarball because `build-host-binaries.sh` emits it; the installers
and units do not use it (only `diagnose.sh`/`smoke.env.example` reference it for host proofs).

**Release builds are gate-disabled** (`executorEnabled` / `activationEnabled` stay false) until the gate
flip in the VPS round (decision D5). Do not cut a production release before then.

## Trust model

- The trust anchor is the public key in `deploy/host/release-signing/allowed_signers` (canonical
  source in the repo; 24-B's `install.sh` embeds these lines literally). It must never be taken from
  the downloaded release or the kit: the kit does not contain it, and `verify-release.sh` has no default
  for `--allowed-signers`.
- Residual risk: write access to the repo (and its tags) is the root of trust, because `install.sh` is
  fetched from a git ref. Protect `main` and `v*` tags (below).
- Publish the key fingerprint out of band (README, release notes, your own site):
  `ssh-keygen -lf ~/autodeploy-release-key.pub` prints `SHA256:...`; `install.sh` prints the same
  fingerprint so an operator can compare it.

## One-time owner setup

Do this on a trusted machine, not in the repo directory.

```bash
# 1. Generate the key. Passphrase-less on purpose (CI must use it unattended), so the private key
#    is only as safe as the GitHub environment secret and your offline backup. Never commit it.
ssh-keygen -t ed25519 -C release@autodeploy -N '' -f ~/autodeploy-release-key
# 2. GitHub: Settings > Environments > New environment `release`: add yourself as Required reviewer,
#    deployment branches and tags = selected, add tag pattern `v*`. Then store the key as an
#    ENVIRONMENT secret (not a repo secret):
gh secret set AUTODEPLOY_RELEASE_SIGNING_KEY --env release --repo Nani-Boddeti/autoDeploy < ~/autodeploy-release-key
# 3. Commit the PUBLIC key: replace the PLACEHOLDER line in deploy/host/release-signing/allowed_signers with
printf 'release@autodeploy namespaces="autodeploy-release" %s\n' "$(cut -d' ' -f1,2 ~/autodeploy-release-key.pub)"
# 4. Fingerprint to publish (README / release notes / your site):
ssh-keygen -lf ~/autodeploy-release-key.pub
# 5. Back up ~/autodeploy-release-key OFFLINE (encrypted USB / password manager). Only after you have
#    confirmed the backup can be read back, delete the local copy.
```

Also in GitHub (Settings): a tag ruleset on `v*` that restricts who can create tags and blocks update
and deletion; branch protection on `main` (PRs, no force push). Remove any repo-level secret of the same
name so only the environment holds it.

The workflow fails closed: no secret, a placeholder `allowed_signers`, or a secret that does not match
the committed public key means no release. A dry run never reads the secret.

## Cut a release

```bash
git tag v0.1.0 && git push origin v0.1.0   # build, then the `release` environment waits for your approval
```

Dry run (builds, signs with a throwaway key generated in the job, uploads artifact `dry-run-assets`,
publishes nothing, never touches the real secret): Actions, Release, Run workflow, keep `dry_run`
ticked. The dry-run signature is not verifiable against the real key by design.

## Reviewer checklist (before approving the `release` environment)

The approval is what lets the signing key run, so check what is about to be signed:

```bash
git fetch --tags origin main
git merge-base --is-ancestor <tag>^{commit} origin/main && echo "on main" || echo "NOT on main: reject"
prev=$(git describe --tags --abbrev=0 --match 'v*' <tag>^)   # previous release tag
git log --oneline "$prev"..<tag>
git diff "$prev"..<tag> -- .github/workflows deploy/host
```

Read every change to workflows and `deploy/host`. Reject if anything is unexplained.

## Verify a downloaded release

```bash
deploy/host/verify-release.sh DIR --allowed-signers FILE --expect-version vX.Y.Z
```

Both options are mandatory (`install.sh` passes its embedded key and the tag it was fetched from).
Refuses: missing or bad signature, wrong key or namespace, the PLACEHOLDER key, a `DIR` that is group or
world writable or owned by another user, a listed file missing, an unlisted file, symlinks,
subdirectories, malformed/duplicate/self-listing `SHA256SUMS`, checksum mismatch, version mismatch.
`VERIFY_DEBUG=1` shows ssh-keygen's error output. Exit 0 only when everything passes.

## Rotate the key

Never replace the line in place: hosts and installers that still embed the old key would reject the new
release (and a compromised-key swap would be invisible).
1. Generate the new key, `gh secret set ... --env release`, and ADD its line to `allowed_signers`
   (both lines active). Publish the new fingerprint.
2. Cut a release signed with the new key. Installers carrying both lines accept either.
3. Later, once nothing depends on the old key, remove its line in a reviewed commit.
4. If the old key may be compromised, remove it immediately and say so in the release notes.

## Notes

- Reproducibility: the kit (`git archive`) is deterministic everywhere; the binaries tarball is
  byte-reproducible with GNU tar only (CI), on macOS only content-identical.
