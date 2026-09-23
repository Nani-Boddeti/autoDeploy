package traefik

import (
	"errors"
	"fmt"
	"regexp"

	"autodeploy/internal/healthgate"
	"autodeploy/internal/runtime/dockerid"
)

// ErrInvalidRoute is wrapped into every error EnvironmentRoute.validate
// returns. Callers distinguish "this input is structurally invalid" from
// any other failure with errors.Is(err, ErrInvalidRoute).
var ErrInvalidRoute = errors.New("traefik: environment route is invalid")

// identifierPattern mirrors internal/runtime/docker's own opaqueIDPattern
// exactly (letters, digits, '-', '_' only, bounded length): every opaque ID
// this package accepts -- environment, release, fence -- must be safe to
// embed in both a Docker-derived name and a YAML scalar without quoting
// tricks. It is redefined here rather than imported because docker's
// pattern is unexported; the two must be kept identical, which is why every
// field validated against it is cross-checked in route_test.go against
// docker's own requireOpaqueID behavior via docker.ContainerName's
// acceptance/rejection (see backend validation below).
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// staticNamePattern validates EntrypointName and CertResolverName: these
// name Traefik *static* configuration objects (entrypoints, ACME resolvers)
// provisioned by host packaging, not database-controlled identifiers, but
// they still flow into this package's rendered YAML, so they are held to
// the same strict, quote-safe charset as everything else this package
// writes -- no field on EnvironmentRoute is exempt from validation.
var staticNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// EnvironmentRoute is the complete, non-secret desired state for one
// environment's Traefik dynamic route document. Every field is either an
// opaque internal identifier, a validated hostname, a validated static
// configuration name, or a bounded numeric/enum value -- there is
// structurally no field capable of carrying environment variable values,
// secret material, or unrestricted user content (ADR 0005: "Route content
// contains no secret values"). Route documents carry their own
// revision/operation metadata rather than Docker labels, mirrored here by
// RouteRevision/ReleaseID/FenceID.
type EnvironmentRoute struct {
	// EnvironmentID is the opaque route_heads.environment_id this document
	// belongs to. It is also this environment's route identity: ADR 0005
	// requires "stable router/middleware names derive from immutable route
	// IDs", and V1 has exactly one route (one hostname) per environment, so
	// EnvironmentID doubles as the route ID for naming purposes.
	EnvironmentID string
	// RouteRevision is route_heads.route_revision, the CAS counter 10a/10c
	// advance on every activation. It is rendered into the document as
	// operation metadata (never used for routing decisions by Traefik
	// itself) so a human or reconciliation process can see which desired
	// revision produced a given file without consulting the database.
	RouteRevision uint64
	// Hostname is the production hostname this route serves. It must
	// already be the canonical, lowercased value 10a's
	// route_heads.hostname stores -- this package does not normalize case,
	// it only validates (see healthgate.ValidRouteHostname).
	Hostname string
	// EntrypointName is the static Traefik entrypoint (e.g. "websecure")
	// this router attaches to. HTTP->HTTPS redirect is static entrypoint
	// configuration, not rendered here -- see this package's doc comment.
	EntrypointName string
	// CertResolverName is the static Traefik ACME certificate resolver
	// name. TLS is always required on the rendered router; there is no way
	// to render a router without a certResolver.
	CertResolverName string
	// BackendRuntimeInstanceID is the candidate release's runtime-instance
	// ID -- the same opaque ID internal/runtime/docker.ContainerName uses
	// to build that container's Docker name. This package derives the
	// backend URL from docker.ContainerName(BackendRuntimeInstanceID)
	// rather than accepting a raw address: see names.go's
	// backendServerURL for why a Docker DNS name is used instead of an IP,
	// and why deriving it through the same validated function 9b's
	// executor already uses for container creation is the safer choice
	// over accepting an arbitrary caller-supplied string.
	BackendRuntimeInstanceID string
	// BackendPort is the candidate container's routed port on the
	// environment network (ADR 0005: "Traefik routes to an explicitly
	// configured container port and scheme"; "Docker's first exposed port
	// is never used as an implicit routing decision").
	BackendPort int
	// BackendScheme is "http" or "https": the scheme Traefik's own
	// loadBalancer server URL uses to reach the candidate container over
	// the environment network. This is independent of the router's own
	// public entrypoint scheme, which is always HTTPS at the edge.
	BackendScheme string
	// ReleaseID is the candidate release's opaque ID. ADR 0005: "Service
	// names include release ID and fence."
	ReleaseID string
	// FenceID is the activation operation's opaque fence ID. Combined with
	// ReleaseID, this makes the service name unique per activation attempt
	// even if a release were somehow re-activated under a new fence.
	FenceID string
}

// validBackendScheme mirrors ADR 0005's "Network and port model": the
// backend scheme is an explicit choice, never inferred.
func validBackendScheme(s string) bool {
	return s == "http" || s == "https"
}

// validate rejects every EnvironmentRoute this package must never render:
// an unvalidated hostname, an identifier outside the strict opaque-ID
// charset, an out-of-range port, or an unsupported scheme. It is called
// first by both Render and (*Writer).Write, so no caller can reach either
// by skipping validation.
func (r EnvironmentRoute) validate() error {
	if !identifierPattern.MatchString(r.EnvironmentID) {
		return fmt.Errorf("%w: environment id %q", ErrInvalidRoute, r.EnvironmentID)
	}
	if !healthgate.ValidRouteHostname(r.Hostname) {
		return fmt.Errorf("%w: hostname %q is not a valid canonical route hostname", ErrInvalidRoute, r.Hostname)
	}
	if !staticNamePattern.MatchString(r.EntrypointName) {
		return fmt.Errorf("%w: entrypoint name %q", ErrInvalidRoute, r.EntrypointName)
	}
	if !staticNamePattern.MatchString(r.CertResolverName) {
		return fmt.Errorf("%w: cert resolver name %q", ErrInvalidRoute, r.CertResolverName)
	}
	if _, err := dockerid.ContainerName(r.BackendRuntimeInstanceID); err != nil {
		return fmt.Errorf("%w: backend runtime instance id %q: %v", ErrInvalidRoute, r.BackendRuntimeInstanceID, err)
	}
	if r.BackendPort < 1 || r.BackendPort > 65535 {
		return fmt.Errorf("%w: backend port %d out of range", ErrInvalidRoute, r.BackendPort)
	}
	if !validBackendScheme(r.BackendScheme) {
		return fmt.Errorf("%w: backend scheme %q", ErrInvalidRoute, r.BackendScheme)
	}
	if !identifierPattern.MatchString(r.ReleaseID) {
		return fmt.Errorf("%w: release id %q", ErrInvalidRoute, r.ReleaseID)
	}
	if !identifierPattern.MatchString(r.FenceID) {
		return fmt.Errorf("%w: fence id %q", ErrInvalidRoute, r.FenceID)
	}
	return nil
}
