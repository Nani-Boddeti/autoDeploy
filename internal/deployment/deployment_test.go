package deployment

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

var (
	testTime     = time.Date(2026, time.July, 24, 10, 0, 0, 0, time.UTC)
	testIdentity = Identity{
		DeploymentID:  "deployment-1",
		ProjectID:     "project-1",
		EnvironmentID: "production",
		ServerID:      "server-1",
		CommitSHA:     "abcdef123456",
	}
)

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name     string
		identity Identity
		created  time.Time
	}{
		{name: "deployment ID", identity: Identity{ProjectID: "p", EnvironmentID: "e", ServerID: "s", CommitSHA: "c"}, created: testTime},
		{name: "project ID", identity: Identity{DeploymentID: "d", EnvironmentID: "e", ServerID: "s", CommitSHA: "c"}, created: testTime},
		{name: "environment ID", identity: Identity{DeploymentID: "d", ProjectID: "p", ServerID: "s", CommitSHA: "c"}, created: testTime},
		{name: "server ID", identity: Identity{DeploymentID: "d", ProjectID: "p", EnvironmentID: "e", CommitSHA: "c"}, created: testTime},
		{name: "commit SHA", identity: Identity{DeploymentID: "d", ProjectID: "p", EnvironmentID: "e", ServerID: "s"}, created: testTime},
		{name: "timestamp", identity: testIdentity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.identity, test.created)
			if !errors.Is(err, ErrInvalidDeployment) {
				t.Fatalf("New() error = %v, want ErrInvalidDeployment", err)
			}
		})
	}
}

func TestNewCreatesRevisionedQueuedDeployment(t *testing.T) {
	deployment := newTestDeployment(t)
	if got := deployment.Status(); got != StatusQueued {
		t.Errorf("Status() = %q, want %q", got, StatusQueued)
	}
	if got := deployment.Revision(); got != 1 {
		t.Errorf("Revision() = %d, want 1", got)
	}
	events := deployment.Events()
	if len(events) != 1 || events[0] != (Event{To: StatusQueued, At: testTime}) {
		t.Errorf("Events() = %#v, want queued creation event", events)
	}
}

func TestTimestampsAreCanonicalUTCToMicroseconds(t *testing.T) {
	created := time.Date(2026, time.July, 24, 15, 30, 0, 123456789, time.FixedZone("offset", 19800))
	value, err := New(testIdentity, created)
	if err != nil {
		t.Fatal(err)
	}
	want := created.UTC().Truncate(time.Microsecond)
	if !value.CreatedAt().Equal(want) || value.CreatedAt().Location() != time.UTC {
		t.Fatalf("created timestamp = %v, want %v UTC", value.CreatedAt(), want)
	}
	next, err := value.Transition(StatusAssigned, created.Add(time.Nanosecond))
	if err != nil || !next.UpdatedAt().Equal(want) {
		t.Fatalf("canonical transition = %v, %v", next.UpdatedAt(), err)
	}
}

func TestTransitionMatrix(t *testing.T) {
	allowed := map[Status]map[Status]bool{
		StatusQueued:         {StatusAssigned: true, StatusCancelled: true, StatusSuperseded: true},
		StatusAssigned:       {StatusFetching: true, StatusCancelled: true, StatusFailed: true},
		StatusFetching:       {StatusBuilding: true, StatusCancelled: true, StatusFailed: true},
		StatusBuilding:       {StatusStarting: true, StatusCancelled: true, StatusFailed: true},
		StatusStarting:       {StatusHealthChecking: true, StatusCancelled: true, StatusFailed: true},
		StatusHealthChecking: {StatusActivating: true, StatusCancelled: true, StatusFailed: true},
		StatusActivating:     {StatusHealthy: true, StatusFailed: true},
		StatusHealthy:        {StatusRolledBack: true},
	}
	statuses := []Status{StatusQueued, StatusAssigned, StatusFetching, StatusBuilding, StatusStarting, StatusHealthChecking, StatusActivating, StatusHealthy, StatusFailed, StatusCancelled, StatusSuperseded, StatusRolledBack}
	for _, from := range statuses {
		for _, to := range statuses {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				deployment := deploymentAt(t, from)
				next, err := deployment.Transition(to, deployment.UpdatedAt())
				if to == from {
					if err != nil || !reflect.DeepEqual(next, deployment) {
						t.Fatalf("idempotent transition = %#v, %v", next, err)
					}
					return
				}
				if allowed[from][to] {
					if err != nil || next.Status() != to || next.Revision() != deployment.Revision()+1 {
						t.Fatalf("allowed transition = %#v, %v", next, err)
					}
					return
				}
				if err == nil || !reflect.DeepEqual(next, deployment) {
					t.Fatalf("invalid transition = %#v, %v; want unchanged with error", next, err)
				}
				if isTerminal(from) {
					if !errors.Is(err, ErrTerminalState) {
						t.Fatalf("error = %v, want ErrTerminalState", err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v, want ErrInvalidTransition", err)
				}
			})
		}
	}
}

func TestTransitionTimestampPolicy(t *testing.T) {
	deployment := newTestDeployment(t)
	for _, at := range []time.Time{time.Time{}, testTime.Add(-time.Nanosecond)} {
		next, err := deployment.Transition(StatusAssigned, at)
		if !errors.Is(err, ErrInvalidTimestamp) || !reflect.DeepEqual(next, deployment) {
			t.Fatalf("Transition at %v = %#v, %v", at, next, err)
		}
	}

	// Equal timestamps are valid; Revision is the concurrency/CAS token.
	next, err := deployment.Transition(StatusAssigned, testTime)
	if err != nil || next.Revision() != 2 || !next.UpdatedAt().Equal(testTime) {
		t.Fatalf("equal timestamp transition = %#v, %v", next, err)
	}
}

func TestTransitionRejectsInvalidAggregateAndUnknownStatusesBeforeNoOp(t *testing.T) {
	zero := Deployment{}
	next, err := zero.Transition("", testTime)
	if !errors.Is(err, ErrInvalidDeployment) || !reflect.DeepEqual(next, zero) {
		t.Fatalf("zero Transition() = %#v, %v", next, err)
	}

	deployment := newTestDeployment(t)
	next, err = deployment.Transition(Status("unknown"), testTime)
	if !errors.Is(err, ErrUnknownStatus) || !reflect.DeepEqual(next, deployment) {
		t.Fatalf("unknown target = %#v, %v", next, err)
	}

	invalid := deployment
	invalid.status = Status("unknown")
	next, err = invalid.Transition(Status("unknown"), testTime)
	if !errors.Is(err, ErrInvalidDeployment) || !errors.Is(err, ErrUnknownStatus) || !reflect.DeepEqual(next, invalid) {
		t.Fatalf("unknown aggregate status = %#v, %v", next, err)
	}
}

func TestTransitionPreservesOriginalAndEventsAreDefensive(t *testing.T) {
	original := newTestDeployment(t)
	next, err := original.Transition(StatusAssigned, testTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if original.Status() != StatusQueued || original.Revision() != 1 || len(original.Events()) != 1 {
		t.Errorf("original mutated: %#v", original)
	}
	events := next.Events()
	events[0].To = StatusFailed
	if got := next.Events()[0].To; got != StatusQueued {
		t.Errorf("Events() exposed mutable backing slice: %q", got)
	}
}

func TestSnapshotRoundTripAndDefensiveCopy(t *testing.T) {
	deployment := deploymentAt(t, StatusHealthy)
	snapshot := mustSnapshot(t, deployment)
	restored, err := Rehydrate(snapshot)
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if got := mustSnapshot(t, restored); !reflect.DeepEqual(got, snapshot) {
		t.Errorf("round trip snapshot = %#v, want %#v", got, snapshot)
	}
	snapshot.Events[0].To = StatusFailed
	if got := deployment.Events()[0].To; got != StatusQueued {
		t.Errorf("Snapshot() exposed mutable backing slice: %q", got)
	}
	if got := restored.Events()[0].To; got != StatusQueued {
		t.Errorf("Rehydrate() retained caller Events backing slice: %q", got)
	}
}

func TestSnapshotRejectsInvalidAggregate(t *testing.T) {
	invalid := Deployment{}
	_, err := invalid.Snapshot()
	if !errors.Is(err, ErrInvalidDeployment) {
		t.Fatalf("Snapshot() error = %v, want ErrInvalidDeployment", err)
	}
}

func TestRehydrateRejectsCorruptSnapshots(t *testing.T) {
	valid := mustSnapshot(t, deploymentAt(t, StatusAssigned))
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "unsupported schema version", mutate: func(s *Snapshot) { s.Version++ }},
		{name: "missing identity", mutate: func(s *Snapshot) { s.Identity.ServerID = "" }},
		{name: "unknown status", mutate: func(s *Snapshot) { s.Status = "unknown" }},
		{name: "zero revision", mutate: func(s *Snapshot) { s.Revision = 0 }},
		{name: "revision mismatch", mutate: func(s *Snapshot) { s.Revision++ }},
		{name: "missing creation event", mutate: func(s *Snapshot) { s.Events = nil }},
		{name: "wrong creation event", mutate: func(s *Snapshot) { s.Events[0].To = StatusAssigned }},
		{name: "event status mismatch", mutate: func(s *Snapshot) { s.Status = StatusBuilding }},
		{name: "updated timestamp mismatch", mutate: func(s *Snapshot) { s.UpdatedAt = s.UpdatedAt.Add(time.Minute) }},
		{name: "event time regression", mutate: func(s *Snapshot) { s.Events[1].At = s.CreatedAt.Add(-time.Nanosecond) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := valid
			snapshot.Events = append([]Event(nil), valid.Events...)
			test.mutate(&snapshot)
			_, err := Rehydrate(snapshot)
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Rehydrate() error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

func newTestDeployment(t *testing.T) Deployment {
	t.Helper()
	deployment, err := New(testIdentity, testTime)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return deployment
}

func mustSnapshot(t *testing.T, deployment Deployment) Snapshot {
	t.Helper()
	snapshot, err := deployment.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func deploymentAt(t *testing.T, target Status) Deployment {
	t.Helper()
	deployment := newTestDeployment(t)
	path := []Status{StatusAssigned, StatusFetching, StatusBuilding, StatusStarting, StatusHealthChecking, StatusActivating, StatusHealthy}
	for _, status := range path {
		var err error
		deployment, err = deployment.Transition(status, deployment.UpdatedAt())
		if err != nil {
			t.Fatalf("Transition(%q) error = %v", status, err)
		}
		if status == target {
			return deployment
		}
	}
	switch target {
	case StatusQueued:
		return newTestDeployment(t)
	case StatusFailed:
		deployment = deploymentAt(t, StatusBuilding)
	case StatusCancelled:
		deployment = deploymentAt(t, StatusHealthChecking)
	case StatusSuperseded:
		deployment = newTestDeployment(t)
	case StatusRolledBack:
		deployment = deploymentAt(t, StatusHealthy)
	default:
		t.Fatalf("unsupported target %q", target)
	}
	next, err := deployment.Transition(target, deployment.UpdatedAt())
	if err != nil {
		t.Fatalf("Transition(%q) error = %v", target, err)
	}
	return next
}
