package traefik

// DefaultEntrypointName and DefaultCertResolverName are ADR 0005's V1 fixed
// Traefik static-configuration identifiers (see EnvironmentRoute's
// EntrypointName/CertResolverName doc comments in route.go): V1 runs one
// Traefik instance per host with exactly one entrypoint and one ACME
// resolver, system-wide -- there is no per-environment or per-server
// selection in this slice.
//
// Step 10c's control plane and (a later slice's) agent both need this exact
// pair of names to agree, because the control plane renders and digests the
// route document in tx1 (see internal/store/postgres's Activate) and the
// agent re-renders the same EnvironmentRoute independently before writing
// it, then refuses to write on a digest mismatch. Rather than configuring
// the same two strings independently on each side of the control-plane/
// agent boundary and checking them for equality at startup (a design that
// still allows a window where a stale config briefly disagrees), both sides
// import these same Go constants: a single source of truth that cannot
// drift, because there is only one copy of the value to begin with. If a
// future slice needs per-server or per-environment entrypoints/resolvers,
// this becomes a real, database-backed configuration column then; V1 has no
// stated requirement for that.
const (
	DefaultEntrypointName   = "websecure"
	DefaultCertResolverName = "letsencrypt-production"
)
