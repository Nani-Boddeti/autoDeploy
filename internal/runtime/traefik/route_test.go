package traefik

import (
	"errors"
	"strings"
	"testing"
)

func validRoute() EnvironmentRoute {
	return EnvironmentRoute{
		EnvironmentID:            "env01ABCxyz",
		RouteRevision:            7,
		Hostname:                 "app.example.com",
		EntrypointName:           "websecure",
		CertResolverName:         "letsencrypt",
		BackendRuntimeInstanceID: "rti01ABCxyz",
		BackendPort:              8080,
		BackendScheme:            "http",
		ReleaseID:                "rel01ABCxyz",
		FenceID:                  "fen01ABCxyz",
	}
}

func TestEnvironmentRouteValidateAccepts(t *testing.T) {
	if err := validRoute().validate(); err != nil {
		t.Fatalf("expected valid route to pass, got %v", err)
	}
}

func TestEnvironmentRouteHostnameRejectsBacktick(t *testing.T) {
	r := validRoute()
	r.Hostname = "app.example`.com"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for backtick hostname, got %v", err)
	}
}

func TestEnvironmentRouteHostnameRejectsSpace(t *testing.T) {
	r := validRoute()
	r.Hostname = "app example.com"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for space in hostname, got %v", err)
	}
}

func TestEnvironmentRouteHostnameRejectsNewline(t *testing.T) {
	r := validRoute()
	r.Hostname = "app.example.com\nHost(`evil.com`)"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for newline in hostname, got %v", err)
	}
}

func TestEnvironmentRouteHostnameRejectsUppercase(t *testing.T) {
	r := validRoute()
	r.Hostname = "App.Example.com"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for uppercase hostname, got %v", err)
	}
}

func TestEnvironmentRouteHostnameRejectsSingleLabel(t *testing.T) {
	r := validRoute()
	r.Hostname = "localhost"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for single-label hostname, got %v", err)
	}
}

func TestEnvironmentRouteIdentifierRejectsSlash(t *testing.T) {
	r := validRoute()
	r.EnvironmentID = "env/../etc"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for slash in environment id, got %v", err)
	}
}

func TestEnvironmentRouteIdentifierRejectsDotDot(t *testing.T) {
	r := validRoute()
	r.ReleaseID = ".."
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for '..' release id, got %v", err)
	}
}

func TestEnvironmentRouteIdentifierRejectsEmpty(t *testing.T) {
	for _, mutate := range []func(*EnvironmentRoute){
		func(r *EnvironmentRoute) { r.EnvironmentID = "" },
		func(r *EnvironmentRoute) { r.ReleaseID = "" },
		func(r *EnvironmentRoute) { r.FenceID = "" },
		func(r *EnvironmentRoute) { r.BackendRuntimeInstanceID = "" },
		func(r *EnvironmentRoute) { r.EntrypointName = "" },
		func(r *EnvironmentRoute) { r.CertResolverName = "" },
		func(r *EnvironmentRoute) { r.Hostname = "" },
	} {
		r := validRoute()
		mutate(&r)
		if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
			t.Fatalf("expected ErrInvalidRoute for empty field, got %v", err)
		}
	}
}

func TestEnvironmentRouteBackendPortOutOfRange(t *testing.T) {
	for _, port := range []int{0, -1, 65536, 100000} {
		r := validRoute()
		r.BackendPort = port
		if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
			t.Fatalf("port %d: expected ErrInvalidRoute, got %v", port, err)
		}
	}
}

func TestEnvironmentRouteBackendSchemeRejectsUnsupported(t *testing.T) {
	for _, scheme := range []string{"ftp", "HTTP", "", "http://"} {
		r := validRoute()
		r.BackendScheme = scheme
		if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
			t.Fatalf("scheme %q: expected ErrInvalidRoute, got %v", scheme, err)
		}
	}
}

func TestEnvironmentRouteEntrypointNameRejectsInjection(t *testing.T) {
	r := validRoute()
	r.EntrypointName = "websecure\"\n  evil: true"
	if err := r.validate(); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute for injection-shaped entrypoint name, got %v", err)
	}
}

// TestRouteHostnameCharsetExcludesYAMLAndTraefikMetacharacters proves the
// specific structural claim ruleValue's doc comment relies on: the accepted
// route hostname charset can never contain a backtick, quote, parenthesis,
// or whitespace/control byte, so no valid EnvironmentRoute.Hostname can
// ever break out of either the Host(`...`) rule template or the
// surrounding YAML double-quoted scalar.
func TestRouteHostnameCharsetExcludesYAMLAndTraefikMetacharacters(t *testing.T) {
	forbidden := "`\"'()\\\t\n\r "
	r := validRoute()
	base := r.Hostname
	for _, c := range forbidden {
		candidate := base[:3] + string(c) + base[3:]
		rr := r
		rr.Hostname = candidate
		if err := rr.validate(); err == nil {
			t.Fatalf("hostname containing %q unexpectedly accepted: %q", c, candidate)
		}
	}
	if strings.ContainsAny(base, forbidden) {
		t.Fatalf("test fixture hostname itself contains a forbidden byte: %q", base)
	}
}
