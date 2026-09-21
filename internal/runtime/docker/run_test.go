package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestContainerNameValid(t *testing.T) {
	name, err := ContainerName(validID)
	if err != nil {
		t.Fatalf("ContainerName: %v", err)
	}
	if !strings.HasPrefix(name, containerNamePrefix) {
		t.Fatalf("name %q missing prefix %q", name, containerNamePrefix)
	}
	if !strings.HasSuffix(name, validID) {
		t.Fatalf("name %q does not encode the runtime-instance id %q", name, validID)
	}
}

func TestContainerNameRejectsInvalidID(t *testing.T) {
	for _, bad := range []string{"", "-leading-dash", "has spaces", "has/slash", "feature/x", "fix: bug"} {
		if _, err := ContainerName(bad); err == nil {
			t.Fatalf("ContainerName(%q) succeeded, want rejection", bad)
		}
	}
}

func TestContainerNameNeverEmbedsBranchOrCommitText(t *testing.T) {
	// The whole point of ContainerName is that it can only ever encode a
	// validated opaque ID -- there is no parameter for a branch, repo
	// name, or commit message to leak through. Passing exactly that kind
	// of value must be rejected, not sanitized and accepted.
	for _, userish := range []string{"refs/heads/main", "fix: SQL injection'; DROP TABLE users;--", "my-repo.git"} {
		if _, err := ContainerName(userish); err == nil {
			t.Fatalf("ContainerName(%q) succeeded; user-shaped input must be rejected, not embedded", userish)
		}
	}
}

func TestRequireNetworkIDAccepts12And64Hex(t *testing.T) {
	short := strings.Repeat("a", 12)
	long := strings.Repeat("a", 64)
	for _, id := range []string{short, long} {
		if _, err := requireNetworkID(id); err != nil {
			t.Fatalf("requireNetworkID(%q): %v", id, err)
		}
	}
}

// TestRequireNetworkIDRejectsDockerMagicStrings is the structural proof
// that host networking cannot reach HostConfig.NetworkMode through this
// package: every Docker-special network-mode string must fail validation.
func TestRequireNetworkIDRejectsDockerMagicStrings(t *testing.T) {
	for _, magic := range []string{"host", "none", "bridge", "container:abc123", "", "HOST"} {
		if _, err := requireNetworkID(magic); err == nil {
			t.Fatalf("requireNetworkID(%q) succeeded, want rejection", magic)
		}
	}
}

func TestBuildContainerCreatePayloadNoPortsOneNetworkDigestImage(t *testing.T) {
	req := RunContainerRequest{
		Identity:      validReleaseIdentity(),
		NetworkID:     strings.Repeat("b", 64),
		CPUMillis:     500,
		MemoryBytes:   256 << 20,
		RestartPolicy: RestartPolicyUnlessStopped,
	}
	labels, err := ReleaseRuntimeContainerLabels(req.Identity)
	if err != nil {
		t.Fatalf("ReleaseRuntimeContainerLabels: %v", err)
	}
	payload := buildContainerCreatePayload(req, labels)

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)

	// Non-negotiable: no published ports, structurally. The wire struct
	// has no field to encode either, so this also proves the struct
	// itself carries none, not merely that this call happened not to set
	// one.
	for _, forbidden := range []string{"PortBindings", "PublishAllPorts", "NetworkingConfig", `"NetworkMode":"host"`, `"NetworkMode":"bridge"`, `"NetworkMode":"none"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("create payload contains forbidden fragment %q: %s", forbidden, body)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hostConfig, ok := decoded["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("no HostConfig in payload: %s", body)
	}
	if hostConfig["NetworkMode"] != req.NetworkID {
		t.Fatalf("NetworkMode = %v, want %v", hostConfig["NetworkMode"], req.NetworkID)
	}
	image, _ := decoded["Image"].(string)
	if image != "sha256:"+req.Identity.ConfigDigest[len("sha256:"):] {
		t.Fatalf("Image = %q, want digest reference derived from ConfigDigest", image)
	}
	if !strings.HasPrefix(image, "sha256:") || strings.Contains(image, ":latest") {
		t.Fatalf("Image %q is not a pure digest reference", image)
	}
}

func TestBuildContainerCreatePayloadCPUAndMemory(t *testing.T) {
	req := RunContainerRequest{
		Identity:      validReleaseIdentity(),
		NetworkID:     strings.Repeat("c", 64),
		CPUMillis:     1500,
		MemoryBytes:   1 << 30,
		RestartPolicy: RestartPolicyOnFailure,
	}
	labels, _ := ReleaseRuntimeContainerLabels(req.Identity)
	payload := buildContainerCreatePayload(req, labels)
	if payload.HostConfig.NanoCPUs != 1_500_000_000 {
		t.Fatalf("NanoCPUs = %d, want 1_500_000_000", payload.HostConfig.NanoCPUs)
	}
	if payload.HostConfig.Memory != 1<<30 {
		t.Fatalf("Memory = %d, want %d", payload.HostConfig.Memory, int64(1)<<30)
	}
	if payload.HostConfig.RestartPolicy.Name != RestartPolicyOnFailure {
		t.Fatalf("RestartPolicy = %q, want %q", payload.HostConfig.RestartPolicy.Name, RestartPolicyOnFailure)
	}
}

func TestRunContainerRequestValidateRejectsBadRestartPolicy(t *testing.T) {
	req := RunContainerRequest{
		NetworkID:     strings.Repeat("a", 64),
		CPUMillis:     100,
		MemoryBytes:   1,
		RestartPolicy: "always", // not one of the two frozen-config values
	}
	if err := req.validate(); err == nil {
		t.Fatal("want rejection of unsupported restart policy")
	}
}

func TestBuildContainerCreatePayloadIncludesSortedEnv(t *testing.T) {
	req := RunContainerRequest{
		Identity:      validReleaseIdentity(),
		NetworkID:     strings.Repeat("d", 64),
		CPUMillis:     500,
		MemoryBytes:   256 << 20,
		RestartPolicy: RestartPolicyUnlessStopped,
		Env: []ContainerEnv{
			{VariableName: "ZVAR", Value: "z"},
			{VariableName: "AVAR", Value: "a-secret"},
		},
	}
	labels, _ := ReleaseRuntimeContainerLabels(req.Identity)
	payload := buildContainerCreatePayload(req, labels)
	want := []string{"AVAR=a-secret", "ZVAR=z"}
	if !reflect.DeepEqual(payload.Env, want) {
		t.Fatalf("Env = %v, want %v (sorted by name)", payload.Env, want)
	}
}

func TestRunContainerRequestValidateRejectsBadEnv(t *testing.T) {
	base := RunContainerRequest{NetworkID: strings.Repeat("a", 64), CPUMillis: 1, MemoryBytes: 1, RestartPolicy: RestartPolicyUnlessStopped}
	badName := base
	badName.Env = []ContainerEnv{{VariableName: "not valid", Value: "x"}}
	if err := badName.validate(); err == nil {
		t.Fatal("want rejection of invalid env variable name")
	}
	dup := base
	dup.Env = []ContainerEnv{{VariableName: "A", Value: "1"}, {VariableName: "A", Value: "2"}}
	if err := dup.validate(); err == nil {
		t.Fatal("want rejection of duplicate env variable name")
	}
}

func TestRunContainerRequestValidateRejectsNonPositiveLimits(t *testing.T) {
	base := RunContainerRequest{NetworkID: strings.Repeat("a", 64), RestartPolicy: RestartPolicyUnlessStopped}
	cpuZero := base
	cpuZero.MemoryBytes = 1
	if err := cpuZero.validate(); err == nil {
		t.Fatal("want rejection of zero cpu millis")
	}
	memZero := base
	memZero.CPUMillis = 100
	if err := memZero.validate(); err == nil {
		t.Fatal("want rejection of zero memory bytes")
	}
}

// --- Engine-API-backed tests (fake unix socket, mirroring engine_test.go) ---

// labelFilterHandler asserts the lookup request actually carries the label
// selector for validReleaseIdentity() rather than merely some non-empty
// filter, so a lookup that forgot to send the selector, or sent a
// different one, is caught. It builds its expectation from the selector
// itself, so it is a wiring check: the label set's own contents are
// pinned independently by labels_test.go's literal expected map, which is
// what would catch a dropped or broadened label.
func labelFilterHandler(t *testing.T, responses []ContainerSummary) http.HandlerFunc {
	t.Helper()
	want, err := ReleaseRuntimeContainerSelector(validReleaseIdentity())
	if err != nil {
		t.Fatalf("ReleaseRuntimeContainerSelector: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("filters")
		if raw == "" {
			t.Errorf("expected a filters query parameter, got none")
			return
		}
		var decoded struct {
			Label []string `json:"label"`
		}
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Errorf("filters %q is not valid JSON: %v", raw, err)
			return
		}
		got := make(map[string]string, len(decoded.Label))
		for _, entry := range decoded.Label {
			k, v, ok := strings.Cut(entry, "=")
			if !ok {
				t.Errorf("label filter %q is not key=value", entry)
				return
			}
			got[k] = v
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("label filter =\n%v\nwant exactly\n%v", got, want)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

func TestFindReleaseRuntimeContainerNoneFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", labelFilterHandler(t, nil))
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	got, err := c.FindReleaseRuntimeContainer(context.Background(), validReleaseIdentity())
	if err != nil {
		t.Fatalf("FindReleaseRuntimeContainer: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestFindReleaseRuntimeContainerOneFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", labelFilterHandler(t, []ContainerSummary{{ID: "c1", State: "running"}}))
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	got, err := c.FindReleaseRuntimeContainer(context.Background(), validReleaseIdentity())
	if err != nil {
		t.Fatalf("FindReleaseRuntimeContainer: %v", err)
	}
	if got == nil || got.ID != "c1" {
		t.Fatalf("got %+v, want ID=c1", got)
	}
}

func TestFindReleaseRuntimeContainerMultipleIsInconsistentState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", labelFilterHandler(t, []ContainerSummary{{ID: "c1"}, {ID: "c2"}}))
	c, err := newClient(startFakeEngine(t, mux))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.FindReleaseRuntimeContainer(context.Background(), validReleaseIdentity())
	if !errors.Is(err, ErrInconsistentLocalState) {
		t.Fatalf("err = %v, want ErrInconsistentLocalState", err)
	}
}

// ensureFakeEngine wires a minimal fake Engine API sufficient to drive
// EnsureReleaseRuntimeContainer end to end: label lookup, network inspect,
// container create, start, and inspect. created/started/inspected let each
// test assert exactly which calls happened.
type ensureFakeEngine struct {
	existing        []ContainerSummary
	networkID       string
	networkMissing  bool
	createStatus    int
	createID        string
	created         []containerCreateRequest
	startCalls      []string
	inspectResponse ContainerInspect
}

func (f *ensureFakeEngine) mux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.44/containers/json", labelFilterHandler(t, f.existing))
	mux.HandleFunc("/v1.44/networks/", func(w http.ResponseWriter, r *http.Request) {
		if f.networkMissing {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("network not found"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(NetworkInspect{ID: f.networkID, Name: "env-net"})
	})
	mux.HandleFunc("/v1.44/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var req containerCreateRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.created = append(f.created, req)
		if f.createStatus != 0 && f.createStatus != http.StatusCreated {
			w.WriteHeader(f.createStatus)
			w.Write([]byte("conflict"))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(containerCreateResponse{ID: f.createID})
	})
	mux.HandleFunc("/v1.44/containers/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.44/containers/"), "/start")
			f.startCalls = append(f.startCalls, id)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(f.inspectResponse)
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	})
	return mux
}

func validRunRequest() RunContainerRequest {
	return RunContainerRequest{
		Identity:      validReleaseIdentity(),
		CPUMillis:     500,
		MemoryBytes:   256 << 20,
		RestartPolicy: RestartPolicyUnlessStopped,
	}
}

// runningInspect builds a ContainerInspect reporting id as running, for
// fake-engine responses -- avoids repeating ContainerInspect's anonymous
// State struct literal at every call site.
func runningInspect(id string) ContainerInspect {
	var out ContainerInspect
	out.ID = id
	out.State.Running = true
	return out
}

func TestEnsureReleaseRuntimeContainerCreatesWhenAbsent(t *testing.T) {
	f := &ensureFakeEngine{
		networkID:       strings.Repeat("d", 64),
		createID:        "newcontainer",
		inspectResponse: runningInspect("newcontainer"),
	}
	c, err := newClient(startFakeEngine(t, f.mux(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	got, err := c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if err != nil {
		t.Fatalf("EnsureReleaseRuntimeContainer: %v", err)
	}
	if got.ID != "newcontainer" {
		t.Fatalf("got ID %q, want newcontainer", got.ID)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d containers, want 1", len(f.created))
	}
	if len(f.startCalls) != 1 || f.startCalls[0] != "newcontainer" {
		t.Fatalf("start calls = %v, want [newcontainer]", f.startCalls)
	}
	// The two fields isolation actually depends on. Asserting only the ID
	// and the call counts would still pass if Ensure forgot to resolve the
	// network name to its canonical ID, or addressed the image by tag.
	if got := f.created[0].HostConfig.NetworkMode; got != f.networkID {
		t.Fatalf("NetworkMode = %q, want the resolved network ID %q -- a raw name here means InspectNetwork was skipped", got, f.networkID)
	}
	wantImage := "sha256:" + strings.TrimPrefix(validReleaseIdentity().ConfigDigest, "sha256:")
	if got := f.created[0].Image; got != wantImage {
		t.Fatalf("Image = %q, want the digest-pinned reference %q", got, wantImage)
	}
}

func TestEnsureReleaseRuntimeContainerReusesPriorAttempt(t *testing.T) {
	f := &ensureFakeEngine{
		existing:        []ContainerSummary{{ID: "prior", State: "created"}},
		networkID:       strings.Repeat("e", 64),
		inspectResponse: runningInspect("prior"),
	}
	c, err := newClient(startFakeEngine(t, f.mux(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	got, err := c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if err != nil {
		t.Fatalf("EnsureReleaseRuntimeContainer: %v", err)
	}
	if got.ID != "prior" {
		t.Fatalf("got ID %q, want prior (reused, not created)", got.ID)
	}
	if len(f.created) != 0 {
		t.Fatalf("created %d containers, want 0 -- retry must not create a second container", len(f.created))
	}
	if len(f.startCalls) != 1 || f.startCalls[0] != "prior" {
		t.Fatalf("start calls = %v, want [prior]", f.startCalls)
	}
}

func TestEnsureReleaseRuntimeContainerFailsClosedOnMissingNetwork(t *testing.T) {
	f := &ensureFakeEngine{networkMissing: true}
	c, err := newClient(startFakeEngine(t, f.mux(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if err == nil {
		t.Fatal("want error when the environment network does not exist")
	}
	if len(f.created) != 0 {
		t.Fatalf("created %d containers, want 0 -- must fail closed before creating anything", len(f.created))
	}
	if ClassifyFailure(err).Retryable() {
		t.Fatalf("missing environment network must not be classified retryable: %v -> %v", err, ClassifyFailure(err))
	}
}

func TestEnsureReleaseRuntimeContainerNameConflictIsCorruptState(t *testing.T) {
	f := &ensureFakeEngine{
		networkID:    strings.Repeat("f", 64),
		createStatus: http.StatusConflict,
	}
	c, err := newClient(startFakeEngine(t, f.mux(t)))
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	_, err = c.EnsureReleaseRuntimeContainer(context.Background(), validRunRequest(), "env-net")
	if !errors.Is(err, ErrContainerNameConflict) {
		t.Fatalf("err = %v, want ErrContainerNameConflict", err)
	}
	if got := ClassifyFailure(err); string(got) != "corrupt_state" {
		t.Fatalf("ClassifyFailure = %v, want corrupt_state", got)
	}
}
