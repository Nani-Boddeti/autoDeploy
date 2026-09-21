package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"autodeploy/internal/crypto"
)

// Delivery scopes, mirroring the CHECK constraints on delivery_scope in
// migrations 000004, 000014, and 000015.
const (
	DeliveryScopeBuild   = "build"
	DeliveryScopeRuntime = "runtime"
)

var (
	// ErrBuildxUnavailable is returned when the buildx capability probe
	// fails: the docker CLI is missing, or the buildx plugin is missing or
	// unusable. The plan calls for this to fail closed at enrollment
	// (startup probe) and again at build time, never falling back to a
	// legacy `docker build`, which has no --secret support and would leak
	// build secrets into ARG/ENV/layers.
	ErrBuildxUnavailable = errors.New("docker buildx: unavailable")
	// ErrBuildFailed wraps a non-zero buildx exit or an unusable metadata
	// result.
	ErrBuildFailed = errors.New("docker buildx: build failed")
	// ErrInvalidBuildRequest is returned for a structurally invalid
	// BuildRequest: this is checked before any subprocess is started or any
	// secret is materialized.
	ErrInvalidBuildRequest = errors.New("docker buildx: invalid build request")
)

// ScopedSecretBinding is the minimal shape of one frozen secret binding
// this package needs to decide whether it may ever reach a build: variable
// name, its declared delivery scope, and the opaque secret-version
// reference ADR 0003 allows onto the wire (internal/agent/http/claim.go's
// secretBindingView). It never carries a value.
type ScopedSecretBinding struct {
	VariableName    string
	DeliveryScope   string
	SecretVersionID string
}

// BuildSecretBinding is a secret binding cleared for build use. It has no
// scope field at all: BuildScopedBindings is the only constructor for this
// type in the package, so nothing downstream can be handed a binding
// without having passed through the build/runtime filter, and a value that
// reaches this type structurally cannot carry a stale or wrong scope
// because there is nowhere on the type to put one.
type BuildSecretBinding struct {
	VariableName    string
	SecretVersionID string
}

// BuildScopedBindings filters bindings to build-scoped ones only. A
// runtime-scoped binding is dropped here, not merely ignored later: this is
// the single choke point ADR 0003 requires so a runtime secret can never
// reach the build path.
func BuildScopedBindings(bindings []ScopedSecretBinding) []BuildSecretBinding {
	out := make([]BuildSecretBinding, 0, len(bindings))
	for _, b := range bindings {
		if b.DeliveryScope == DeliveryScopeBuild {
			out = append(out, BuildSecretBinding{VariableName: b.VariableName, SecretVersionID: b.SecretVersionID})
		}
	}
	return out
}

// BuildSecretValue is one build-scoped secret's plaintext value, resolved
// for exactly one build attempt. It is transient: this package never
// persists it and holds it only for the duration of one Build call.
type BuildSecretValue struct {
	VariableName string
	Value        []byte
}

// BuildSecretMaterializer is the narrow seam this package builds against
// for turning frozen build-scoped secret-version references into
// plaintext. Step 6 implements it against Slice 9a Step 3a's fenced
// materialization; this package never reaches into materialization or
// secret decryption itself, and never receives anything but
// build-scoped bindings (see BuildScopedBindings).
type BuildSecretMaterializer interface {
	MaterializeBuildSecrets(ctx context.Context, bindings []BuildSecretBinding) ([]BuildSecretValue, error)
}

// BuildRequest is one buildx invocation's input.
type BuildRequest struct {
	// ContextDir is the build context directory. It becomes the subprocess
	// working directory; DockerfilePath is resolved relative to it.
	ContextDir string
	// DockerfilePath is the Dockerfile path relative to ContextDir. Empty
	// means "Dockerfile" in ContextDir's root.
	DockerfilePath string
	// Identity supplies the label fields applied at build time (see
	// BuiltImageIdentity).
	Identity BuiltImageIdentity
	// SecretBindings must already be filtered to build scope by
	// BuildScopedBindings; Build does not re-derive scope from anything
	// else.
	SecretBindings []BuildSecretBinding
	// Materializer resolves SecretBindings to plaintext. Required whenever
	// SecretBindings is non-empty.
	Materializer BuildSecretMaterializer
	// Stdout and Stderr are the pre-redaction sinks for build output. Build
	// wraps both in a crypto.RedactingWriter itself; callers must not
	// redact separately or pass an already-redacting writer.
	Stdout, Stderr io.Writer
}

// BuildResult is the outcome of one successful build: the built image's
// immutable content digest and its config blob's digest, both read back
// from buildx's own --metadata-file output. Everything downstream (Step 6)
// runs the container by ImageDigest, never a tag.
type BuildResult struct {
	ImageDigest  string
	ConfigDigest string
}

// envVarPattern bounds the variable name used both as a literal argv
// fragment (`--secret id=...,env=<VAR>`) and as a subprocess environment
// variable name. Rejecting anything outside this pattern is what keeps a
// binding's VariableName from being able to inject a comma, '=', or
// newline into the --secret flag's own encoding.
var envVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedSecretEnvVars names environment variables a build secret's
// VariableName must never be assigned to. These are variables the docker
// CLI/buildx itself reads to decide which daemon, builder, config, or TLS
// material to use; a secret binding named one of these would not merely
// supply a mount value, it would repoint the CLI invocation itself
// (potentially at an attacker-controlled daemon or a less isolated
// builder) or overwrite the search/home paths the subprocess depends on.
// This check is independent of, and does not replace, the minimal
// subprocess environment allowlist in subprocessEnv: that allowlist keeps
// the parent process's own secrets out of the child, while this one keeps
// a build secret from ever being assigned a name that controls the CLI.
var reservedSecretEnvVars = map[string]struct{}{
	"PATH":              {},
	"HOME":              {},
	"XDG_RUNTIME_DIR":   {},
	"DOCKER_HOST":       {},
	"DOCKER_CONFIG":     {},
	"DOCKER_CONTEXT":    {},
	"DOCKER_TLS_VERIFY": {},
	"DOCKER_CERT_PATH":  {},
	"BUILDKIT_HOST":     {},
	"BUILDX_BUILDER":    {},
	"BUILDX_CONFIG":     {},
}

// subprocessEnvAllowlist is the complete set of environment variables the
// docker/buildx subprocess may inherit from the agent process, before the
// per-build secret entries (which are never in this list -- see
// reservedSecretEnvVars) are appended. The agent's own process environment
// routinely carries values ADR 0003 forbids from reaching a build, such as
// AUTODEPLOY_DATABASE_URL (internal/config/database.go, carries a
// password) and KEK/GitHub-App-key file paths; passing os.Environ()
// through unfiltered would hand all of that to an arbitrary Dockerfile's
// build steps. Everything the docker CLI genuinely needs to run
// (locate itself, find a home/config directory, and optionally target a
// non-default daemon/builder) is named explicitly here instead.
var subprocessEnvAllowlist = []string{
	"PATH",
	"HOME",
	"XDG_RUNTIME_DIR",
	"DOCKER_HOST",
	"DOCKER_CONFIG",
	"DOCKER_CONTEXT",
	"DOCKER_TLS_VERIFY",
	"DOCKER_CERT_PATH",
	"BUILDKIT_HOST",
	"BUILDX_BUILDER",
	"BUILDX_CONFIG",
}

// subprocessEnv builds the docker/buildx subprocess's complete environment:
// only the allowlisted ambient variables that are actually set, followed by
// the per-build secret entries. It never starts from os.Environ().
func subprocessEnv(secretEnv []string) []string {
	env := make([]string, 0, len(subprocessEnvAllowlist)+len(secretEnv))
	for _, name := range subprocessEnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, secretEnv...)
}

// opaqueSecretID returns the opaque BuildKit session secret id for the i-th
// secret in a build, e.g. "build-secret-0". It is a positional index, never
// derived from the variable name or any secret content, so the id itself
// carries no information about what the secret is -- ADR 0003 requires the
// id to be opaque, and a name-derived id would leak the variable name (and
// therefore a hint about the secret's purpose) into build logs, argv, and
// BuildKit's own diagnostics.
func opaqueSecretID(i int) string { return fmt.Sprintf("build-secret-%d", i) }

// resolvedBuildSecret pairs one binding with its materialized value plus
// the opaque id and env var name that will carry it into the build.
type resolvedBuildSecret struct {
	id     string
	envVar string
	value  []byte
}

// resolveBuildSecrets validates that the materializer returned exactly one
// value per requested binding (by variable name, ADR 0003's identity for a
// binding) and assigns each a positional opaque id, sorted by variable name
// so argv construction is deterministic and testable.
func resolveBuildSecrets(bindings []BuildSecretBinding, values []BuildSecretValue) ([]resolvedBuildSecret, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	if len(values) != len(bindings) {
		return nil, fmt.Errorf("%w: materializer returned %d values for %d bindings", ErrInvalidBuildRequest, len(values), len(bindings))
	}
	byName := make(map[string][]byte, len(values))
	for _, v := range values {
		if _, exists := byName[v.VariableName]; exists {
			return nil, fmt.Errorf("%w: duplicate materialized value for %q", ErrInvalidBuildRequest, v.VariableName)
		}
		byName[v.VariableName] = v.Value
	}
	sorted := append([]BuildSecretBinding(nil), bindings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].VariableName < sorted[j].VariableName })

	out := make([]resolvedBuildSecret, 0, len(sorted))
	for i, b := range sorted {
		if !envVarPattern.MatchString(b.VariableName) {
			return nil, fmt.Errorf("%w: invalid secret variable name %q", ErrInvalidBuildRequest, b.VariableName)
		}
		if _, reserved := reservedSecretEnvVars[b.VariableName]; reserved {
			return nil, fmt.Errorf("%w: secret variable name %q is reserved for the docker/buildx CLI", ErrInvalidBuildRequest, b.VariableName)
		}
		value, ok := byName[b.VariableName]
		if !ok {
			return nil, fmt.Errorf("%w: materializer did not return a value for %q", ErrInvalidBuildRequest, b.VariableName)
		}
		if len(value) == 0 {
			return nil, fmt.Errorf("%w: empty materialized value for %q", ErrInvalidBuildRequest, b.VariableName)
		}
		out = append(out, resolvedBuildSecret{id: opaqueSecretID(i), envVar: b.VariableName, value: value})
	}
	return out, nil
}

// resolveDockerfilePath validates dockerfilePath and resolves it to a path
// relative to contextDir that is guaranteed, at the time of the check, to
// stay inside contextDir and to cross no symlink on the way there.
//
// docker buildx build's --file is resolved on the local filesystem and
// does not require the file to lie inside the build context: an
// unconstrained relative path such as "../../../etc/passwd" (or an
// absolute path) makes buildx open and parse an arbitrary host file as a
// Dockerfile, and a parse error echoes the offending file content into
// build output, where redaction only ever covers known secret patterns --
// not arbitrary host file content. On a host that builds other tenants'
// repositories that is a file-disclosure primitive, so this is checked
// before anything else touches DockerfilePath.
//
// filepath.Clean alone is not enough (it only rejects lexical escapes);
// a symlink inside the context, or a symlink standing in for a context
// subdirectory, can point anywhere on the host and would still lexically
// resolve as "inside" the context. So every path component from the
// context root down to (and including) the Dockerfile itself is checked
// with os.Lstat -- which, unlike os.Stat, does not follow a symlink -- and
// rejected if it is a symlink. A pre-open path check alone (a single
// os.Stat on the final joined path) would follow exactly the symlink this
// function exists to catch.
func resolveDockerfilePath(contextDir, dockerfilePath string) (string, error) {
	if strings.ContainsAny(dockerfilePath, "\x00\r\n") {
		return "", fmt.Errorf("%w: invalid dockerfile path", ErrInvalidBuildRequest)
	}
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if filepath.IsAbs(dockerfilePath) {
		return "", fmt.Errorf("%w: dockerfile path must not be absolute", ErrInvalidBuildRequest)
	}
	cleaned := filepath.Clean(dockerfilePath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: dockerfile path escapes the build context", ErrInvalidBuildRequest)
	}

	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return "", fmt.Errorf("%w: unusable context directory: %v", ErrInvalidBuildRequest, err)
	}
	if info, err := os.Lstat(absContext); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: unusable context directory", ErrInvalidBuildRequest)
	}

	parts := strings.Split(cleaned, string(filepath.Separator))
	current := absContext
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("%w: dockerfile path unusable: %v", ErrInvalidBuildRequest, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: dockerfile path traverses a symlink", ErrInvalidBuildRequest)
		}
		last := i == len(parts)-1
		if last {
			if info.IsDir() {
				return "", fmt.Errorf("%w: dockerfile path is a directory", ErrInvalidBuildRequest)
			}
		} else if !info.IsDir() {
			return "", fmt.Errorf("%w: dockerfile path component %q is not a directory", ErrInvalidBuildRequest, part)
		}
	}
	return cleaned, nil
}

// buildxArgv constructs the exact `docker buildx build` argv this package
// emits, plus the "VAR=value" subprocess environment entries that deliver
// secret values. No secret value or fragment of one ever appears in the
// returned argv: values travel only through env. dockerfile must already
// be resolved by resolveDockerfilePath.
//
// Shape (labels and secrets sorted for determinism):
//
//	buildx build --progress=plain --file <path>
//	  --label <k>=<v> [--label <k>=<v> ...]
//	  --secret id=<opaque>,env=<VAR> [--secret id=<opaque>,env=<VAR> ...]
//	  --metadata-file <path> --load <contextDir>
func buildxArgv(dockerfile string, labels map[string]string, secrets []resolvedBuildSecret, metadataFile string) ([]string, []string) {
	args := []string{"buildx", "build", "--progress=plain", "--file", dockerfile}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}

	env := make([]string, 0, len(secrets))
	for _, s := range secrets {
		args = append(args, "--secret", "id="+s.id+",env="+s.envVar)
		env = append(env, s.envVar+"="+string(s.value))
	}

	args = append(args, "--metadata-file", metadataFile, "--load", ".")
	return args, env
}

func validateBuildRequest(req BuildRequest) error {
	if req.ContextDir == "" {
		return fmt.Errorf("%w: empty context directory", ErrInvalidBuildRequest)
	}
	info, err := os.Stat(req.ContextDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: context directory unusable: %v", ErrInvalidBuildRequest, err)
	}
	if _, err := resolveDockerfilePath(req.ContextDir, req.DockerfilePath); err != nil {
		return err
	}
	if len(req.SecretBindings) > 0 && req.Materializer == nil {
		return fmt.Errorf("%w: secret bindings require a materializer", ErrInvalidBuildRequest)
	}
	seen := make(map[string]struct{}, len(req.SecretBindings))
	for _, b := range req.SecretBindings {
		if b.VariableName == "" || b.SecretVersionID == "" {
			return fmt.Errorf("%w: empty secret binding field", ErrInvalidBuildRequest)
		}
		if _, exists := seen[b.VariableName]; exists {
			return fmt.Errorf("%w: duplicate secret binding for %q", ErrInvalidBuildRequest, b.VariableName)
		}
		seen[b.VariableName] = struct{}{}
	}
	return nil
}

// buildMetadata is the subset of buildx's --metadata-file JSON this
// package relies on.
type buildMetadata struct {
	ImageDigest  string `json:"containerimage.digest"`
	ConfigDigest string `json:"containerimage.config.digest"`
}

func parseBuildMetadata(data []byte) (BuildResult, error) {
	var meta buildMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return BuildResult{}, fmt.Errorf("%w: unreadable metadata file: %v", ErrBuildFailed, err)
	}
	imageDigest, err := requireHexDigest("containerimage.digest", meta.ImageDigest)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", ErrBuildFailed, err)
	}
	configDigest, err := requireHexDigest("containerimage.config.digest", meta.ConfigDigest)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", ErrBuildFailed, err)
	}
	return BuildResult{ImageDigest: imageDigest, ConfigDigest: configDigest}, nil
}

// dockerExecutable names the CLI binary this package shells out to. A var
// (not a const) only so tests can point it at a fake binary via PATH
// manipulation while keeping the literal name identical to production.
const dockerExecutable = "docker"

// probeBuildx is the buildx half of the plan's startup capability probe
// (the Engine-API half lives in Client.Probe; buildx has no HTTP surface to
// probe over the socket). It fails closed whenever the docker CLI or the
// buildx plugin is missing or returns anything but a clean success --
// including a present-but-broken plugin, which must not be treated as
// available.
func probeBuildx(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, dockerExecutable, "buildx", "version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %v", ErrBuildxUnavailable, err)
	}
	if !strings.Contains(out.String(), "buildx") {
		return fmt.Errorf("%w: unrecognized buildx version output", ErrBuildxUnavailable)
	}
	return nil
}

// build is the real implementation behind the platform-gated Build. It is
// unexported and untagged (unlike the Engine API client, running a
// subprocess has no platform restriction) so tests on any OS can exercise
// it directly with a fake docker binary on PATH, mirroring how
// engine_test.go exercises newClient directly. The exported, platform-gated
// Build wraps this identically to how client_linux.go wraps newClient.
func build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	if err := validateBuildRequest(req); err != nil {
		return BuildResult{}, err
	}
	// Fail closed before anything else runs: a missing/unusable buildx
	// must never fall through to a legacy `docker build`, which has no
	// --secret support.
	if err := probeBuildx(ctx); err != nil {
		return BuildResult{}, err
	}
	labels, err := BuiltImageLabels(req.Identity)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", ErrInvalidBuildRequest, err)
	}

	var values []BuildSecretValue
	if len(req.SecretBindings) > 0 {
		values, err = req.Materializer.MaterializeBuildSecrets(ctx, req.SecretBindings)
		if err != nil {
			return BuildResult{}, err
		}
	}
	resolved, err := resolveBuildSecrets(req.SecretBindings, values)
	if err != nil {
		return BuildResult{}, err
	}
	// resolved[i].value aliases the same backing array as the matching
	// values[i].Value (resolveBuildSecrets does not copy), so the
	// authoritative plaintext copy for the rest of this call is `resolved`;
	// values itself is not cleared separately to avoid a double-free-style
	// clear racing the alias.

	redactionValues := make([][]byte, len(resolved))
	for i, s := range resolved {
		redactionValues[i] = s.value
	}

	// Redactor construction failure must suppress output, never emit it
	// raw: both writers are built before the subprocess runs, and any
	// failure here returns without starting docker at all.
	stdout, err := crypto.NewRedactingWriter(orDiscard(req.Stdout), redactionValues)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: stdout redactor: %v", crypto.ErrRedaction, err)
	}
	stderr, err := crypto.NewRedactingWriter(orDiscard(req.Stderr), redactionValues)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: stderr redactor: %v", crypto.ErrRedaction, err)
	}

	// Already checked once by validateBuildRequest above; resolved again
	// here (not carried forward) because that call intentionally discards
	// the resolved path, and TOCTOU on a re-check is no worse than the
	// single check would be -- both run immediately before the path is
	// used, with no attacker-controlled step in between.
	dockerfile, err := resolveDockerfilePath(req.ContextDir, req.DockerfilePath)
	if err != nil {
		return BuildResult{}, err
	}

	metaDir, err := os.MkdirTemp("", "adbx")
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", ErrBuildFailed, err)
	}
	defer os.RemoveAll(metaDir)
	metaFile := metaDir + "/metadata.json"

	argv, secretEnv := buildxArgv(dockerfile, labels, resolved, metaFile)
	for i := range resolved {
		clearBytes(resolved[i].value)
	}

	cmd := exec.CommandContext(ctx, dockerExecutable, argv...)
	cmd.Dir = req.ContextDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = subprocessEnv(secretEnv)

	runErr := cmd.Run()
	// Wipe the subprocess env strings we built; cmd.Env's backing array is
	// ours (append onto os.Environ() may or may not reuse it, so this is
	// best-effort, not a guarantee -- matching ADR 0003's "memory clearing
	// is best effort only").
	for i := range secretEnv {
		secretEnv[i] = ""
	}

	closeErr := errors.Join(stdout.Close(), stderr.Close())
	if runErr != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", ErrBuildFailed, runErr)
	}
	if closeErr != nil {
		return BuildResult{}, fmt.Errorf("%w: %v", crypto.ErrRedaction, closeErr)
	}

	data, err := os.ReadFile(metaFile)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: metadata file unreadable: %v", ErrBuildFailed, err)
	}
	return parseBuildMetadata(data)
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return discardWriter{}
	}
	return w
}
