package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autodeploy/internal/queue"
)

// fakeBuilderDocker installs a fake docker whose behaviour is steered by files
// in a temp directory: every invocation is appended to calls.log as
// "<BUILDX_CONFIG>|<argv>", `buildx ls` prints ls.json, and any subcommand
// named in failOn (space separated, e.g. "create") exits 1.
func fakeBuilderDocker(t *testing.T, lsJSON, failOn string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	lsPath := filepath.Join(dir, "ls.json")
	if err := os.WriteFile(lsPath, []byte(lsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeDocker(t, fmt.Sprintf(`echo "${BUILDX_CONFIG:-}|$*" >> %q
for f in %s; do
  if [ "$2" = "$f" ]; then echo "boom" >&2; exit 1; fi
done
case "$2" in
  ls) cat %q ;;
  inspect) echo "Name: autodeploy-build"; echo "Driver: docker-container"; echo "Status: running" ;;
esac
`, logPath, shellWords(failOn), lsPath))
	return logPath
}

func shellWords(s string) string {
	if s == "" {
		return "_none_"
	}
	return s
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func testBuilderConfig(t *testing.T) BuilderConfig {
	t.Helper()
	return BuilderConfig{ConfigDir: filepath.Join(t.TempDir(), "buildx"), MemoryBytes: 2 << 30, CPUs: 2}
}

const wantDriverOpts = "image=" + BuildkitImage + ",memory=2147483648,memory-swap=2147483648,cpu-period=100000,cpu-quota=200000"

func matchingLS() string {
	return `{"Name":"autodeploy-build","Driver":"docker-container","Nodes":[{"Name":"autodeploy-build0","DriverOpts":{"image":"` + BuildkitImage +
		`","memory":"2147483648","memory-swap":"2147483648","cpu-period":"100000","cpu-quota":"200000"}}]}` + "\n" +
		`{"Name":"default","Driver":"docker","Nodes":[{"Name":"default"}]}` + "\n"
}

func TestBuildkitImageIsPinnedByDigest(t *testing.T) {
	if !strings.HasPrefix(BuildkitImage, "moby/buildkit:v") || !strings.Contains(BuildkitImage, "@sha256:") {
		t.Fatalf("BuildkitImage = %q, want moby/buildkit:<tag>@sha256:<digest>", BuildkitImage)
	}
}

func TestEnsureBuilderCreatesWhenAbsent(t *testing.T) {
	logPath := fakeBuilderDocker(t, `{"Name":"default","Driver":"docker","Nodes":[{"Name":"default"}]}`+"\n", "")
	cfg := testBuilderConfig(t)
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatalf("ensureBuilder: %v", err)
	}
	calls := readCalls(t, logPath)
	want := []string{
		cfg.ConfigDir + "|buildx ls --format json",
		cfg.ConfigDir + "|buildx create --name autodeploy-build --driver docker-container --driver-opt " + wantDriverOpts,
		cfg.ConfigDir + "|buildx inspect --builder autodeploy-build --bootstrap",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	if fi, err := os.Stat(cfg.ConfigDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("config dir = %v, %v; want existing 0700", fi, err)
	}
}

func TestEnsureBuilderMatchingBuilderIsNotRecreated(t *testing.T) {
	logPath := fakeBuilderDocker(t, matchingLS(), "")
	cfg := testBuilderConfig(t)
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatalf("ensureBuilder: %v", err)
	}
	for _, c := range readCalls(t, logPath) {
		if strings.Contains(c, "buildx create") || strings.Contains(c, "buildx rm") {
			t.Fatalf("matching builder must not be recreated, saw %q", c)
		}
	}
}

func TestEnsureBuilderDriftedBuilderIsRemovedAndRecreated(t *testing.T) {
	drift := strings.Replace(matchingLS(), `"memory":"2147483648"`, `"memory":"1073741824"`, 1)
	logPath := fakeBuilderDocker(t, drift, "")
	cfg := testBuilderConfig(t)
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatalf("ensureBuilder: %v", err)
	}
	var seq []string
	for _, c := range readCalls(t, logPath) {
		seq = append(seq, strings.SplitN(c, "|", 2)[1])
	}
	got := strings.Join(seq, "\n")
	for _, w := range []string{"buildx rm autodeploy-build", "buildx create --name autodeploy-build"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Index(got, "buildx rm") > strings.Index(got, "buildx create") {
		t.Fatalf("rm must precede create:\n%s", got)
	}
	for _, c := range seq {
		if strings.Contains(c, "buildx rm") && c != "buildx rm autodeploy-build" {
			t.Fatalf("rm may only name autodeploy-build, got %q", c)
		}
	}
}

func TestEnsureBuilderWrongDriverIsRecreated(t *testing.T) {
	wrong := `{"Name":"autodeploy-build","Driver":"docker","Nodes":[{"Name":"x"}]}` + "\n"
	logPath := fakeBuilderDocker(t, wrong, "")
	if err := ensureBuilder(context.Background(), testBuilderConfig(t)); err != nil {
		t.Fatalf("ensureBuilder: %v", err)
	}
	if got := strings.Join(readCalls(t, logPath), "\n"); !strings.Contains(got, "buildx rm autodeploy-build") || !strings.Contains(got, "buildx create") {
		t.Fatalf("want rm+create, got:\n%s", got)
	}
}

func TestEnsureBuilderFailsClosed(t *testing.T) {
	for _, failOn := range []string{"ls", "create", "inspect"} {
		t.Run(failOn, func(t *testing.T) {
			ls := `{"Name":"default","Driver":"docker","Nodes":[{"Name":"default"}]}` + "\n"
			fakeBuilderDocker(t, ls, failOn)
			err := ensureBuilder(context.Background(), testBuilderConfig(t))
			if !errors.Is(err, ErrBuilderUnavailable) {
				t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
			}
		})
	}
}

func TestEnsureBuilderRmFailureFailsClosed(t *testing.T) {
	drift := strings.Replace(matchingLS(), `"cpu-quota":"200000"`, `"cpu-quota":"100000"`, 1)
	fakeBuilderDocker(t, drift, "rm")
	if err := ensureBuilder(context.Background(), testBuilderConfig(t)); !errors.Is(err, ErrBuilderUnavailable) {
		t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
	}
}

func TestEnsureBuilderUnparsableListFailsClosed(t *testing.T) {
	fakeBuilderDocker(t, "not json\n", "")
	if err := ensureBuilder(context.Background(), testBuilderConfig(t)); !errors.Is(err, ErrBuilderUnavailable) {
		t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
	}
}

func TestEnsureBuilderMissingDockerFailsClosed(t *testing.T) {
	noDockerOnPath(t)
	if err := ensureBuilder(context.Background(), testBuilderConfig(t)); !errors.Is(err, ErrBuilderUnavailable) {
		t.Fatalf("err = %v, want ErrBuilderUnavailable", err)
	}
}

func TestBuilderConfigValidate(t *testing.T) {
	ok := BuilderConfig{ConfigDir: "/var/lib/autodeploy/agent/buildx", MemoryBytes: 2 << 30, CPUs: 2}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := map[string]func(*BuilderConfig){
		"empty dir":     func(c *BuilderConfig) { c.ConfigDir = "" },
		"relative dir":  func(c *BuilderConfig) { c.ConfigDir = "buildx" },
		"memory low":    func(c *BuilderConfig) { c.MemoryBytes = MinBuildMemoryBytes - 1 },
		"memory high":   func(c *BuilderConfig) { c.MemoryBytes = MaxBuildMemoryBytes + 1 },
		"cpus zero":     func(c *BuilderConfig) { c.CPUs = 0 },
		"cpus too high": func(c *BuilderConfig) { c.CPUs = MaxBuildCPUs + 1 },
	}
	for name, mut := range bad {
		c := ok
		mut(&c)
		if err := c.validate(); !errors.Is(err, ErrBuilderUnavailable) {
			t.Errorf("%s: err = %v, want ErrBuilderUnavailable", name, err)
		}
	}
}

func TestBuildPassesBuilderAndConfigDir(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv")
	writeFakeDocker(t, fmt.Sprintf(`case "$2" in
  version) echo "github.com/docker/buildx v0.0.0"; exit 0 ;;
esac
echo "${BUILDX_CONFIG:-}|${BUILDX_BUILDER:-}|$*" > %q
meta=""
while [ $# -gt 0 ]; do if [ "$1" = "--metadata-file" ]; then meta="$2"; fi; shift; done
printf %q > "$meta"
`, argvLog, `{"containerimage.digest":"sha256:`+strings.Repeat("b", 64)+`","containerimage.config.digest":"sha256:`+strings.Repeat("c", 64)+`"}`))
	t.Setenv("BUILDX_BUILDER", "ambient-builder") // must never win over --builder
	t.Setenv("BUILDX_CONFIG", "/ambient/config")
	req := testBuildRequest(t, t.TempDir())
	req.BuildxConfigDir = "/var/lib/autodeploy/agent/buildx"
	if _, err := build(context.Background(), req); err != nil {
		t.Fatalf("build: %v", err)
	}
	b, _ := os.ReadFile(argvLog)
	got := string(b)
	if !strings.HasPrefix(got, "/var/lib/autodeploy/agent/buildx||buildx build ") {
		t.Fatalf("config/builder env wrong: %q", got)
	}
	if !strings.Contains(got, " --builder autodeploy-build ") {
		t.Fatalf("argv lacks --builder autodeploy-build: %q", got)
	}
}

func TestBuildxArgvAlwaysNamesTheDedicatedBuilder(t *testing.T) {
	argv, _ := buildxArgv("Dockerfile", nil, nil, nil, "/tmp/meta.json")
	for i, a := range argv {
		if a == "--builder" && i+1 < len(argv) && argv[i+1] == BuilderName {
			return
		}
	}
	t.Fatalf("argv = %v, want --builder %s", argv, BuilderName)
}

// builderLossDocker is a fake docker whose build always fails (exit 3). The
// plain `buildx inspect --builder autodeploy-build` (no --bootstrap) succeeds
// only when the builder exists: state lives in dir/exists, `buildx create`
// creates it, and `buildx rm` removes it. Every call is appended to calls.log.
func builderLossDocker(t *testing.T, exists bool) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	marker := filepath.Join(dir, "exists")
	if exists {
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeDocker(t, fmt.Sprintf(`echo "$*" >> %q
case "$2" in
  version) echo "github.com/docker/buildx v0.17.0" ;;
  ls) echo '{"Name":"default","Driver":"docker","Nodes":[{"Name":"default"}]}' ;;
  create) : > %q ;;
  rm) rm -f %q ;;
  inspect) if [ -e %q ]; then echo "Name: autodeploy-build"; echo "Driver: docker-container"; else echo "no builder" >&2; exit 1; fi ;;
  build) echo "boom" >&2; exit 3 ;;
esac
`, logPath, marker, marker, marker))
	return logPath
}

func lossRequest(t *testing.T) BuildRequest {
	t.Helper()
	cfg := testBuilderConfig(t)
	req := testBuildRequest(t, t.TempDir())
	req.Timeout = time.Minute
	req.BuildxConfigDir = cfg.ConfigDir
	req.Builder = cfg
	return req
}

func TestBuildFailureWithMissingBuilderIsBuilderUnavailableTransient(t *testing.T) {
	logPath := builderLossDocker(t, false)
	_, err := build(context.Background(), lossRequest(t))
	if !errors.Is(err, ErrBuilderUnavailable) || errors.Is(err, ErrBuildFailed) {
		t.Fatalf("want ErrBuilderUnavailable (not ErrBuildFailed), got %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassTransient {
		t.Fatalf("class = %s, want transient", got)
	}
	if calls := strings.Join(readCalls(t, logPath), "\n"); !strings.Contains(calls, "buildx create") {
		t.Fatalf("builder must be re-ensured once, calls:\n%s", calls)
	}
}

func TestBuildFailureWithHealthyBuilderStaysBuildFailed(t *testing.T) {
	logPath := builderLossDocker(t, true)
	_, err := build(context.Background(), lossRequest(t))
	if !errors.Is(err, ErrBuildFailed) || errors.Is(err, ErrBuilderUnavailable) {
		t.Fatalf("want plain ErrBuildFailed, got %v", err)
	}
	if got := ClassifyFailure(err); got != queue.FailureClassPermanentConfiguration {
		t.Fatalf("class = %s, want permanent_configuration", got)
	}
	if calls := strings.Join(readCalls(t, logPath), "\n"); strings.Contains(calls, "buildx create") || strings.Contains(calls, "buildx rm") {
		t.Fatalf("healthy builder must not be touched, calls:\n%s", calls)
	}
}

func TestBuildFailureWithMissingBuilderAndFailedRecoveryStillUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeFakeDocker(t, fmt.Sprintf(`case "$2" in
  version) echo "github.com/docker/buildx v0.17.0" ;;
  build) echo "boom" >&2; exit 3 ;;
  *) echo "gone" >&2; echo x > %q; exit 1 ;;
esac
`, filepath.Join(dir, "touched")))
	_, err := build(context.Background(), lossRequest(t))
	if !errors.Is(err, ErrBuilderUnavailable) || errors.Is(err, ErrBuildFailed) {
		t.Fatalf("want ErrBuilderUnavailable, got %v", err)
	}
}

func TestBuilderUnavailableIsNotPermanent(t *testing.T) {
	if got := ClassifyFailure(fmt.Errorf("x: %w", ErrBuilderUnavailable)); got != queue.FailureClassTransient {
		t.Fatalf("class = %s, want transient", got)
	}
}

func TestEnsureBuilderLogsWhyItRecreates(t *testing.T) {
	cases := map[string]struct {
		ls   string
		want string
	}{
		"driver":      {`{"Name":"autodeploy-build","Driver":"docker","Nodes":[{"Name":"x"}]}` + "\n", `driver "docker"`},
		"opt differs": {strings.Replace(matchingLS(), `"memory":"2147483648"`, `"memory":"1073741824"`, 1), "driver-opt memory differs"},
		"opt absent":  {strings.Replace(matchingLS(), `"cpu-quota":"200000"`, `"cpu-other":"1"`, 1), "driver-opt cpu-quota absent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakeBuilderDocker(t, tc.ls, "")
			var buf bytes.Buffer
			cfg := testBuilderConfig(t)
			cfg.Logger = slog.New(slog.NewTextHandler(&buf, nil))
			if err := ensureBuilder(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(unquoted(&buf), "recreating") || !strings.Contains(unquoted(&buf), tc.want) {
				t.Fatalf("log = %q, want recreating + %q", buf.String(), tc.want)
			}
			if strings.Contains(buf.String(), BuildkitImage) && name != "driver" {
				t.Fatalf("log must not dump the image reference: %q", buf.String())
			}
		})
	}
}

// unquoted undoes slog text-handler value quoting so assertions can match
// the human text.
func unquoted(buf *bytes.Buffer) string { return strings.ReplaceAll(buf.String(), `\"`, `"`) }

// A nil Logger discards: nothing reaches the standard logger or slog default.
func TestEnsureBuilderNilLoggerDiscards(t *testing.T) {
	fakeBuilderDocker(t, `{"Name":"autodeploy-build","Driver":"docker","Nodes":[{"Name":"x"}]}`+"\n", "")
	var std, def bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	prevDef := slog.Default()
	log.SetOutput(&std)
	slog.SetDefault(slog.New(slog.NewTextHandler(&def, nil)))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags); slog.SetDefault(prevDef) })
	cfg := testBuilderConfig(t)
	cfg.Logger = nil
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if std.Len() != 0 || def.Len() != 0 {
		t.Fatalf("nil Logger wrote to a process logger: std=%q default=%q", std.String(), def.String())
	}
}

func TestEnsureBuilderLogsWhenDriverOptsAbsentFromListing(t *testing.T) {
	ls := `{"Name":"autodeploy-build","Driver":"docker-container","Nodes":[{"Name":"autodeploy-build0"}]}` + "\n"
	fakeBuilderDocker(t, ls, "")
	var buf bytes.Buffer
	cfg := testBuilderConfig(t)
	cfg.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unquoted(&buf), "reports no driver options") {
		t.Fatalf("log = %q, want the absent-DriverOpts note", buf.String())
	}
}

func TestEnsureBuilderTightensLooseConfigDir(t *testing.T) {
	fakeBuilderDocker(t, matchingLS(), "")
	cfg := testBuilderConfig(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureBuilder(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(cfg.ConfigDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("config dir = %v, %v; want 0700", fi, err)
	}
}
