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
	"time"

	"autodeploy/internal/buildsecret"
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
	// ErrBuildTimeout means the build ran past the agent's own build time
	// limit (BuildRequest.Timeout) and was killed. The build is too slow
	// for its limit, so retrying unchanged would hit the same wall:
	// permanent_configuration. It deliberately does not wrap
	// context.DeadlineExceeded, so it can never be mistaken for a lost
	// lease.
	ErrBuildTimeout = errors.New("docker buildx: build exceeded its time limit")
	// ErrBuildInterrupted means the caller's context ended mid-build (job
	// cancel, lease loss, agent shutdown) and the build was killed. Nothing
	// about the application failed, so this is transient. The wrapped
	// chain also carries the caller's context error.
	ErrBuildInterrupted = errors.New("docker buildx: build interrupted")
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
	// Literals are the build-scoped literal_nonsecret bindings, delivered as
	// `--build-arg NAME=value` (ADR 0003 forbids ARG/ENV only for secrets;
	// literals are non-secret by classification). Names are validated by
	// resolveBuildLiterals; the values are visible in argv and image history.
	Literals []BuildLiteral
	// Stdout and Stderr are the pre-redaction sinks for build output. Build
	// wraps both in a crypto.RedactingWriter itself; callers must not
	// redact separately or pass an already-redacting writer.
	Stdout, Stderr io.Writer
	// Timeout bounds the buildx subprocess. Zero or negative means
	// DefaultBuildTimeout, so a build is never unbounded.
	Timeout time.Duration
	// BuildxConfigDir is the BUILDX_CONFIG directory holding the dedicated
	// builder's definition (BuilderConfig.ConfigDir). Empty leaves buildx's
	// own default; production always sets it.
	BuildxConfigDir string
	// Builder carries the dedicated builder's caps so a builder lost after
	// startup can be recreated once after a failed build (see
	// checkBuilderAfterFailure). Its ConfigDir defaults to BuildxConfigDir.
	Builder BuilderConfig
}

// BuildLiteral is one build-scoped literal_nonsecret binding.
type BuildLiteral struct {
	VariableName string
	Value        string
}

// ErrInvalidBuildLiteral is returned when a build-scoped literal binding is
// refused (invalid, reserved or built-in name, duplicate, or a name also used
// as a build secret). It wraps ErrInvalidBuildRequest, so it classifies as
// permanent_configuration. Messages name the variable, never its value.
var ErrInvalidBuildLiteral = fmt.Errorf("%w: build literal", ErrInvalidBuildRequest)

// DefaultBuildTimeout is the build time limit when BuildRequest.Timeout is
// unset; it matches the agent's --build-timeout default.
const DefaultBuildTimeout = 30 * time.Minute

// buildWaitDelay bounds how long Wait lingers after the process group is
// killed, so a descendant that somehow still holds the output pipes cannot
// hang the build forever.
const buildWaitDelay = 10 * time.Second

// EffectiveBuildTimeout resolves an unset (<= 0) limit to the default.
func EffectiveBuildTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultBuildTimeout
	}
	return d
}

// BuildResult is the outcome of one successful build: the built image's
// immutable content digest and its config blob's digest, both read back
// from buildx's own --metadata-file output. Everything downstream (Step 6)
// runs the container by ImageDigest, never a tag.
type BuildResult struct {
	ImageDigest string
	// ConfigDigest is the config blob digest (classic image store) or, when
	// buildx reports none (containerd image store), the image digest, i.e. the
	// loaded image's local ID.
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

	// Pinned by subprocessEnv; a secret must not be able to flip them back.
	"BUILDX_GIT_INFO":            {},
	"BUILDX_METADATA_PROVENANCE": {},
}

// Literals deliberately keep the narrower rule (the fixed set above plus
// BUILDKIT_*/BUILDX_*, no DOCKER_* prefix): they travel as `--build-arg` argv,
// never in the CLI's environment, so DOCKER_*-named literals are harmless.
// PATH/HOME/DOCKER_HOST stay refused for literals on purpose, to avoid
// confusion with the same names as env-borne secrets; that is documented
// behaviour, not an accident.

// reservedBuildArgNames are BuildKit/Dockerfile predefined build args a
// literal must not override (case-insensitive): the proxy args and the
// platform args.
var reservedBuildArgNames = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "FTP_PROXY": {}, "NO_PROXY": {}, "ALL_PROXY": {},
	"TARGETPLATFORM": {}, "TARGETOS": {}, "TARGETARCH": {}, "TARGETVARIANT": {},
	"BUILDPLATFORM": {}, "BUILDOS": {}, "BUILDARCH": {}, "BUILDVARIANT": {},
}

// maxBuildLiteralNameLen mirrors the control plane's variable_name CHECK
// (1..128 and the same pattern as envVarPattern).
const maxBuildLiteralNameLen = 128

// resolveBuildLiterals validates the build-scoped literals and returns them
// sorted by name so argv is deterministic. A name that is invalid, duplicated,
// reserved (reservedSecretEnvVars), a BuildKit/buildx built-in (BUILDKIT_*,
// BUILDX_*, proxy and platform args) or also a build secret is refused, never
// dropped: the secret clash is refused rather than guessed.
func resolveBuildLiterals(literals []BuildLiteral, secrets []BuildSecretBinding) ([]BuildLiteral, error) {
	if len(literals) == 0 {
		return nil, nil
	}
	secretNames := make(map[string]struct{}, len(secrets))
	for _, b := range secrets {
		secretNames[b.VariableName] = struct{}{}
	}
	seen := make(map[string]struct{}, len(literals))
	for _, l := range literals {
		n := l.VariableName
		if len(n) < 1 || len(n) > maxBuildLiteralNameLen || !envVarPattern.MatchString(n) {
			return nil, fmt.Errorf("%w: invalid variable name %q", ErrInvalidBuildLiteral, n)
		}
		upper := strings.ToUpper(n)
		_, reserved := reservedSecretEnvVars[n]
		_, builtIn := reservedBuildArgNames[upper]
		if reserved || builtIn || strings.HasPrefix(upper, "BUILDKIT_") || strings.HasPrefix(upper, "BUILDX_") {
			return nil, fmt.Errorf("%w: variable name %q is reserved for docker/buildx/BuildKit", ErrInvalidBuildLiteral, n)
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("%w: duplicate variable name %q", ErrInvalidBuildLiteral, n)
		}
		seen[n] = struct{}{}
		if _, clash := secretNames[n]; clash {
			return nil, fmt.Errorf("%w: variable name %q is bound both as a build secret and a build literal", ErrInvalidBuildLiteral, n)
		}
	}
	out := append([]BuildLiteral(nil), literals...)
	sort.Slice(out, func(i, j int) bool { return out[i].VariableName < out[j].VariableName })
	return out, nil
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
	// Fixed, non-secret buildx behaviour switches (never overridable by the
	// ambient environment or a secret binding; see reservedSecretEnvVars for
	// BUILDX_*): BUILDX_GIT_INFO=0 stops buildx probing the context for a git
	// repository (the checkout is deliberately not a usable repository for this
	// user and the probe only produces a warning), and
	// BUILDX_METADATA_PROVENANCE=disabled keeps buildx.build.provenance out of
	// the --metadata-file JSON.
	env = append(env, "BUILDX_GIT_INFO=0", "BUILDX_METADATA_PROVENANCE=disabled")
	return append(env, secretEnv...)
}

// resolvedBuildSecret pairs one binding with its materialized value plus
// the opaque id and env var name that will carry it into the build.
type resolvedBuildSecret struct {
	id     string
	envVar string
	value  []byte
}

// resolveBuildSecrets validates that the materializer returned exactly one
// value per requested binding (by variable name, ADR 0003's identity for a
// binding) and assigns each its opaque id via buildsecret.ID (index among the
// build-scoped names sorted by variable name, ADR 0003: never the name itself),
// so ids and argv order are deterministic whatever order bindings arrive in.
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
	names := make([]string, len(sorted))
	for i, b := range sorted {
		if i > 0 && names[i-1] == b.VariableName {
			return nil, fmt.Errorf("%w: duplicate secret binding %q", ErrInvalidBuildRequest, b.VariableName)
		}
		names[i] = b.VariableName
	}

	out := make([]resolvedBuildSecret, 0, len(sorted))
	for _, b := range sorted {
		if !envVarPattern.MatchString(b.VariableName) {
			return nil, fmt.Errorf("%w: invalid secret variable name %q", ErrInvalidBuildRequest, b.VariableName)
		}
		// Secret values are exported into the docker CLI's own environment, so
		// every BUILDKIT_*/BUILDX_*/DOCKER_* name (any case) is refused, not
		// only the fixed set: those prefixes are CLI configuration.
		upper := strings.ToUpper(b.VariableName)
		_, reserved := reservedSecretEnvVars[b.VariableName]
		if reserved || strings.HasPrefix(upper, "BUILDKIT_") || strings.HasPrefix(upper, "BUILDX_") || strings.HasPrefix(upper, "DOCKER_") {
			return nil, fmt.Errorf("%w: secret variable name %q is reserved for the docker/buildx CLI", ErrInvalidBuildRequest, b.VariableName)
		}
		value, ok := byName[b.VariableName]
		if !ok {
			return nil, fmt.Errorf("%w: materializer did not return a value for %q", ErrInvalidBuildRequest, b.VariableName)
		}
		if len(value) == 0 {
			return nil, fmt.Errorf("%w: empty materialized value for %q", ErrInvalidBuildRequest, b.VariableName)
		}
		id, _ := buildsecret.ID(names, b.VariableName) // b's name is in names by construction
		out = append(out, resolvedBuildSecret{id: id, envVar: b.VariableName, value: value})
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
//	buildx build --builder autodeploy-build --progress=plain --file <path> --provenance=false --sbom=false
//	  --label <k>=<v> [--label <k>=<v> ...]
//	  --secret id=<opaque>,env=<VAR> [--secret id=<opaque>,env=<VAR> ...]
//	  --build-arg <NAME>=<literal> [--build-arg ...]   (non-secret literals only, sorted by name)
//	  --metadata-file <path> --load <contextDir>
func buildxArgv(dockerfile string, labels map[string]string, secrets []resolvedBuildSecret, literals []BuildLiteral, metadataFile string) ([]string, []string) {
	// --provenance=false --sbom=false: with the containerd image store
	// (Docker >= 29 default) buildx otherwise exports an OCI index holding an
	// attestation manifest next to the image, so --metadata-file carries no
	// containerimage.config.digest and the image's local ID becomes the index
	// digest. A single image manifest is what the run path addresses, and the
	// provenance attestation embeds the build's invocation (build args, source
	// layout) that ADR 0003 requires to stay out of anything persisted with
	// the image. See ADR 0003 amendments.
	// --builder: every build runs in the dedicated capped builder (builder.go),
	// never the default one; the name is a constant, not ambient state.
	args := []string{"buildx", "build", "--builder", BuilderName, "--progress=plain", "--file", dockerfile, "--provenance=false", "--sbom=false"}

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

	// Literals are non-secret by classification and already sorted by
	// resolveBuildLiterals; they travel in argv, never in the subprocess env.
	for _, l := range literals {
		args = append(args, "--build-arg", l.VariableName+"="+l.Value)
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
	if _, err := resolveBuildLiterals(req.Literals, req.SecretBindings); err != nil {
		return err
	}
	return nil
}

// buildMetadata is the subset of buildx's --metadata-file JSON this
// package relies on. containerimage.config.digest is present only with the
// classic image store; the containerd image store never writes it.
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
	configDigest := imageDigest
	if meta.ConfigDigest != "" {
		configDigest, err = requireHexDigest("containerimage.config.digest", meta.ConfigDigest)
		if err != nil {
			return BuildResult{}, fmt.Errorf("%w: %v", ErrBuildFailed, err)
		}
	}
	// Containerd image store: no config digest is reported and the loaded
	// image's ID is the manifest digest itself, so that is the identity this
	// host can address (Client.resolveLocalImage probes both). The value only
	// feeds the config-digest label and the local image lookup.
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
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = buildWaitDelay
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
	var ran bool
	res, err := runBuild(ctx, req, &ran)
	if err != nil && !ran {
		err = MarkBuildNotStarted(err)
	}
	return res, err
}

// buildNotStartedError marks a build that failed before the buildx subprocess
// ran (validation, probe, secret materialization, setup).
type buildNotStartedError struct{ err error }

func (e *buildNotStartedError) Error() string { return e.err.Error() }
func (e *buildNotStartedError) Unwrap() error { return e.err }

// MarkBuildNotStarted wraps err as a failure that happened before the buildx
// subprocess ran. Build applies it itself; it is exported so callers' fakes
// can reproduce that shape.
func MarkBuildNotStarted(err error) error { return &buildNotStartedError{err} }

// BuildNeverStarted reports whether err came from a Build that failed before
// the buildx subprocess was started, so there is nothing to clean up after it.
func BuildNeverStarted(err error) bool {
	var ns *buildNotStartedError
	return errors.As(err, &ns)
}

func runBuild(ctx context.Context, req BuildRequest, ran *bool) (BuildResult, error) {
	if err := validateBuildRequest(req); err != nil {
		return BuildResult{}, err
	}
	// Fail closed before anything else runs: a missing/unusable buildx
	// must never fall through to a legacy `docker build`, which has no
	// --secret support.
	if err := probeBuildx(ctx); err != nil {
		if ctx.Err() != nil {
			// The probe was killed by the caller's context, not by a
			// broken buildx: a cancel/lease loss, not host misconfiguration.
			return BuildResult{}, fmt.Errorf("%w: %w", ErrBuildInterrupted, ctx.Err())
		}
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
	literals, err := resolveBuildLiterals(req.Literals, req.SecretBindings)
	if err != nil {
		return BuildResult{}, err
	}
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

	argv, secretEnv := buildxArgv(dockerfile, labels, resolved, literals, metaFile)
	for i := range resolved {
		clearBytes(resolved[i].value)
	}

	// Only the subprocess is bounded by the limit; the earlier probe and
	// secret materialization keep the caller's context. A derived context
	// lets the two causes of death be told apart afterwards by their own
	// Err(), never by error text.
	limit := EffectiveBuildTimeout(req.Timeout)
	buildCtx, cancelBuild := context.WithTimeout(ctx, limit)
	defer cancelBuild()
	cmd := exec.CommandContext(buildCtx, dockerExecutable, argv...)
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = buildWaitDelay
	cmd.Dir = req.ContextDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = withBuildxConfig(subprocessEnv(secretEnv), req.BuildxConfigDir)

	*ran = true
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
		switch {
		case ctx.Err() != nil:
			// The caller's context ended (checked first: it also ends
			// buildCtx, and a lost lease must never read as a timeout).
			return BuildResult{}, fmt.Errorf("%w: %w", ErrBuildInterrupted, ctx.Err())
		case errors.Is(buildCtx.Err(), context.DeadlineExceeded):
			return BuildResult{}, fmt.Errorf("%w (%s): %v", ErrBuildTimeout, limit, runErr)
		}
		// A non-zero exit is the tenant's unless the builder itself is gone.
		bcfg := req.Builder
		if bcfg.ConfigDir == "" {
			bcfg.ConfigDir = req.BuildxConfigDir
		}
		if berr := checkBuilderAfterFailure(ctx, bcfg); berr != nil {
			return BuildResult{}, berr
		}
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
