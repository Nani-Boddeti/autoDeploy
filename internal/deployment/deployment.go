// Package deployment contains deterministic deployment lifecycle rules.
package deployment

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidDeployment reports an invalid in-memory aggregate.
	ErrInvalidDeployment = errors.New("invalid deployment")
	// ErrInvalidSnapshot reports corrupt or inconsistent persisted aggregate data.
	ErrInvalidSnapshot = errors.New("invalid deployment snapshot")
	// ErrUnknownStatus reports a status outside the deployment lifecycle.
	ErrUnknownStatus = errors.New("unknown deployment status")
	// ErrInvalidTransition reports an unsupported deployment state change.
	ErrInvalidTransition = errors.New("invalid deployment transition")
	// ErrTerminalState reports an attempted change after a final state.
	ErrTerminalState = errors.New("deployment is in a terminal state")
	// ErrInvalidTimestamp reports an event timestamp that precedes the current state.
	ErrInvalidTimestamp = errors.New("transition timestamp precedes current state")
)

// ID identifies a deployment.
type ID string

// Revision is an optimistic-lock token. It increments on creation and real transitions only.
type Revision uint64

// SchemaVersion identifies the persisted Snapshot format independently of Revision.
type SchemaVersion uint16

// CurrentSchemaVersion is the only Snapshot format supported by this package.
const CurrentSchemaVersion SchemaVersion = 1

// Status describes a deployment's lifecycle position.
type Status string

const (
	StatusQueued         Status = "queued"
	StatusAssigned       Status = "assigned"
	StatusFetching       Status = "fetching"
	StatusBuilding       Status = "building"
	StatusStarting       Status = "starting"
	StatusHealthChecking Status = "health_checking"
	StatusActivating     Status = "activating"
	StatusHealthy        Status = "healthy"
	StatusFailed         Status = "failed"
	StatusCancelled      Status = "cancelled"
	StatusSuperseded     Status = "superseded"
	StatusRolledBack     Status = "rolled_back"
)

// Identity contains immutable deployment scope and source revision identifiers.
type Identity struct {
	DeploymentID  ID
	ProjectID     string
	EnvironmentID string
	ServerID      string
	CommitSHA     string
}

// Event records an immutable state change. The creation event has an empty From value.
type Event struct {
	From Status
	To   Status
	At   time.Time
}

// Snapshot is the complete, versioned persistence representation of a deployment.
// Rehydrate validates it without replaying external effects.
type Snapshot struct {
	Version   SchemaVersion
	Identity  Identity
	Status    Status
	CreatedAt time.Time
	UpdatedAt time.Time
	Revision  Revision
	Events    []Event
}

// Deployment is an immutable deployment aggregate. Its transition method returns a new value.
type Deployment struct {
	identity  Identity
	status    Status
	createdAt time.Time
	updatedAt time.Time
	revision  Revision
	events    []Event
}

// New creates a queued deployment at the supplied deterministic timestamp. Its revision is one.
func New(identity Identity, createdAt time.Time) (Deployment, error) {
	if err := validateIdentity(identity); err != nil {
		return Deployment{}, fmt.Errorf("%w: %v", ErrInvalidDeployment, err)
	}
	if createdAt.IsZero() {
		return Deployment{}, fmt.Errorf("%w: creation timestamp is required", ErrInvalidDeployment)
	}

	return Deployment{
		identity:  identity,
		status:    StatusQueued,
		createdAt: createdAt,
		updatedAt: createdAt,
		revision:  1,
		events:    []Event{{To: StatusQueued, At: createdAt}},
	}, nil
}

// Rehydrate validates and restores a persisted deployment snapshot.
func Rehydrate(snapshot Snapshot) (Deployment, error) {
	if snapshot.Version != CurrentSchemaVersion {
		return Deployment{}, fmt.Errorf("%w: unsupported schema version %d", ErrInvalidSnapshot, snapshot.Version)
	}
	d := Deployment{
		identity:  snapshot.Identity,
		status:    snapshot.Status,
		createdAt: snapshot.CreatedAt,
		updatedAt: snapshot.UpdatedAt,
		revision:  snapshot.Revision,
		events:    append([]Event(nil), snapshot.Events...),
	}
	if err := d.validate(); err != nil {
		return Deployment{}, fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
	}
	return d, nil
}

// Identity returns the immutable identifiers associated with the deployment.
func (d Deployment) Identity() Identity { return d.identity }

// Status returns the deployment's current state.
func (d Deployment) Status() Status { return d.status }

// CreatedAt returns when the deployment was created.
func (d Deployment) CreatedAt() time.Time { return d.createdAt }

// UpdatedAt returns when the current state was recorded.
func (d Deployment) UpdatedAt() time.Time { return d.updatedAt }

// Revision returns the optimistic-lock token for this aggregate version.
func (d Deployment) Revision() Revision { return d.revision }

// Events returns a copy of the transition audit trail.
func (d Deployment) Events() []Event { return append([]Event(nil), d.events...) }

// Snapshot validates the aggregate and returns a complete defensive copy suitable for persistence.
func (d Deployment) Snapshot() (Snapshot, error) {
	if err := d.validate(); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrInvalidDeployment, err)
	}
	return Snapshot{
		Version:   CurrentSchemaVersion,
		Identity:  d.identity,
		Status:    d.status,
		CreatedAt: d.createdAt,
		UpdatedAt: d.updatedAt,
		Revision:  d.revision,
		Events:    d.Events(),
	}, nil
}

// Transition returns an updated deployment. Equal timestamps are allowed, so callers can use a
// deterministic clock with coarse resolution. Revision, not timestamps, is the CAS token.
// Repeating the current status is a no-op and does not append an event or increment revision.
func (d Deployment) Transition(to Status, at time.Time) (Deployment, error) {
	if err := d.validate(); err != nil {
		return d, fmt.Errorf("%w: %w", ErrInvalidDeployment, err)
	}
	if !isKnownStatus(to) {
		return d, fmt.Errorf("%w: %q", ErrUnknownStatus, to)
	}
	if to == d.status {
		return d, nil
	}
	if at.IsZero() || at.Before(d.updatedAt) {
		return d, fmt.Errorf("%w: current=%s requested=%s", ErrInvalidTimestamp, d.status, to)
	}
	if isTerminal(d.status) {
		return d, fmt.Errorf("%w: %s", ErrTerminalState, d.status)
	}
	if !canTransition(d.status, to) {
		return d, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, d.status, to)
	}

	next := d
	next.status = to
	next.updatedAt = at
	next.revision++
	next.events = append(append([]Event(nil), d.events...), Event{From: d.status, To: to, At: at})
	return next, nil
}

func (d Deployment) validate() error {
	if err := validateIdentity(d.identity); err != nil {
		return err
	}
	if !isKnownStatus(d.status) {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, d.status)
	}
	if d.createdAt.IsZero() || d.updatedAt.IsZero() || d.updatedAt.Before(d.createdAt) {
		return errors.New("invalid deployment timestamps")
	}
	if d.revision == 0 || d.revision != Revision(len(d.events)) {
		return errors.New("revision must equal event count")
	}
	if len(d.events) == 0 {
		return errors.New("creation event is required")
	}
	first := d.events[0]
	if first.From != "" || first.To != StatusQueued || !first.At.Equal(d.createdAt) {
		return errors.New("invalid creation event")
	}

	current := StatusQueued
	previousAt := d.createdAt
	for index, event := range d.events[1:] {
		if event.At.IsZero() || event.At.Before(previousAt) {
			return fmt.Errorf("event %d timestamp precedes prior event", index+1)
		}
		if event.From != current || !isKnownStatus(event.To) || !canTransition(current, event.To) {
			return fmt.Errorf("invalid event transition %s -> %s", event.From, event.To)
		}
		current = event.To
		previousAt = event.At
	}
	if current != d.status || !previousAt.Equal(d.updatedAt) {
		return errors.New("status or updated timestamp disagrees with events")
	}
	return nil
}

func validateIdentity(identity Identity) error {
	switch {
	case identity.DeploymentID == "":
		return errors.New("deployment ID is required")
	case identity.ProjectID == "":
		return errors.New("project ID is required")
	case identity.EnvironmentID == "":
		return errors.New("environment ID is required")
	case identity.ServerID == "":
		return errors.New("server ID is required")
	case identity.CommitSHA == "":
		return errors.New("commit SHA is required")
	default:
		return nil
	}
}

func isKnownStatus(status Status) bool {
	switch status {
	case StatusQueued, StatusAssigned, StatusFetching, StatusBuilding, StatusStarting,
		StatusHealthChecking, StatusActivating, StatusHealthy, StatusFailed, StatusCancelled,
		StatusSuperseded, StatusRolledBack:
		return true
	default:
		return false
	}
}

func isTerminal(status Status) bool {
	switch status {
	case StatusFailed, StatusCancelled, StatusSuperseded, StatusRolledBack:
		return true
	default:
		return false
	}
}

func canTransition(from, to Status) bool {
	switch from {
	case StatusQueued:
		return to == StatusAssigned || to == StatusCancelled || to == StatusSuperseded
	case StatusAssigned:
		return to == StatusFetching || to == StatusCancelled || to == StatusFailed
	case StatusFetching:
		return to == StatusBuilding || to == StatusCancelled || to == StatusFailed
	case StatusBuilding:
		return to == StatusStarting || to == StatusCancelled || to == StatusFailed
	case StatusStarting:
		return to == StatusHealthChecking || to == StatusCancelled || to == StatusFailed
	case StatusHealthChecking:
		return to == StatusActivating || to == StatusCancelled || to == StatusFailed
	case StatusActivating:
		return to == StatusHealthy || to == StatusFailed
	case StatusHealthy:
		return to == StatusRolledBack
	default:
		return false
	}
}
