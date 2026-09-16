// Package githubhttp implements the isolated GitHub webhook HTTP boundary.
package githubhttp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"autodeploy/internal/github"
)

const (
	Path         = "/webhooks/github"
	MaxBodyBytes = 1 << 20
)

var errUnsupportedWebhook = errors.New("unsupported webhook")

// SecretRing contains mounted credential material only in process memory.
// Version values are diagnostics for the caller and are never persisted here.
type SecretRing struct {
	ActiveVersion string
	Active        []byte
	Retained      map[string][]byte
}

func (r SecretRing) valid() bool {
	if r.ActiveVersion == "" || len(r.Active) < 32 || len(r.Retained) > 4 {
		return false
	}
	if _, exists := r.Retained[r.ActiveVersion]; exists {
		return false
	}
	for version, secret := range r.Retained {
		if version == "" || len(secret) < 32 {
			return false
		}
	}
	return true
}

// Handler deliberately has no browser session or CSRF dependency.
type Handler struct {
	Secrets           SecretRing
	TrustedProxyCIDRs []netip.Prefix
	Processor         Processor
}

// Processor is the narrow, verified-only boundary between public ingress and
// durable webhook effects. It never receives headers, signatures, or JSON.
type Processor interface {
	ProcessPush(context.Context, github.Delivery, github.Push) error
	ProcessLifecycle(context.Context, github.Delivery, github.Lifecycle) error
	Reject(context.Context, github.Delivery) error
}

// TerminalClassifier lets the durable boundary identify sanitized permanent
// failures without exposing persistence internals to this HTTP package.
type TerminalClassifier interface{ IsTerminalWebhookError(error) bool }

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost || r.URL == nil || r.URL.Path != Path || r.URL.RawPath != "" || r.URL.RawQuery != "" || !h.https(r) || !h.Secrets.valid() {
		h.fail(w, http.StatusNotFound)
		return
	}
	headers, ok := canonicalHeaders(r.Header)
	if !ok {
		h.fail(w, http.StatusBadRequest)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	delivery := github.Delivery{DeliveryID: headers.DeliveryID, Event: headers.Event, BodyDigest: github.BodyDigest(body), Verification: github.VerificationRejected, Processing: github.ProcessingRejected, Failure: github.FailureSignature}
	if !verify(headers.Signature, body, h.Secrets) {
		h.fail(w, http.StatusUnauthorized)
		return
	}
	delivery.Verification, delivery.Processing, delivery.Failure = github.VerificationVerified, github.ProcessingPending, github.FailureNone
	if h.Processor == nil {
		h.fail(w, http.StatusUnauthorized)
		return
	}
	var processErr error
	parseFailure := false
	switch headers.Event {
	case github.EventPush:
		push, err := parsePush(body)
		if err == nil {
			processErr = h.Processor.ProcessPush(r.Context(), delivery, push)
		} else {
			processErr = err
			parseFailure = true
		}
	case github.EventInstallation, github.EventInstallationRepositories:
		lifecycle, err := parseLifecycle(headers.Event, body)
		if err == nil {
			processErr = h.Processor.ProcessLifecycle(r.Context(), delivery, lifecycle)
		} else {
			processErr = err
			parseFailure = true
		}
	default:
		processErr = errUnsupportedWebhook
		parseFailure = true
	}
	if processErr != nil {
		if parseFailure {
			delivery.Processing, delivery.Failure = github.ProcessingRejected, failureFor(processErr)
			if err := h.Processor.Reject(r.Context(), delivery); err != nil {
				h.fail(w, http.StatusServiceUnavailable)
				return
			}
			h.fail(w, http.StatusBadRequest)
			return
		}
		if terminal, ok := h.Processor.(TerminalClassifier); ok && terminal.IsTerminalWebhookError(processErr) {
			h.fail(w, http.StatusBadRequest)
			return
		}
		// Durable failures are retryable and never rewrite a committed receipt.
		h.fail(w, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) fail(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "invalid webhook\n")
}

func canonicalHeaders(header http.Header) (github.Headers, bool) {
	get := func(name string) (string, bool) { values := header.Values(name); return first(values) }
	delivery, ok := get("X-GitHub-Delivery")
	if !ok {
		return github.Headers{}, false
	}
	event, ok := get("X-GitHub-Event")
	if !ok {
		return github.Headers{}, false
	}
	signature, ok := get("X-Hub-Signature-256")
	if !ok {
		return github.Headers{}, false
	}
	h := github.Headers{DeliveryID: delivery, Event: github.EventType(event), Signature: signature}
	return h, h.Validate() == nil
}
func first(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" || len(values[0]) > 256 || strings.ContainsAny(values[0], "\r\n\x00") {
		return "", false
	}
	return values[0], true
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		w.WriteHeader(http.StatusBadRequest)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil || len(body) > MaxBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

func verify(signature string, body []byte, ring SecretRing) bool {
	if !strings.HasPrefix(signature, "sha256=") || len(signature) != len("sha256=")+sha256.Size*2 {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	valid := check(ring.Active, body, want)
	for _, secret := range ring.Retained {
		valid = check(secret, body, want) || valid
	}
	return valid
}
func check(secret, body, want []byte) bool {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

type pushPayload struct {
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
}

func parsePush(body []byte) (github.Push, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var payload pushPayload
	if err := dec.Decode(&payload); err != nil {
		return github.Push{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return github.Push{}, errors.New("trailing JSON")
	}
	if payload.Deleted {
		return github.Push{}, errors.New("deleted")
	}
	p := github.Push{InstallationID: payload.Installation.ID, RepositoryID: payload.Repository.ID, OwnerID: payload.Repository.Owner.ID, Ref: payload.Ref, CommitSHA: payload.After}
	return p, p.Validate()
}

type lifecyclePayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID int64 `json:"id"`
		} `json:"account"`
	} `json:"installation"`
	Repository struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
	RepositoriesRemoved []struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repositories_removed"`
	RepositoriesAdded []struct {
		ID    int64 `json:"id"`
		Owner struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repositories_added"`
}

func parseLifecycle(event github.EventType, body []byte) (github.Lifecycle, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var payload lifecyclePayload
	if err := dec.Decode(&payload); err != nil {
		return github.Lifecycle{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return github.Lifecycle{}, errors.New("trailing JSON")
	}
	value := github.Lifecycle{Event: event, InstallationID: payload.Installation.ID, Action: payload.Action}
	if event == github.EventInstallation {
		if payload.Action != "created" && payload.Action != "deleted" && payload.Action != "suspend" && payload.Action != "unsuspend" {
			return github.Lifecycle{}, errUnsupportedWebhook
		}
		value.OwnerID = payload.Installation.Account.ID
		return value, value.Validate()
	}
	if payload.Action != "added" && payload.Action != "removed" {
		return github.Lifecycle{}, errUnsupportedWebhook
	}
	items := payload.RepositoriesRemoved
	if payload.Action == "added" {
		items = payload.RepositoriesAdded
	}
	if len(items) == 0 || len(items) > github.MaxLifecycleRepositories {
		return github.Lifecycle{}, errors.New("repository count")
	}
	value.OwnerID = payload.Installation.Account.ID
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if item.Owner.ID != value.OwnerID {
			return github.Lifecycle{}, errors.New("repository owner")
		}
		ids = append(ids, item.ID)
	}
	value.RepositoryIDs = github.CanonicalLifecycleRepositories(ids)
	value.RepositoryID = value.RepositoryIDs[0] // receipt compatibility; full set is persisted separately.
	return value, value.Validate()
}
func failureFor(err error) github.Failure {
	if errors.Is(err, errUnsupportedWebhook) {
		return github.FailureUnsupported
	}
	return github.FailureMalformed
}
func (h Handler) https(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	trusted := false
	for _, cidr := range h.TrustedProxyCIDRs {
		if cidr.Contains(peer.Unmap()) {
			trusted = true
			break
		}
	}
	if !trusted {
		return false
	}
	values := r.Header.Values("X-Forwarded-Proto")
	return len(values) == 1 && values[0] == "https"
}
