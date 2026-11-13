package queue

import (
	"testing"
	"time"
)

func TestDeploymentExecutionJobIsImmutableAndBounded(t *testing.T) {
	j, err := NewDeploymentExecutionJob("owner", "project", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "intent-1", 100, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	j.ServerID = "another"
	if err := j.Validate(); err == nil {
		t.Fatal("mutated job accepted")
	}
}

func TestDeploymentExecutionJobRejectsUnsafeInputs(t *testing.T) {
	_, err := NewDeploymentExecutionJob("owner", "project", "environment", "server", "deployment", 4, "main", "0123456789abcdef0123456789abcdef01234567", "intent", 101, time.Now())
	if err == nil {
		t.Fatal("unsafe job accepted")
	}
}

func TestJobRejectsNoncanonicalTimeZeroSHAAndAmbiguousScopeDigest(t *testing.T) {
	j, err := NewDeploymentExecutionJob("owner/a", "project:one", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "intent", 0, time.Unix(1, 123456789))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := j
	bad.AvailableAt = time.Unix(1, 0).In(time.FixedZone("offset", 60))
	if bad.Validate() == nil {
		t.Fatal("non-UTC availability accepted")
	}
	bad = j
	bad.AvailableAt = j.AvailableAt.Add(time.Nanosecond)
	if bad.Validate() == nil {
		t.Fatal("non-microsecond availability accepted")
	}
	bad = j
	bad.CommitSHA = "0000000000000000000000000000000000000000"
	if bad.Validate() == nil {
		t.Fatal("zero SHA accepted")
	}
	a, err := NewDeploymentExecutionJob("a", "bc", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "intent", 0, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDeploymentExecutionJob("ab", "c", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "intent", 0, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if a.PayloadDigest == b.PayloadDigest {
		t.Fatal("ambiguous fields share digest")
	}
}

func TestRollbackExecutionJobCarriesAndBindsItsTarget(t *testing.T) {
	at := time.Unix(1, 0)
	j, err := NewRollbackExecutionJob("owner", "project", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "release-a", "key-1", 0, at)
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != KindRollbackExecution || j.RollbackTargetReleaseID != "release-a" || j.Validate() != nil {
		t.Fatalf("job %+v", j)
	}
	other, err := NewRollbackExecutionJob("owner", "project", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "release-b", "key-1", 0, at)
	if err != nil || other.PayloadDigest == j.PayloadDigest {
		t.Fatalf("the target must be part of the payload digest: %v", err)
	}
	// Target and kind must agree, both ways.
	bad := j
	bad.RollbackTargetReleaseID = ""
	if bad.Validate() == nil {
		t.Fatal("rollback job without a target accepted")
	}
	plain, err := NewDeploymentExecutionJob("owner", "project", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "key-1", 0, at)
	if err != nil {
		t.Fatal(err)
	}
	plain.RollbackTargetReleaseID = "release-a"
	if plain.Validate() == nil {
		t.Fatal("deployment job with a rollback target accepted")
	}
	if _, err = NewRollbackExecutionJob("owner", "project", "environment", "server", "deployment", 4, "refs/heads/main", "0123456789abcdef0123456789abcdef01234567", "", "key-1", 0, at); err == nil {
		t.Fatal("empty target accepted")
	}
}
