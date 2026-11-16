package docker

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildxArgvEmitsSortedBuildArgsForLiterals(t *testing.T) {
	lits, err := resolveBuildLiterals([]BuildLiteral{
		{VariableName: "ZED", Value: "last"},
		{VariableName: "ALPHA", Value: "a b=c"},
	}, nil)
	if err != nil {
		t.Fatalf("resolveBuildLiterals: %v", err)
	}
	argv, env := buildxArgv("Dockerfile", nil, nil, lits, "/tmp/meta.json")
	var got []string
	for i, a := range argv {
		if a == "--build-arg" {
			got = append(got, argv[i+1])
		}
	}
	want := []string{"ALPHA=a b=c", "ZED=last"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("build args = %v, want %v", got, want)
	}
	for _, e := range env {
		if strings.Contains(e, "last") || strings.Contains(e, "a b=c") {
			t.Fatalf("literal leaked into subprocess env: %q", e)
		}
	}
}

func TestBuildxArgvWithoutLiteralsHasNoBuildArg(t *testing.T) {
	argv, _ := buildxArgv("Dockerfile", nil, nil, nil, "/tmp/meta.json")
	for _, a := range argv {
		if a == "--build-arg" {
			t.Fatalf("unexpected --build-arg: %v", argv)
		}
	}
}

func TestResolveBuildLiteralsRefusesReservedAndBuiltInNames(t *testing.T) {
	names := []string{"BUILDKIT_INLINE_CACHE", "buildkit_x", "BUILDX_BUILDER", "BUILDX_GIT_INFO", "HTTP_PROXY", "https_proxy", "NO_PROXY", "TARGETPLATFORM", "BUILDARCH", "TARGETVARIANT", "PATH", "DOCKER_HOST"}
	for name := range reservedSecretEnvVars {
		names = append(names, name)
	}
	for _, name := range names {
		_, err := resolveBuildLiterals([]BuildLiteral{{VariableName: name, Value: "v"}}, nil)
		if !errors.Is(err, ErrInvalidBuildRequest) || !errors.Is(err, ErrInvalidBuildLiteral) {
			t.Fatalf("name %q: want ErrInvalidBuildLiteral, got %v", name, err)
		}
		if strings.Contains(err.Error(), "\"v\"") {
			t.Fatalf("error must not echo the value: %v", err)
		}
	}
}

func TestResolveBuildLiteralsRefusesInvalidAndDuplicateNames(t *testing.T) {
	for _, name := range []string{"", "1A", "A-B", "A=B", "A B", strings.Repeat("A", 129)} {
		if _, err := resolveBuildLiterals([]BuildLiteral{{VariableName: name, Value: "v"}}, nil); !errors.Is(err, ErrInvalidBuildLiteral) {
			t.Fatalf("name %q: want ErrInvalidBuildLiteral, got %v", name, err)
		}
	}
	if _, err := resolveBuildLiterals([]BuildLiteral{{VariableName: "A", Value: "1"}, {VariableName: "A", Value: "2"}}, nil); !errors.Is(err, ErrInvalidBuildLiteral) {
		t.Fatalf("duplicate: want ErrInvalidBuildLiteral, got %v", err)
	}
}

func TestResolveBuildLiteralsRefusesSecretNameClash(t *testing.T) {
	_, err := resolveBuildLiterals(
		[]BuildLiteral{{VariableName: "TOKEN", Value: "v"}},
		[]BuildSecretBinding{{VariableName: "TOKEN", SecretVersionID: "v1"}},
	)
	if !errors.Is(err, ErrInvalidBuildLiteral) {
		t.Fatalf("want ErrInvalidBuildLiteral, got %v", err)
	}
}

func TestValidateBuildRequestRefusesLiteralProblems(t *testing.T) {
	req := testBuildRequest(t, t.TempDir())
	req.Literals = []BuildLiteral{{VariableName: "BUILDKIT_X", Value: "v"}}
	if err := validateBuildRequest(req); !errors.Is(err, ErrInvalidBuildLiteral) {
		t.Fatalf("reserved: want ErrInvalidBuildLiteral, got %v", err)
	}
	req.Materializer = fakeMaterializer{}
	req.SecretBindings = []BuildSecretBinding{{VariableName: "TOKEN", SecretVersionID: "v1"}}
	req.Literals = []BuildLiteral{{VariableName: "TOKEN", Value: "v"}}
	if err := validateBuildRequest(req); !errors.Is(err, ErrInvalidBuildLiteral) {
		t.Fatalf("clash: want ErrInvalidBuildLiteral, got %v", err)
	}
	req.Literals = []BuildLiteral{{VariableName: "OK_NAME", Value: "v"}}
	if err := validateBuildRequest(req); err != nil {
		t.Fatalf("valid literal refused: %v", err)
	}
}

func TestClassifyBuildLiteralRefusalIsPermanentConfiguration(t *testing.T) {
	_, err := resolveBuildLiterals([]BuildLiteral{{VariableName: "BUILDX_X", Value: "v"}}, nil)
	if got := string(ClassifyFailure(err)); got != "permanent_configuration" {
		t.Fatalf("class = %q", got)
	}
}
