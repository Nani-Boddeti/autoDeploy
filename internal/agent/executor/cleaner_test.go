package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentclient "autodeploy/internal/agent/client"
	"autodeploy/internal/runtime/docker"
)

var _ CleanupDocker = (*docker.Client)(nil)
var _ CleanupControlPlane = (*agentclient.Client)(nil)

const (
	cleanEnv      = "env-clean"
	cleanRelease  = "rel-clean"
	cleanInstance = "77"
	cleanDigest   = "aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11aa11"
	cleanConfig   = "bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22bb22"
	cleanID       = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type cleanDockerFake struct {
	mu          sync.Mutex
	calls       []string
	present     bool
	running     bool
	labels      map[string]string
	imageID     string
	extraMatch  int // additional containers matching the label query
	stopErr     error
	removeErr   error
	imageErr    error
	listErr     error
	inspectErr  error
	removedID   string
	removedRefs []string
}

func newCleanDocker() *cleanDockerFake {
	return &cleanDockerFake{present: true, imageID: "sha256:" + cleanConfig, labels: map[string]string{
		docker.LabelManaged: "true", docker.LabelKind: docker.KindReleaseRuntimeContainer,
		docker.LabelEnvironmentID: cleanEnv, docker.LabelReleaseID: cleanRelease, docker.LabelRuntimeInstanceID: cleanInstance,
		docker.LabelImageDigest: cleanDigest, docker.LabelConfigDigest: cleanConfig,
	}}
}

func (f *cleanDockerFake) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *cleanDockerFake) InspectContainer(_ context.Context, id string) (docker.ContainerInspect, error) {
	f.rec("inspect")
	if f.inspectErr != nil {
		return docker.ContainerInspect{}, f.inspectErr
	}
	if !f.present {
		return docker.ContainerInspect{}, fmt.Errorf("%w: inspect", docker.ErrContainerNotFound)
	}
	var out docker.ContainerInspect
	out.ID, out.Image = cleanID, f.imageID
	out.State.Running = f.running
	out.State.Status = map[bool]string{true: "running", false: "exited"}[f.running]
	return out, nil
}

func (f *cleanDockerFake) ListContainersByLabels(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	f.rec("list")
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(labels) < 5 {
		return nil, errors.New("selector too broad")
	}
	if !f.present {
		return nil, nil
	}
	for k, v := range labels {
		if f.labels[k] != v {
			return nil, nil
		}
	}
	out := []docker.ContainerSummary{{ID: cleanID, Labels: f.labels}}
	for i := 0; i < f.extraMatch; i++ {
		out = append(out, docker.ContainerSummary{ID: fmt.Sprintf("%064d", i+1), Labels: f.labels})
	}
	return out, nil
}

func (f *cleanDockerFake) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.rec("stop:" + id)
	if f.stopErr == nil {
		f.running = false
	}
	return f.stopErr
}

func (f *cleanDockerFake) WaitStopped(_ context.Context, _ string, _, _ time.Duration) (docker.ContainerInspect, error) {
	f.rec("wait")
	return docker.ContainerInspect{}, nil
}

func (f *cleanDockerFake) RemoveContainer(_ context.Context, id string) error {
	f.rec("rmc:" + id)
	if f.removeErr != nil {
		return f.removeErr
	}
	f.present, f.removedID = false, id
	return nil
}

func (f *cleanDockerFake) RemoveImage(_ context.Context, ref string) error {
	f.rec("rmi:" + ref)
	f.removedRefs = append(f.removedRefs, ref)
	return f.imageErr
}

type cleanCP struct {
	mu      sync.Mutex
	items   []agentclient.CleanupItem
	claimEr error
	reports []cleanReport
	repErrs []error
}

type cleanReport struct {
	id                 int64
	result, class, msg string
	kept               bool
}

func (c *cleanCP) ClaimCleanups(_ context.Context, limit int) ([]agentclient.CleanupItem, error) {
	if limit <= 0 {
		return nil, errors.New("limit")
	}
	return c.items, c.claimEr
}

func (c *cleanCP) ReportCleanupResult(_ context.Context, id int64, result, class string, kept bool, summary string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.repErrs) > 0 {
		err := c.repErrs[0]
		c.repErrs = c.repErrs[1:]
		if err != nil {
			return err
		}
	}
	c.reports = append(c.reports, cleanReport{id, result, class, summary, kept})
	return nil
}

func cleanItem() agentclient.CleanupItem {
	return agentclient.CleanupItem{CleanupID: 9, ReleaseID: cleanRelease, EnvironmentID: cleanEnv, RuntimeInstanceID: cleanInstance, ImageDigest: cleanDigest}
}

func newCleaner(dk *cleanDockerFake, cp *cleanCP) *Cleaner {
	return &Cleaner{Docker: dk, ControlPlane: cp, Logger: log.New(io.Discard, "", 0)}
}

func has(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestCleanerStoppedContainerRemovedThenImage(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	newCleaner(dk, cp).Pass(context.Background())
	if len(cp.reports) != 1 || cp.reports[0].result != "removed" || cp.reports[0].kept || cp.reports[0].id != 9 {
		t.Fatalf("reports = %+v", cp.reports)
	}
	if has(dk.calls, "stop:") {
		t.Fatal("a stopped container was stopped again")
	}
	if dk.removedID != cleanID || len(dk.removedRefs) != 1 || dk.removedRefs[0] != "sha256:"+cleanConfig {
		t.Fatalf("removed %q images %v", dk.removedID, dk.removedRefs)
	}
}

func TestCleanerRunningContainerIsStoppedThenRemoved(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.running = true
	newCleaner(dk, cp).Pass(context.Background())
	if cp.reports[0].result != "removed" {
		t.Fatalf("reports = %+v", cp.reports)
	}
	stop, rm := -1, -1
	for i, c := range dk.calls {
		if strings.HasPrefix(c, "stop:") && stop < 0 {
			stop = i
		}
		if strings.HasPrefix(c, "rmc:") {
			rm = i
		}
	}
	if stop < 0 || rm < 0 || stop > rm {
		t.Fatalf("calls = %v, want stop before remove", dk.calls)
	}
}

func TestCleanerStopFailureLeavesContainerAndReportsFailed(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.running = true
	dk.stopErr = errors.New("engine down")
	newCleaner(dk, cp).Pass(context.Background())
	if r := cp.reports[0]; r.result != "failed" || r.class != "stop" {
		t.Fatalf("report = %+v", r)
	}
	if has(dk.calls, "rmc:") || has(dk.calls, "rmi:") {
		t.Fatalf("removal attempted after a failed stop: %v", dk.calls)
	}
}

func TestCleanerMissingContainerIsSuccess(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.present = false
	newCleaner(dk, cp).Pass(context.Background())
	if r := cp.reports[0]; r.result != "removed" || !r.kept || !strings.Contains(r.msg, "already absent") {
		t.Fatalf("report = %+v", r)
	}
	if has(dk.calls, "rmi:") {
		t.Fatalf("an image was removed although the container (and its verified image id) is gone: %v", dk.calls)
	}
	if has(dk.calls, "rmc:") {
		t.Fatal("removed a container that does not exist")
	}
}

func TestCleanerImageInUseIsKeptAndStillSuccess(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.imageErr = fmt.Errorf("%w: image-remove", docker.ErrImageInUse)
	newCleaner(dk, cp).Pass(context.Background())
	if r := cp.reports[0]; r.result != "removed" || !r.kept {
		t.Fatalf("report = %+v", r)
	}
	dk2, cp2 := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk2.imageErr = &docker.EngineError{Class: docker.ErrClassTransient, Op: "image-remove", Err: errors.New("boom")}
	var logBuf strings.Builder
	c2 := newCleaner(dk2, cp2)
	c2.Logger = log.New(&logBuf, "", 0)
	c2.Pass(context.Background())
	if !strings.Contains(logBuf.String(), "removal failed") || !strings.Contains(logBuf.String(), "boom") {
		t.Fatalf("image failure not logged: %q", logBuf.String())
	}
	if r := cp2.reports[0]; r.result != "removed" || !r.kept {
		t.Fatalf("other image error: %+v", r)
	}
}

func TestCleanerNeverTouchesWhatLacksTheExactLabels(t *testing.T) {
	for name, mutate := range map[string]func(*cleanDockerFake){
		"wrong release label":      func(f *cleanDockerFake) { f.labels[docker.LabelReleaseID] = "other" },
		"wrong environment label":  func(f *cleanDockerFake) { f.labels[docker.LabelEnvironmentID] = "other" },
		"wrong instance label":     func(f *cleanDockerFake) { f.labels[docker.LabelRuntimeInstanceID] = "78" },
		"not managed":              func(f *cleanDockerFake) { f.labels[docker.LabelManaged] = "false" },
		"wrong kind":               func(f *cleanDockerFake) { f.labels[docker.LabelKind] = "other" },
		"missing labels":           func(f *cleanDockerFake) { f.labels = map[string]string{} },
		"image digest differs":     func(f *cleanDockerFake) { f.labels[docker.LabelImageDigest] = strings.Repeat("c", 64) },
		"two containers match":     func(f *cleanDockerFake) { f.extraMatch = 1 },
		"image id differs for rmi": nil,
	} {
		dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
		dk.running = true
		if mutate == nil {
			dk.imageID = "sha256:" + strings.Repeat("d", 64)
			newCleaner(dk, cp).Pass(context.Background())
			if r := cp.reports[0]; r.result != "removed" || !r.kept || len(dk.removedRefs) != 0 {
				t.Fatalf("%s: report %+v refs %v (an image whose id is not the labelled config digest must be kept)", name, r, dk.removedRefs)
			}
			continue
		}
		mutate(dk)
		newCleaner(dk, cp).Pass(context.Background())
		if r := cp.reports[0]; r.result != "skipped" || r.class != "identity" {
			t.Fatalf("%s: report = %+v", name, r)
		}
		if has(dk.calls, "stop:") || has(dk.calls, "rmc:") || has(dk.calls, "rmi:") {
			t.Fatalf("%s: touched a container or image: %v", name, dk.calls)
		}
	}
}

func TestCleanerRejectsInvalidClaims(t *testing.T) {
	for name, mutate := range map[string]func(*agentclient.CleanupItem){
		"zero id": func(i *agentclient.CleanupItem) { i.CleanupID = 0 }, "empty release": func(i *agentclient.CleanupItem) { i.ReleaseID = "" },
		"bad instance": func(i *agentclient.CleanupItem) { i.RuntimeInstanceID = "07" }, "short digest": func(i *agentclient.CleanupItem) { i.ImageDigest = "ab" },
		"uppercase digest": func(i *agentclient.CleanupItem) { i.ImageDigest = strings.ToUpper(cleanDigest) },
	} {
		it := cleanItem()
		mutate(&it)
		dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{it}}
		newCleaner(dk, cp).Pass(context.Background())
		if len(cp.reports) != 1 || cp.reports[0].result != "skipped" || len(dk.calls) != 0 {
			t.Fatalf("%s: reports %+v calls %v", name, cp.reports, dk.calls)
		}
	}
}

func TestCleanerEngineErrorsReportFailedWithoutRemoval(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.inspectErr = &docker.EngineError{Class: docker.ErrClassTransient, Op: "container-inspect", Err: errors.New("x")}
	newCleaner(dk, cp).Pass(context.Background())
	if r := cp.reports[0]; r.result != "failed" || r.class != "engine" || has(dk.calls, "rmc:") {
		t.Fatalf("report %+v calls %v", r, dk.calls)
	}
	dk, cp = newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	dk.removeErr = fmt.Errorf("%w: container-remove", docker.ErrContainerRunning)
	newCleaner(dk, cp).Pass(context.Background())
	if r := cp.reports[0]; r.result != "failed" || r.class != "stop" || has(dk.calls, "rmi:") {
		t.Fatalf("running refusal: %+v calls %v", r, dk.calls)
	}
}

// A report that could not be delivered is not lost: the next claim hands the
// same cleanup out again and the (idempotent) removal is redone and reported.
func TestCleanerLostReportIsRedoneOnTheNextPass(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}, repErrs: []error{errors.New("503")}}
	c := newCleaner(dk, cp)
	c.Pass(context.Background())
	if len(cp.reports) != 0 || dk.present {
		t.Fatalf("first pass: reports %v present %v", cp.reports, dk.present)
	}
	c.Pass(context.Background()) // same item re-delivered; the container is already gone
	if len(cp.reports) != 1 || cp.reports[0].result != "removed" || cp.reports[0].id != 9 {
		t.Fatalf("second pass reports = %+v", cp.reports)
	}
}

func TestCleanerClaimFailureIsLoggedNotFatal(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{claimEr: errors.New("down")}
	newCleaner(dk, cp).Pass(context.Background())
	if len(dk.calls) != 0 || len(cp.reports) != 0 {
		t.Fatal("acted without a claim")
	}
}

func TestCleanerRunPassesAtStartAndStopsOnCancel(t *testing.T) {
	dk, cp := newCleanDocker(), &cleanCP{items: []agentclient.CleanupItem{cleanItem()}}
	c := newCleaner(dk, cp)
	c.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cp.mu.Lock()
		n := len(cp.reports)
		cp.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if len(cp.reports) != 1 {
		t.Fatalf("reports = %+v, want the start-up pass", cp.reports)
	}
	if (&Cleaner{}).Validate() == nil {
		t.Fatal("empty cleaner validated")
	}
}

// Removal capability is exactly the two non-force calls (no force parameter, no kill).
func TestCleanupDockerSeam(t *testing.T) {
	typ := reflect.TypeOf((*CleanupDocker)(nil)).Elem()
	want := map[string]bool{"InspectContainer": true, "ListContainersByLabels": true, "StopContainer": true, "WaitStopped": true, "RemoveContainer": true, "RemoveImage": true}
	if typ.NumMethod() != len(want) {
		t.Fatalf("seam has %d methods", typ.NumMethod())
	}
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !want[m.Name] || strings.Contains(strings.ToLower(m.Name), "kill") || strings.Contains(strings.ToLower(m.Name), "force") {
			t.Fatalf("unexpected method %q", m.Name)
		}
		if strings.HasPrefix(m.Name, "Remove") && m.Type.NumIn() != 2 { // ctx, id (interface method type has no receiver)
			t.Fatalf("%s takes %d args, want (ctx, id)", m.Name, m.Type.NumIn())
		}
	}
}
