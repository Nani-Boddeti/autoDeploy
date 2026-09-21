package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeDocker installs a fake "docker" executable (a POSIX shell
// script) at the front of PATH for the duration of the test, so build()/
// probeBuildx() can be exercised end-to-end with no real Docker daemon or
// buildx installation involved.
func writeFakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// noDockerOnPath points PATH at an empty directory so exec.LookPath finds
// nothing named "docker" at all.
func noDockerOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// testBuildRequest builds a BuildRequest against ctxDir, writing a minimal
// real "Dockerfile" into it: resolveDockerfilePath now requires the
// Dockerfile to actually exist (that existence/no-symlink check is exactly
// what closes the path-traversal/file-disclosure hole), so any test that
// exercises build()/Build() end-to-end needs a real file there.
func testBuildRequest(t *testing.T, ctxDir string) BuildRequest {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write context Dockerfile: %v", err)
	}
	return BuildRequest{
		ContextDir: ctxDir,
		Identity: BuiltImageIdentity{
			OwnerID:          "owner01",
			ProjectID:        "project01",
			SourceDigest:     "sha256:" + strings.Repeat("a", 64),
			BuildOperationID: "buildop01",
		},
	}
}

// --- probeBuildx ---

func TestProbeBuildxMissingDockerFailsClosed(t *testing.T) {
	noDockerOnPath(t)
	err := probeBuildx(context.Background())
	if !errors.Is(err, ErrBuildxUnavailable) {
		t.Fatalf("want ErrBuildxUnavailable, got %v", err)
	}
}

func TestProbeBuildxUnusablePluginFailsClosed(t *testing.T) {
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "docker: 'buildx' is not a docker command" >&2
  exit 1
fi
exit 1
`)
	err := probeBuildx(context.Background())
	if !errors.Is(err, ErrBuildxUnavailable) {
		t.Fatalf("want ErrBuildxUnavailable, got %v", err)
	}
}

func TestProbeBuildxUnrecognizedOutputFailsClosed(t *testing.T) {
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "totally unexpected"
  exit 0
fi
exit 1
`)
	err := probeBuildx(context.Background())
	if !errors.Is(err, ErrBuildxUnavailable) {
		t.Fatalf("want ErrBuildxUnavailable, got %v", err)
	}
}

func TestProbeBuildxSuccess(t *testing.T) {
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0 abcdef"
  exit 0
fi
exit 1
`)
	if err := probeBuildx(context.Background()); err != nil {
		t.Fatalf("probeBuildx: %v", err)
	}
}

// --- build(): fail-closed when buildx is unavailable, never falls back ---

func TestBuildFailsClosedWhenBuildxUnavailable(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "build-ran")
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  exit 1
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  touch "`+sentinel+`"
  exit 0
fi
if [ "$1" = "build" ]; then
  touch "`+sentinel+`"
  exit 0
fi
exit 1
`)
	_, err := build(context.Background(), testBuildRequest(t, dir))
	if !errors.Is(err, ErrBuildxUnavailable) {
		t.Fatalf("want ErrBuildxUnavailable, got %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("build subprocess ran despite unusable buildx: fail-closed violated")
	}
}

// --- BuildScopedBindings: the ADR 0003 build/runtime scope filter ---

func TestBuildScopedBindingsExcludesRuntime(t *testing.T) {
	in := []ScopedSecretBinding{
		{VariableName: "DB_PASSWORD", DeliveryScope: DeliveryScopeRuntime, SecretVersionID: "v1"},
		{VariableName: "NPM_TOKEN", DeliveryScope: DeliveryScopeBuild, SecretVersionID: "v2"},
		{VariableName: "OTHER", DeliveryScope: "bogus", SecretVersionID: "v3"},
	}
	out := BuildScopedBindings(in)
	if len(out) != 1 || out[0].VariableName != "NPM_TOKEN" || out[0].SecretVersionID != "v2" {
		t.Fatalf("runtime/unknown-scope binding leaked into build scope: %#v", out)
	}
}

// --- resolveBuildSecrets ---

func TestResolveBuildSecretsAssignsOpaqueIDs(t *testing.T) {
	bindings := []BuildSecretBinding{
		{VariableName: "NPM_TOKEN", SecretVersionID: "v1"},
		{VariableName: "AWS_KEY", SecretVersionID: "v2"},
	}
	values := []BuildSecretValue{
		{VariableName: "NPM_TOKEN", Value: []byte("supersecret-npm")},
		{VariableName: "AWS_KEY", Value: []byte("supersecret-aws")},
	}
	resolved, err := resolveBuildSecrets(bindings, values)
	if err != nil {
		t.Fatalf("resolveBuildSecrets: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("want 2 resolved secrets, got %d", len(resolved))
	}
	for _, r := range resolved {
		if r.id == r.envVar {
			t.Fatalf("opaque id must not equal variable name, got id=%q envVar=%q", r.id, r.envVar)
		}
		if strings.Contains(r.id, "NPM_TOKEN") || strings.Contains(r.id, "AWS_KEY") {
			t.Fatalf("opaque id leaks variable name: %q", r.id)
		}
	}
	// Sorted by variable name: AWS_KEY before NPM_TOKEN.
	if resolved[0].envVar != "AWS_KEY" || resolved[1].envVar != "NPM_TOKEN" {
		t.Fatalf("expected deterministic sort by variable name, got %+v", resolved)
	}
}

func TestResolveBuildSecretsCountMismatchFails(t *testing.T) {
	bindings := []BuildSecretBinding{{VariableName: "A", SecretVersionID: "v1"}}
	_, err := resolveBuildSecrets(bindings, nil)
	if !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

func TestResolveBuildSecretsEmptyValueFails(t *testing.T) {
	bindings := []BuildSecretBinding{{VariableName: "A", SecretVersionID: "v1"}}
	values := []BuildSecretValue{{VariableName: "A", Value: nil}}
	_, err := resolveBuildSecrets(bindings, values)
	if !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

func TestResolveBuildSecretsInvalidVariableNameFails(t *testing.T) {
	bindings := []BuildSecretBinding{{VariableName: "bad,name", SecretVersionID: "v1"}}
	values := []BuildSecretValue{{VariableName: "bad,name", Value: []byte("x")}}
	_, err := resolveBuildSecrets(bindings, values)
	if !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

// --- buildxArgv: secret values never appear in argv ---

func TestBuildxArgvNeverContainsSecretValue(t *testing.T) {
	labels := map[string]string{"io.autodeploy.owner-id": "owner01"}
	resolved := []resolvedBuildSecret{
		{id: opaqueSecretID(0), envVar: "AWS_KEY", value: []byte("supersecret-aws-value")},
	}
	argv, env := buildxArgv("Dockerfile", labels, resolved, "/tmp/meta.json")
	for _, a := range argv {
		if strings.Contains(a, "supersecret-aws-value") {
			t.Fatalf("secret value leaked into argv: %q", a)
		}
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--secret id=build-secret-0,env=AWS_KEY") {
		t.Fatalf("missing expected --secret flag shape, argv: %v", argv)
	}
	if len(env) != 1 || env[0] != "AWS_KEY=supersecret-aws-value" {
		t.Fatalf("secret value must travel only via env, got %v", env)
	}
	// No --build-arg, no ARG/ENV Dockerfile injection vector, no context-file path.
	for _, a := range argv {
		if strings.Contains(a, "--build-arg") {
			t.Fatalf("argv must never use --build-arg for secrets: %v", argv)
		}
	}
}

// --- parseBuildMetadata ---

func TestParseBuildMetadataValid(t *testing.T) {
	data := []byte(`{"containerimage.digest":"sha256:` + strings.Repeat("a", 64) + `","containerimage.config.digest":"sha256:` + strings.Repeat("b", 64) + `"}`)
	result, err := parseBuildMetadata(data)
	if err != nil {
		t.Fatalf("parseBuildMetadata: %v", err)
	}
	if result.ImageDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("unexpected image digest: %s", result.ImageDigest)
	}
	if result.ConfigDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("unexpected config digest: %s", result.ConfigDigest)
	}
}

func TestParseBuildMetadataRejectsInvalidDigest(t *testing.T) {
	data := []byte(`{"containerimage.digest":"not-a-digest","containerimage.config.digest":"sha256:` + strings.Repeat("b", 64) + `"}`)
	if _, err := parseBuildMetadata(data); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("want ErrBuildFailed for invalid digest, got %v", err)
	}
}

func TestParseBuildMetadataRejectsMissingField(t *testing.T) {
	data := []byte(`{"containerimage.digest":"sha256:` + strings.Repeat("a", 64) + `"}`)
	if _, err := parseBuildMetadata(data); !errors.Is(err, ErrBuildFailed) {
		t.Fatalf("want ErrBuildFailed for missing config digest, got %v", err)
	}
}

// --- end-to-end build() against a fake buildx, including redaction ---

type fakeMaterializer struct {
	values []BuildSecretValue
	err    error
}

func (f fakeMaterializer) MaterializeBuildSecrets(context.Context, []BuildSecretBinding) ([]BuildSecretValue, error) {
	return f.values, f.err
}

func TestBuildEndToEndRedactsLeakedSecretFromOutput(t *testing.T) {
	ctxDir := t.TempDir()
	imageDigest := "sha256:" + strings.Repeat("1", 64)
	configDigest := "sha256:" + strings.Repeat("2", 64)
	// The fake buildx deliberately echoes the secret value to stdout, as if
	// a misbehaving build leaked it into build output, so this test proves
	// the agent's own redaction wiring -- not BuildKit -- is what keeps the
	// plaintext out of what leaves the process.
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  echo "leaked-secret-in-log: $NPM_TOKEN"
  meta=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--metadata-file" ]; then meta="$a"; fi
    prev="$a"
  done
  printf '{"containerimage.digest":"`+imageDigest+`","containerimage.config.digest":"`+configDigest+`"}' > "$meta"
  exit 0
fi
exit 1
`)
	var stdout, stderr bytes.Buffer
	req := testBuildRequest(t, ctxDir)
	req.Stdout = &stdout
	req.Stderr = &stderr
	req.SecretBindings = []BuildSecretBinding{{VariableName: "NPM_TOKEN", SecretVersionID: "sv1"}}
	req.Materializer = fakeMaterializer{values: []BuildSecretValue{{VariableName: "NPM_TOKEN", Value: []byte("supersecret-npm-token-value")}}}

	result, err := build(context.Background(), req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.ImageDigest != imageDigest || result.ConfigDigest != configDigest {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(stdout.String(), "supersecret-npm-token-value") {
		t.Fatalf("secret value leaked into redacted stdout: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("expected redaction marker in stdout, got %q", stdout.String())
	}
}

func TestBuildRejectsSecretBindingsWithoutMaterializer(t *testing.T) {
	ctxDir := t.TempDir()
	req := testBuildRequest(t, ctxDir)
	req.SecretBindings = []BuildSecretBinding{{VariableName: "A", SecretVersionID: "v1"}}
	_, err := build(context.Background(), req)
	if !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

func TestBuildRedactorConstructionFailureSuppressesOutput(t *testing.T) {
	ctxDir := t.TempDir()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "build-ran")
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  touch "`+sentinel+`"
  exit 0
fi
exit 1
`)
	// Oversized secret value forces crypto.NewRedactingWriter's internal
	// redactionPatterns() to fail (MaxRedactionRawBytes), which must
	// suppress output rather than let it through raw.
	oversized := bytes.Repeat([]byte("x"), 9<<20)
	var stdout bytes.Buffer
	req := testBuildRequest(t, ctxDir)
	req.Stdout = &stdout
	req.SecretBindings = []BuildSecretBinding{{VariableName: "HUGE", SecretVersionID: "v1"}}
	req.Materializer = fakeMaterializer{values: []BuildSecretValue{{VariableName: "HUGE", Value: oversized}}}

	_, err := build(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error from redactor construction failure")
	}
	if stdout.Len() != 0 {
		t.Fatalf("output must be suppressed on redactor construction failure, got %d bytes", stdout.Len())
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("build subprocess ran despite redactor construction failure: output could have leaked raw")
	}
}

func TestValidateBuildRequestRejectsUnusableContextDir(t *testing.T) {
	req := BuildRequest{ContextDir: filepath.Join(t.TempDir(), "does-not-exist")}
	if err := validateBuildRequest(req); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

func TestValidateBuildRequestRejectsDuplicateSecretBindings(t *testing.T) {
	req := testBuildRequest(t, t.TempDir())
	req.Materializer = fakeMaterializer{}
	req.SecretBindings = []BuildSecretBinding{
		{VariableName: "A", SecretVersionID: "v1"},
		{VariableName: "A", SecretVersionID: "v2"},
	}
	if err := validateBuildRequest(req); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest, got %v", err)
	}
}

// --- resolveDockerfilePath: no arbitrary host file may be read as a Dockerfile ---

func TestResolveDockerfilePathRejectsTraversal(t *testing.T) {
	// A real, existing target outside the context: a coincidentally
	// nonexistent /etc/passwd-shaped path under a tmp directory would pass
	// this test even with the escape check removed (Lstat just fails "not
	// found" either way), so the target here must genuinely exist one
	// level above the context.
	root := t.TempDir()
	ctxDir := filepath.Join(root, "ctx")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatalf("mkdir ctx: %v", err)
	}
	secretFile := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("outside content\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if _, err := resolveDockerfilePath(ctxDir, "../secret.txt"); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest for traversal, got %v", err)
	}
}

func TestResolveDockerfilePathRejectsAbsolutePath(t *testing.T) {
	ctxDir := t.TempDir()
	if _, err := resolveDockerfilePath(ctxDir, "/etc/passwd"); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest for absolute path, got %v", err)
	}
}

func TestResolveDockerfilePathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	ctxDir := filepath.Join(root, "ctx")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatalf("mkdir ctx: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	secretFile := filepath.Join(outside, "Dockerfile")
	if err := os.WriteFile(secretFile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// Dockerfile is a symlink pointing outside the context.
	link := filepath.Join(ctxDir, "Dockerfile")
	if err := os.Symlink(secretFile, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveDockerfilePath(ctxDir, "Dockerfile"); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest for symlinked dockerfile, got %v", err)
	}
}

func TestResolveDockerfilePathRejectsSymlinkedParentDirectory(t *testing.T) {
	root := t.TempDir()
	ctxDir := filepath.Join(root, "ctx")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		t.Fatalf("mkdir ctx: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// A subdirectory of the context is itself a symlink to somewhere else.
	subLink := filepath.Join(ctxDir, "sub")
	if err := os.Symlink(outside, subLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveDockerfilePath(ctxDir, "sub/Dockerfile"); !errors.Is(err, ErrInvalidBuildRequest) {
		t.Fatalf("want ErrInvalidBuildRequest for symlinked parent directory, got %v", err)
	}
}

func TestResolveDockerfilePathAcceptsOrdinaryNestedFile(t *testing.T) {
	ctxDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ctxDir, "docker"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "docker", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	resolved, err := resolveDockerfilePath(ctxDir, "docker/Dockerfile")
	if err != nil {
		t.Fatalf("resolveDockerfilePath: %v", err)
	}
	if resolved != filepath.Join("docker", "Dockerfile") {
		t.Fatalf("unexpected resolved path: %q", resolved)
	}
}

// --- resolveBuildSecrets: reserved env var names ---

func TestResolveBuildSecretsRejectsReservedEnvVar(t *testing.T) {
	for name := range reservedSecretEnvVars {
		bindings := []BuildSecretBinding{{VariableName: name, SecretVersionID: "v1"}}
		values := []BuildSecretValue{{VariableName: name, Value: []byte("x")}}
		if _, err := resolveBuildSecrets(bindings, values); !errors.Is(err, ErrInvalidBuildRequest) {
			t.Fatalf("variable %q: want ErrInvalidBuildRequest, got %v", name, err)
		}
	}
}

// --- subprocessEnv: the parent process's own environment must not leak in ---

func TestSubprocessEnvDoesNotLeakArbitraryParentVars(t *testing.T) {
	t.Setenv("AUTODEPLOY_DATABASE_URL", "postgres://user:sentinel-password@host/db")
	env := subprocessEnv([]string{"AWS_KEY=value"})
	for _, e := range env {
		if strings.Contains(e, "sentinel-password") {
			t.Fatalf("parent environment leaked into subprocess env: %q", e)
		}
		if strings.HasPrefix(e, "AUTODEPLOY_DATABASE_URL=") {
			t.Fatalf("non-allowlisted variable reached subprocess env: %q", e)
		}
	}
}

func TestSubprocessEnvIncludesAllowlistedVars(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	env := subprocessEnv(nil)
	found := false
	for _, e := range env {
		if e == "PATH=/usr/bin:/bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected allowlisted PATH to reach subprocess env, got %v", env)
	}
}

func TestBuildEndToEndDoesNotLeakParentEnvToSubprocess(t *testing.T) {
	ctxDir := t.TempDir()
	dir := t.TempDir()
	captured := filepath.Join(dir, "env.txt")
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  env > "`+captured+`"
  meta=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--metadata-file" ]; then meta="$a"; fi
    prev="$a"
  done
  printf '{"containerimage.digest":"sha256:`+strings.Repeat("3", 64)+`","containerimage.config.digest":"sha256:`+strings.Repeat("4", 64)+`"}' > "$meta"
  exit 0
fi
exit 1
`)
	t.Setenv("AUTODEPLOY_DATABASE_URL", "postgres://user:sentinel-password@host/db")
	req := testBuildRequest(t, ctxDir)
	if _, err := build(context.Background(), req); err != nil {
		t.Fatalf("build: %v", err)
	}
	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured env: %v", err)
	}
	if strings.Contains(string(data), "sentinel-password") {
		t.Fatalf("parent environment leaked into the build subprocess: %s", data)
	}
	if strings.Contains(string(data), "AUTODEPLOY_DATABASE_URL") {
		t.Fatalf("non-allowlisted variable reached the build subprocess: %s", data)
	}
}

// --- redaction wiring: stderr twin of the stdout leak test ---

func TestBuildEndToEndRedactsLeakedSecretFromStderr(t *testing.T) {
	ctxDir := t.TempDir()
	imageDigest := "sha256:" + strings.Repeat("5", 64)
	configDigest := "sha256:" + strings.Repeat("6", 64)
	writeFakeDocker(t, `
if [ "$1" = "buildx" ] && [ "$2" = "version" ]; then
  echo "github.com/docker/buildx v0.17.0"
  exit 0
fi
if [ "$1" = "buildx" ] && [ "$2" = "build" ]; then
  echo "leaked-secret-in-log: $NPM_TOKEN" >&2
  meta=""
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--metadata-file" ]; then meta="$a"; fi
    prev="$a"
  done
  printf '{"containerimage.digest":"`+imageDigest+`","containerimage.config.digest":"`+configDigest+`"}' > "$meta"
  exit 0
fi
exit 1
`)
	var stdout, stderr bytes.Buffer
	req := testBuildRequest(t, ctxDir)
	req.Stdout = &stdout
	req.Stderr = &stderr
	req.SecretBindings = []BuildSecretBinding{{VariableName: "NPM_TOKEN", SecretVersionID: "sv1"}}
	req.Materializer = fakeMaterializer{values: []BuildSecretValue{{VariableName: "NPM_TOKEN", Value: []byte("supersecret-npm-token-value")}}}

	if _, err := build(context.Background(), req); err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(stderr.String(), "supersecret-npm-token-value") {
		t.Fatalf("secret value leaked into redacted stderr: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "[REDACTED]") {
		t.Fatalf("expected redaction marker in stderr, got %q", stderr.String())
	}
}

// --- platform-gated stub sanity (compiles + behaves the same on darwin) ---

func TestBuildUnsupportedOrLinuxWrapperMatchesUnexported(t *testing.T) {
	// On darwin this exercises build_unsupported.go's fail-closed stub
	// (ErrUnsupportedPlatform); on linux it exercises build_linux.go's thin
	// wrapper around build(), which with no docker on PATH fails with
	// ErrBuildxUnavailable. The exported entry points must never diverge
	// from the platform's intended behavior, and the assertion is pinned to
	// the specific expected sentinel per platform rather than "some error",
	// so a wrapper that silently changed behavior would be caught.
	noDockerOnPath(t)
	_, err := Build(context.Background(), testBuildRequest(t, t.TempDir()))
	if runtime.GOOS == "linux" {
		if !errors.Is(err, ErrBuildxUnavailable) {
			t.Fatalf("want ErrBuildxUnavailable on linux, got %v", err)
		}
		return
	}
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("want ErrUnsupportedPlatform on %s, got %v", runtime.GOOS, err)
	}
}
