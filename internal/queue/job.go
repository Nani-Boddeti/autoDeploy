// Package queue contains immutable logical enqueue domain values.
package queue

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"autodeploy/internal/github"
)

var ErrInvalidJob = errors.New("invalid deployment execution job")

type SchemaVersion uint16

const CurrentSchemaVersion SchemaVersion = 1

type State string

const StateQueued State = "queued"

type Kind string

const KindDeploymentExecution Kind = "deployment_execution"

type Job struct {
	Version                                                   SchemaVersion
	Kind                                                      Kind
	OwnerID, ProjectID, EnvironmentID, ServerID, DeploymentID string
	RepositoryID                                              int64
	Ref, CommitSHA                                            string
	Priority                                                  int16
	AvailableAt                                               time.Time
	IdempotencyKey                                            string
	PayloadDigest                                             [sha256.Size]byte
	State                                                     State
}

func NewDeploymentExecutionJob(ownerID, projectID, environmentID, serverID, deploymentID string, repositoryID int64, ref, commitSHA, idempotencyKey string, priority int16, availableAt time.Time) (Job, error) {
	j := Job{Version: CurrentSchemaVersion, Kind: KindDeploymentExecution, OwnerID: ownerID, ProjectID: projectID, EnvironmentID: environmentID, ServerID: serverID, DeploymentID: deploymentID, RepositoryID: repositoryID, Ref: ref, CommitSHA: commitSHA, Priority: priority, AvailableAt: availableAt.UTC().Truncate(time.Microsecond), IdempotencyKey: idempotencyKey, State: StateQueued}
	j.PayloadDigest = payloadDigest(j)
	if err := j.Validate(); err != nil {
		return Job{}, err
	}
	return j, nil
}

func (j Job) Validate() error {
	if j.Version != CurrentSchemaVersion || j.Kind != KindDeploymentExecution || !github.ValidOpaqueID(j.OwnerID) || !github.ValidOpaqueID(j.ProjectID) || !github.ValidOpaqueID(j.EnvironmentID) || !github.ValidOpaqueID(j.ServerID) || !github.ValidOpaqueID(j.DeploymentID) || j.RepositoryID <= 0 || !github.ValidBranchRef(j.Ref) || !github.ValidCommitSHA(j.CommitSHA) || j.Priority < -100 || j.Priority > 100 || j.AvailableAt.IsZero() || j.AvailableAt.Location() != time.UTC || !j.AvailableAt.Equal(j.AvailableAt.Truncate(time.Microsecond)) || !github.ValidOpaqueID(j.IdempotencyKey) || j.State != StateQueued || j.PayloadDigest != payloadDigest(j) {
		return ErrInvalidJob
	}
	return nil
}

func payloadDigest(j Job) [sha256.Size]byte {
	parts := []string{strconv.FormatUint(uint64(j.Version), 10), string(j.Kind), j.OwnerID, j.ProjectID, j.EnvironmentID, j.ServerID, j.DeploymentID, strconv.FormatInt(j.RepositoryID, 10), j.Ref, j.CommitSHA, strconv.FormatInt(int64(j.Priority), 10), j.AvailableAt.Format(time.RFC3339Nano), j.IdempotencyKey, string(j.State)}
	b := make([]byte, 0, 512)
	for _, part := range parts {
		b = strconv.AppendInt(b, int64(len(part)), 10)
		b = append(b, ':')
		b = append(b, part...)
	}
	return sha256.Sum256(b)
}
