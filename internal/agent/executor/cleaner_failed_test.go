package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/runtime/docker"
)

// multiDockerFake models several containers by runtime instance id. Its error
// shapes mirror the real client: inspect 404 is a *docker.EngineError (Status
// 404) whose cause wraps ErrContainerNotFound (engine.go InspectContainer);
// remove 404/409 are fmt.Errorf-wrapped sentinels (remove.go removeResult).
type multiDockerFake struct {
	mu          sync.Mutex
	calls       []string
	cs          map[string]*fakeCtr // by runtime instance id
	removeErr   map[string]error    // by instance id
	imageErr    error
	removedRefs []string
	stopErr     error
	listN       int
	listErrAt   map[int]error  // nth ListContainersByLabels call (1-based) fails
	beforeList  map[int]func() // runs before the nth list call computes its answer
	afterList   map[int]func() // runs after the nth list call computed its answer
}

type fakeCtr struct {
	id      string
	labels  map[string]string
	running bool
	image   string
	gone    bool
}

func ctrID(n int) string { return fmt.Sprintf("%064x", n+1000) }

func fcLabels(instance, release, digest string) map[string]string {
	return map[string]string{
		docker.LabelManaged: "true", docker.LabelKind: docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID: cleanEnv, docker.LabelReleaseID: release, docker.LabelRuntimeInstanceID: instance,
		docker.LabelImageDigest: digest, docker.LabelConfigDigest: cleanConfig,
	}
}

func newMulti(instances ...string) *multiDockerFake {
	f := &multiDockerFake{cs: map[string]*fakeCtr{}, removeErr: map[string]error{}}
	for i, in := range instances {
		f.cs[in] = &fakeCtr{id: ctrID(i), labels: fcLabels(in, cleanRelease, cleanDigest), image: "sha256:" + cleanConfig}
	}
	return f
}

func (f *multiDockerFake) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *multiDockerFake) byRef(ref string) *fakeCtr {
	for in, c := range f.cs {
		if !c.gone && (ref == c.id || ref == "autodeploy-rt-"+in || strings.HasSuffix(ref, "-"+in)) {
			return c
		}
	}
	return nil
}

func (f *multiDockerFake) InspectContainer(_ context.Context, ref string) (docker.ContainerInspect, error) {
	f.rec("inspect")
	c := f.byRef(ref)
	if c == nil {
		return docker.ContainerInspect{}, &docker.EngineError{Class: docker.ErrClassConfiguration, Op: "container-inspect", Status: 404,
			Err: fmt.Errorf("%w: no such container", docker.ErrContainerNotFound)}
	}
	var out docker.ContainerInspect
	out.ID, out.Image = c.id, c.image
	out.State.Running = c.running
	return out, nil
}

func (f *multiDockerFake) ListContainersByLabels(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	f.rec("list")
	if len(labels) < 5 {
		return nil, errors.New("selector too broad")
	}
	f.listN++
	if fn := f.beforeList[f.listN]; fn != nil {
		fn()
	}
	if fn := f.afterList[f.listN]; fn != nil {
		defer fn()
	}
	if err := f.listErrAt[f.listN]; err != nil {
		return nil, err
	}
	var out []docker.ContainerSummary
	for _, c := range f.cs {
		if c.gone {
			continue
		}
		ok := true
		for k, v := range labels {
			if c.labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, docker.ContainerSummary{ID: c.id, Labels: c.labels})
		}
	}
	return out, nil
}

func (f *multiDockerFake) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.rec("stop:" + id)
	if f.stopErr != nil {
		return f.stopErr
	}
	for _, c := range f.cs {
		if c.id == id {
			c.running = false
		}
	}
	return nil
}

func (f *multiDockerFake) WaitStopped(_ context.Context, _ string, _, _ time.Duration) (docker.ContainerInspect, error) {
	f.rec("wait")
	return docker.ContainerInspect{}, nil
}

func (f *multiDockerFake) RemoveContainer(_ context.Context, id string) error {
	f.rec("rmc:" + id)
	for in, c := range f.cs {
		if c.id == id {
			if err := f.removeErr[in]; err != nil {
				return err
			}
			c.gone = true
			return nil
		}
	}
	return fmt.Errorf("%w: container-remove", docker.ErrContainerNotFound)
}

func (f *multiDockerFake) RemoveImage(_ context.Context, ref string) error {
	f.rec("rmi:" + ref)
	f.removedRefs = append(f.removedRefs, ref)
	return f.imageErr
}

func (f *multiDockerFake) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func fcItem(digest string, ids ...string) agentclient.CleanupItem {
	return agentclient.CleanupItem{CleanupID: 21, ReleaseID: cleanRelease, EnvironmentID: cleanEnv, ImageDigest: digest,
		Kind: agentclient.CleanupKindFailedCandidate, RuntimeInstanceIDs: ids}
}

func runFC(t *testing.T, f *multiDockerFake, it agentclient.CleanupItem) cleanReport {
	t.Helper()
	cp := &cleanCP{items: []agentclient.CleanupItem{it}}
	c := newCleaner(nil, cp)
	c.Docker = f
	c.StopGrace, c.StopWait = time.Millisecond, time.Second
	c.Pass(context.Background())
	if len(cp.reports) != 1 {
		t.Fatalf("reports = %+v", cp.reports)
	}
	return cp.reports[0]
}

func TestFailedCandidateMixedPresentRunningMissing(t *testing.T) {
	f := newMulti("11", "12", "13")
	f.cs["12"].running = true
	delete(f.cs, "13") // missing by name and by labels
	r := runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
	if r.result != "removed" || r.kept {
		t.Fatalf("report = %+v", r)
	}
	if f.count("stop:") != 1 || !f.cs["11"].gone || !f.cs["12"].gone {
		t.Fatalf("calls = %v", f.calls)
	}
	if f.count("rmc:") != 2 || f.count("rmi:") != 1 || f.removedRefs[0] != "sha256:"+cleanConfig {
		t.Fatalf("calls = %v", f.calls)
	}
	// The running one is stopped before it is removed.
	stop, rm := -1, -1
	for i, c := range f.calls {
		if c == "stop:"+f.cs["12"].id {
			stop = i
		}
		if c == "rmc:"+f.cs["12"].id {
			rm = i
		}
	}
	if stop < 0 || rm < stop {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestFailedCandidateEmptyDigestKeepsImage(t *testing.T) {
	f := newMulti("11", "12")
	f.cs["11"].labels[docker.LabelImageDigest] = strings.Repeat("c", 64) // digest label not compared when claim digest is empty
	r := runFC(t, f, fcItem("", "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 0 || !f.cs["11"].gone || !f.cs["12"].gone {
		t.Fatalf("report = %+v calls = %v", r, f.calls)
	}
}

func TestFailedCandidateAllMissingKeepsImage(t *testing.T) {
	f := newMulti()
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 0 || f.count("rmc:") != 0 {
		t.Fatalf("report = %+v calls = %v", r, f.calls)
	}
}

func TestFailedCandidateImageInUseIsKeptStillRemoved(t *testing.T) {
	f := newMulti("11")
	f.imageErr = fmt.Errorf("%w: image-remove", docker.ErrImageInUse)
	r := runFC(t, f, fcItem(cleanDigest, "11"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 1 {
		t.Fatalf("report = %+v calls = %v", r, f.calls)
	}
}

func TestFailedCandidateUnverifiedImageIdIsKept(t *testing.T) {
	f := newMulti("11", "12")
	f.cs["12"].image = "sha256:" + strings.Repeat("d", 64)
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 0 {
		t.Fatalf("report = %+v calls = %v", r, f.calls)
	}
}

func TestFailedCandidateIdentityMismatchStopsAndTouchesNothingLater(t *testing.T) {
	for name, mutate := range map[string]func(*multiDockerFake){
		"digest label differs": func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelImageDigest] = strings.Repeat("c", 64) },
		"release label":        func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelReleaseID] = "other" },
		"environment label":    func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelEnvironmentID] = "other" },
		"instance label names another instance (no label match)": func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelRuntimeInstanceID] = "99" },
		"right name, wrong release-id label":                     func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelReleaseID] = "rel-other-owner" },
		"not managed":                                            func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelManaged] = "false" },
		"wrong kind":                                             func(f *multiDockerFake) { f.cs["12"].labels[docker.LabelKind] = "other" },
		"missing labels":                                         func(f *multiDockerFake) { f.cs["12"].labels = map[string]string{} },
		"two label matches": func(f *multiDockerFake) {
			f.cs["99"] = &fakeCtr{id: ctrID(99), labels: fcLabels("12", cleanRelease, cleanDigest), image: "sha256:" + cleanConfig}
		},
	} {
		f := newMulti("11", "12", "13")
		f.cs["12"].running = true
		mutate(f)
		r := runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
		if r.result != "skipped" || r.class != "identity" {
			t.Fatalf("%s: report = %+v", name, r)
		}
		if !f.cs["11"].gone || f.cs["13"].gone || f.count("rmc:") != 1 || f.count("stop:") != 0 || f.count("rmi:") != 0 {
			t.Fatalf("%s: calls = %v", name, f.calls)
		}
		if !strings.Contains(r.msg, "11:removed") {
			t.Fatalf("%s: summary lacks the earlier removal: %q", name, r.msg)
		}
	}
}

func TestFailedCandidateAbsentByNameButLabelsMatchIsRefused(t *testing.T) {
	f := newMulti("11")
	f.cs["11"].labels[docker.LabelRuntimeInstanceID] = "11"
	f.cs["77"] = &fakeCtr{id: ctrID(77), labels: fcLabels("12", cleanRelease, cleanDigest), image: "sha256:" + cleanConfig}
	// instance 12 has no container named for it but one with its labels exists
	r := runFC(t, f, fcItem(cleanDigest, "12", "11"))
	if r.result != "skipped" || r.class != "identity" || f.count("rmc:") != 0 {
		t.Fatalf("report = %+v calls = %v", r, f.calls)
	}
}

func TestFailedCandidateInvalidClaimsTouchNothing(t *testing.T) {
	long := make([]string, 65)
	for i := range long {
		long[i] = fmt.Sprint(i + 1)
	}
	for name, it := range map[string]agentclient.CleanupItem{
		"empty list":        fcItem(cleanDigest),
		"bad id":            fcItem(cleanDigest, "11", "07"),
		"non numeric":       fcItem(cleanDigest, "1x"),
		"duplicate id":      fcItem(cleanDigest, "11", "11"),
		"too many":          fcItem(cleanDigest, long...),
		"bad digest":        fcItem("ab", "11"),
		"upper digest":      fcItem(strings.ToUpper(cleanDigest), "11"),
		"bad release":       func() agentclient.CleanupItem { i := fcItem(cleanDigest, "11"); i.ReleaseID = ""; return i }(),
		"zero cleanup id":   func() agentclient.CleanupItem { i := fcItem(cleanDigest, "11"); i.CleanupID = 0; return i }(),
		"singular id given": func() agentclient.CleanupItem { i := fcItem(cleanDigest, "11"); i.RuntimeInstanceID = "11"; return i }(),
	} {
		f := newMulti("11")
		r := runFC(t, f, it)
		if r.result != "skipped" || r.class != "identity" || len(f.calls) != 0 {
			t.Fatalf("%s: report %+v calls %v", name, r, f.calls)
		}
	}
}

func TestFailedCandidateEngineErrorStopsTheClaim(t *testing.T) {
	f := newMulti("11", "12", "13")
	f.removeErr["12"] = &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-remove", Status: 500, Err: errors.New("boom")}
	r := runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
	if r.result != "failed" || r.class != "engine" || f.cs["13"].gone || f.count("rmc:") != 2 || f.count("rmi:") != 0 {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
	f = newMulti("11", "12")
	f.removeErr["11"] = fmt.Errorf("%w: container-remove", docker.ErrContainerRunning)
	r = runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "failed" || r.class != "stop" || f.cs["12"].gone || f.count("rmi:") != 0 {
		t.Fatalf("running refusal: %+v calls %v", r, f.calls)
	}
	// 404 on remove (raced away) is success, as for retained.
	f = newMulti("11")
	f.removeErr["11"] = fmt.Errorf("%w: container-remove", docker.ErrContainerNotFound)
	if r = runFC(t, f, fcItem(cleanDigest, "11")); r.result != "removed" {
		t.Fatalf("remove 404: %+v", r)
	}
}

func TestFailedCandidateInspectEngineErrorFailsAndStops(t *testing.T) {
	f := newMulti("11", "12")
	g := &inspectFailFake{multiDockerFake: f, failName: "12"}
	cp := &cleanCP{items: []agentclient.CleanupItem{fcItem(cleanDigest, "12", "11")}}
	c := newCleaner(nil, cp)
	c.Docker = g
	c.Pass(context.Background())
	if r := cp.reports[0]; r.result != "failed" || r.class != "engine" || f.count("rmc:") != 0 {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}

type inspectFailFake struct {
	*multiDockerFake
	failName string
}

func (g *inspectFailFake) InspectContainer(ctx context.Context, ref string) (docker.ContainerInspect, error) {
	if strings.HasSuffix(ref, "-"+g.failName) {
		return docker.ContainerInspect{}, &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-inspect", Err: errors.New("x")}
	}
	return g.multiDockerFake.InspectContainer(ctx, ref)
}

func TestCleanerRefusesUnknownKind(t *testing.T) {
	for _, kind := range []string{agentclient.CleanupKindUnknown, "mystery", "FAILED_CANDIDATE"} {
		f := newMulti("11")
		it := fcItem(cleanDigest, "11")
		it.Kind = kind
		it.RuntimeInstanceID = "11"
		r := runFC(t, f, it)
		if r.result != "skipped" || r.class != "other" || len(f.calls) != 0 {
			t.Fatalf("%q: report %+v calls %v", kind, r, f.calls)
		}
	}
}

// A retained item in the extended shape (Kind set, plus an ignored id list)
// behaves exactly like the 20E item: the same single-container flow.
func TestRetainedItemInExtendedShapeBehavesAsBefore(t *testing.T) {
	plain, cp1 := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	newCleaner(plain, cp1).Pass(context.Background())
	it := cleanItem()
	it.Kind = agentclient.CleanupKindRetained
	ext, cp2 := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{it}}
	newCleaner(ext, cp2).Pass(context.Background())
	if fmt.Sprint(plain.calls) != fmt.Sprint(ext.calls) || fmt.Sprint(cp1.reports) != fmt.Sprint(cp2.reports) || cp2.reports[0].result != "removed" {
		t.Fatalf("plain %v %+v / extended %v %+v", plain.calls, cp1.reports, ext.calls, cp2.reports)
	}
}

// Two removed containers that each verify against a different image id must not
// make the cleaner pick one: the image is kept.
func TestFailedCandidateDisagreeingImageIdsKeepTheImage(t *testing.T) {
	f := newMulti("11", "12")
	other := strings.Repeat("e", 64)
	f.cs["12"].labels[docker.LabelConfigDigest] = other
	f.cs["12"].image = "sha256:" + other
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 0 || f.count("rmc:") != 2 {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}

// A container that vanishes while being stopped has no verifiable image id, so
// the image is kept even though the other container verified (retained path
// parity, cleaner.go stopGone).
func TestFailedCandidateVanishedWhileStoppingKeepsImage(t *testing.T) {
	f := newMulti("11", "12")
	f.cs["12"].running = true
	f.afterList = map[int]func(){2: func() { f.cs["12"].gone = true }} // gone after the cleaner's own lookup
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 0 || !f.cs["11"].gone || !strings.Contains(r.msg, "vanished") {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}

func TestFailedCandidateStopFailureStopsTheClaim(t *testing.T) {
	f := newMulti("11", "12", "13")
	f.cs["12"].running = true
	f.stopErr = &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-stop", Status: 500, Err: errors.New("boom")}
	r := runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
	if r.result != "failed" || r.class != "stop" || !f.cs["11"].gone || f.cs["12"].gone || f.cs["13"].gone ||
		f.count("rmc:") != 1 || f.count("rmi:") != 0 || !strings.Contains(r.msg, "11:removed") {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}

func TestFailedCandidateStopIdentityMismatchStopsTheClaim(t *testing.T) {
	f := newMulti("11", "12", "13")
	f.cs["12"].running = true
	// The guarded stop's own label check (3rd list call) no longer finds the container.
	f.beforeList = map[int]func(){3: func() { f.cs["12"].labels[docker.LabelReleaseID] = "changed" }}
	r := runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
	if r.result != "skipped" || r.class != "identity" || f.count("stop:") != 0 || f.cs["12"].gone || f.cs["13"].gone ||
		f.count("rmc:") != 1 || f.count("rmi:") != 0 {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}

func TestFailedCandidateLabelLookupErrorsStopTheClaim(t *testing.T) {
	boom := &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-list", Status: 500, Err: errors.New("boom")}
	// After a successful inspect.
	f := newMulti("11", "12")
	f.listErrAt = map[int]error{1: boom}
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "failed" || r.class != "engine" || f.count("rmc:") != 0 || f.count("rmi:") != 0 {
		t.Fatalf("after inspect: report %+v calls %v", r, f.calls)
	}
	// On the absent-by-name path (instance 12 has no container; 11 was removed first).
	f = newMulti("11", "12", "13")
	delete(f.cs, "12")
	f.listErrAt = map[int]error{2: boom}
	r = runFC(t, f, fcItem(cleanDigest, "11", "12", "13"))
	if r.result != "failed" || r.class != "engine" || !f.cs["11"].gone || f.cs["13"].gone || f.count("rmc:") != 1 || f.count("rmi:") != 0 {
		t.Fatalf("absent path: report %+v calls %v", r, f.calls)
	}
}

func TestFailedCandidateImageRemoveFailureIsKeptAndReported(t *testing.T) {
	f := newMulti("11", "12")
	f.imageErr = &docker.EngineError{Class: docker.ErrClassTransient, Op: "image-remove", Status: 500, Err: errors.New("boom")}
	r := runFC(t, f, fcItem(cleanDigest, "11", "12"))
	if r.result != "removed" || !r.kept || f.count("rmi:") != 1 || !strings.Contains(r.msg, "11:removed") ||
		!strings.Contains(r.msg, "12:removed") || !strings.Contains(r.msg, "remove failed") {
		t.Fatalf("report %+v calls %v", r, f.calls)
	}
}
