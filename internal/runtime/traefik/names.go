package traefik

import (
	"fmt"

	"autodeploy/internal/runtime/docker"
)

// Naming prefixes. Every derived name is namespaced so it cannot collide
// with an operator's own unrelated Traefik configuration, mirroring
// internal/runtime/docker's containerNamePrefix convention.
const (
	routerNamePrefix  = "autodeploy-router-"
	serviceNamePrefix = "autodeploy-svc-"
	routeFilePrefix   = "environment-"
	// routeFileExtension is the provider-recognized extension Traefik's
	// file provider watches for. routeTempFileExtension (write.go) is
	// deliberately different and not a provider-recognized extension, per
	// ADR 0005.
	routeFileExtension = ".yml"
)

// routerName derives the router name from EnvironmentID alone (ADR 0005:
// "Stable router/middleware names derive from immutable route IDs"; V1 has
// one route per environment, so EnvironmentID is the route ID).
func routerName(r EnvironmentRoute) string {
	return routerNamePrefix + r.EnvironmentID
}

// serviceName derives the service name from EnvironmentID, ReleaseID, and
// FenceID (ADR 0005: "Service names include release ID and fence"). It
// additionally includes EnvironmentID so two environments activating the
// same release ID under different fences can never collide in one shared
// per-environment file -- not required by the ADR text alone, but this
// package renders one file per environment, so scoping every name in it by
// that environment costs nothing and removes a cross-environment collision
// class entirely.
func serviceName(r EnvironmentRoute) string {
	return serviceNamePrefix + r.EnvironmentID + "-" + r.ReleaseID + "-" + r.FenceID
}

// routeFileName derives this environment's route document's final,
// provider-recognized file name from its opaque EnvironmentID alone --
// never from a repository/app/hostname string, per ADR 0005's "Immutable
// identity, names, and labels".
func routeFileName(environmentID string) string {
	return routeFilePrefix + environmentID + routeFileExtension
}

// backendServerURL builds the loadBalancer server URL Traefik uses to reach
// the candidate container: <scheme>://<container-name>:<port>.
//
// This uses the container's Docker DNS name, not its environment-network
// IP address, for two reasons tied directly to ADR 0005:
//
//   - "Traefik routes to an explicitly configured container port and
//     scheme using a unique internal Docker DNS name" -- the ADR names the
//     mechanism explicitly; an IP address is not a Docker DNS name.
//   - A container's environment-network IP is not stable across a
//     recreate (Step 9b's health gate resolves it fresh via InspectContainer
//     immediately before probing, specifically because it does not commit
//     to that IP being reusable); Docker's embedded DNS on a user-defined
//     bridge network resolves the container's *name* to its current IP on
//     every lookup, so a route file written once at activation time keeps
//     resolving correctly even if Docker ever re-assigns the container's
//     IP without recreating the container. Traefik joins each environment
//     network it routes (ADR 0005 "Network and port model"), so it can
//     resolve that name exactly as any other container on the same network
//     can.
//
// The name is derived through docker.ContainerName itself, the same
// collision-checked function 9b's executor uses to create the container,
// rather than duplicating its prefix/charset here -- this is a hard
// guarantee that this package can never name a backend Traefik will never
// find, and any future change to that naming scheme cannot silently
// desynchronize the two packages.
func backendServerURL(r EnvironmentRoute) (string, error) {
	name, err := docker.ContainerName(r.BackendRuntimeInstanceID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s:%d", r.BackendScheme, name, r.BackendPort), nil
}
