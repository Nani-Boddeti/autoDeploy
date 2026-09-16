// Package github contains normalized, credential-free GitHub webhook domain values.
package github

import (
	"crypto/sha256"
	"errors"
	"sort"
)

var ErrInvalidWebhook = errors.New("invalid GitHub webhook")

type EventType string

const (
	EventPush                     EventType = "push"
	EventInstallation             EventType = "installation"
	EventInstallationRepositories EventType = "installation_repositories"
)

type VerificationState string

const (
	VerificationPending  VerificationState = "pending"
	VerificationVerified VerificationState = "verified"
	VerificationRejected VerificationState = "rejected"
)

type ProcessingState string

const (
	ProcessingPending   ProcessingState = "pending"
	ProcessingProcessed ProcessingState = "processed"
	ProcessingRejected  ProcessingState = "rejected"
)

type Failure string

const (
	FailureNone         Failure = ""
	FailureSignature    Failure = "signature"
	FailureMalformed    Failure = "malformed"
	FailureUnsupported  Failure = "unsupported"
	FailureUnauthorized Failure = "unauthorized"
)

// Headers is transient ingress input. It must be validated/verified before a
// credential-free Delivery is constructed and must never be persisted.
type Headers struct {
	DeliveryID string
	Event      EventType
	Signature  string
}

func (h Headers) Validate() error {
	if !ValidOpaqueID(h.DeliveryID) || !validEvent(h.Event) || h.Signature == "" || len(h.Signature) > 256 {
		return ErrInvalidWebhook
	}
	return nil
}

type Digest [sha256.Size]byte

func BodyDigest(body []byte) Digest { return sha256.Sum256(body) }
func (d Digest) valid() bool        { return d != Digest{} }

// Delivery is the durable normalized receipt; it excludes headers, signatures,
// raw JSON, credentials, and all other ingress material.
type Delivery struct {
	DeliveryID   string
	Event        EventType
	BodyDigest   Digest
	Verification VerificationState
	Processing   ProcessingState
	Failure      Failure
}

func (d Delivery) Validate() error {
	if !ValidOpaqueID(d.DeliveryID) || !validEvent(d.Event) || !d.BodyDigest.valid() {
		return ErrInvalidWebhook
	}
	switch d.Verification {
	case VerificationPending:
		if d.Processing != ProcessingPending || d.Failure != FailureNone {
			return ErrInvalidWebhook
		}
	case VerificationRejected:
		if d.Processing != ProcessingRejected || d.Failure != FailureSignature {
			return ErrInvalidWebhook
		}
	case VerificationVerified:
		switch d.Processing {
		case ProcessingPending, ProcessingProcessed:
			if d.Failure != FailureNone {
				return ErrInvalidWebhook
			}
		case ProcessingRejected:
			if d.Failure != FailureMalformed && d.Failure != FailureUnsupported && d.Failure != FailureUnauthorized {
				return ErrInvalidWebhook
			}
		default:
			return ErrInvalidWebhook
		}
	default:
		return ErrInvalidWebhook
	}
	return nil
}

type Push struct {
	InstallationID, RepositoryID, OwnerID int64
	Ref, CommitSHA                        string
}

func (p Push) Validate() error {
	if p.InstallationID <= 0 || p.RepositoryID <= 0 || p.OwnerID <= 0 || !ValidBranchRef(p.Ref) || !ValidCommitSHA(p.CommitSHA) {
		return ErrInvalidWebhook
	}
	return nil
}

type Lifecycle struct {
	Event                                 EventType
	InstallationID, RepositoryID, OwnerID int64
	RepositoryIDs                         []int64
	Action                                string
}

const MaxLifecycleRepositories = 100

func (e Lifecycle) Validate() error {
	if e.InstallationID <= 0 || e.OwnerID <= 0 {
		return ErrInvalidWebhook
	}
	switch e.Event {
	case EventInstallation:
		if (len(e.RepositoryIDs) != 0 || e.RepositoryID != 0) || (e.Action != "created" && e.Action != "deleted" && e.Action != "suspend" && e.Action != "unsuspend") {
			return ErrInvalidWebhook
		}
	case EventInstallationRepositories:
		ids := e.RepositoryIDs
		if len(ids) == 0 && e.RepositoryID > 0 {
			ids = []int64{e.RepositoryID}
		}
		if len(ids) == 0 || len(ids) > MaxLifecycleRepositories || (e.Action != "added" && e.Action != "removed") {
			return ErrInvalidWebhook
		}
		for index, id := range ids {
			if id <= 0 || (index > 0 && ids[index-1] >= id) {
				return ErrInvalidWebhook
			}
		}
	default:
		return ErrInvalidWebhook
	}
	return nil
}

// CanonicalLifecycleRepositories validates owner-scoped IDs and sorts them so
// every storage path locks and persists the same stable order.
func CanonicalLifecycleRepositories(ids []int64) []int64 {
	value := append([]int64(nil), ids...)
	sort.Slice(value, func(i, j int) bool { return value[i] < value[j] })
	return value
}
func validEvent(v EventType) bool {
	return v == EventPush || v == EventInstallation || v == EventInstallationRepositories
}
