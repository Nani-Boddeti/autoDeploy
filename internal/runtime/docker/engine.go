package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// apiVersion pins the Engine API version path prefix this client speaks.
//
// v1.44 is the API surface shipped by Docker Engine 25.0 (released January
// 2024). The Engine API is backward compatible: a client pinned to an
// older, still-supported version continues to work unchanged against newer
// daemons, so pinning to a stable, broadly deployed baseline rather than
// the newest version is the safer choice across a fleet of hosts that were
// each provisioned with whatever Engine release was current at the time.
// v1.44 is well inside Docker's supported-version window as of 2026 and
// carries no deprecation notice.
const apiVersion = "v1.44"

// dummyHost is the Host header/URL authority used for requests over the
// Unix socket transport. The Engine API ignores it; net/http requires a
// syntactically valid authority to build the request.
const dummyHost = "docker.autodeploy.invalid"

const (
	defaultRequestTimeout = 5 * time.Second
	// maxResponseBody bounds every response this client reads. Every
	// endpoint it calls (ping, version) returns a small, fixed-shape body;
	// anything larger indicates a malformed or hostile response.
	maxResponseBody = 1 << 20 // 1 MiB
)

// Client is an Engine API client bound to one Unix domain socket. The zero
// value is not usable; construct one with the platform-gated NewClient.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// newClient builds a Client without the platform gate NewClient applies. It
// is unexported so tests in this package (which run on any OS, since Unix
// domain sockets work on darwin too) can exercise the transport directly,
// while NewClient itself is only reachable on Linux builds.
func newClient(socketPath string) (*Client, error) {
	if socketPath == "" {
		return nil, &EngineError{Class: ErrClassConfiguration, Op: "new", Err: errors.New("empty socket path")}
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{
		httpClient: &http.Client{Transport: transport},
		baseURL:    "http://" + dummyHost + "/" + apiVersion,
	}, nil
}

// do issues one request. When the caller's context has no deadline, do
// applies defaultRequestTimeout for the whole call, body read included: the
// timeout's cancel is not deferred here (that would fire the instant do
// returns, before a caller reads a single byte of a trailing-body response
// such as a version/ping reply or, later, a build-log/wait stream) but is
// instead handed to the response body, which cancels it on Close. Every
// early-return path before that handoff cancels it directly so the context
// is never leaked.
func (c *Client) do(ctx context.Context, op, method, path string) (*http.Response, error) {
	return c.doBody(ctx, op, method, path, nil, "")
}

// doBody is do's superset: it additionally accepts a request body and its
// content type, for the JSON-bodied container/network create calls this
// file adds. Every other behavior (timeout handoff to the response body,
// dial-error classification) is identical to do, which is now a thin
// wrapper over this.
func (c *Client) doBody(ctx context.Context, op, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, &EngineError{Class: ErrClassConfiguration, Op: op, Err: err}
	}
	req.Host = dummyHost
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, classifyDoError(op, err)
	}
	if cancel != nil {
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

// getJSON issues a GET and decodes a JSON response into out.
func (c *Client) getJSON(ctx context.Context, op, path string, out any) error {
	resp, err := c.do(ctx, op, http.MethodGet, path)
	if err != nil {
		return err
	}
	if err := checkStatus(resp, op); err != nil {
		return err
	}
	return decodeJSON(resp, op, out)
}

// cancelOnCloseBody defers a request's timeout cancellation until the
// response body is closed, so the deadline covers the full body read
// instead of expiring the instant the headers come back.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// classifyDoError distinguishes a permanent local/dial failure from a
// transient one. ENOENT/EACCES/ENOTDIR mean the configured socket path is
// wrong or unusable and retrying unchanged will never help; connection
// refused/reset/timeout mean the daemon is unreachable right now, which is
// exactly the kind of blip a retry can outlive.
func classifyDoError(op string, err error) *EngineError {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &EngineError{Class: ErrClassTransient, Op: op, Err: err}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var errno syscall.Errno
		if errors.As(opErr.Err, &errno) {
			switch errno {
			case syscall.ENOENT, syscall.EACCES, syscall.ENOTDIR:
				return &EngineError{Class: ErrClassConfiguration, Op: op, Err: err}
			}
		}
	}
	return &EngineError{Class: ErrClassTransient, Op: op, Err: err}
}

// checkStatus consumes and closes the body of a non-2xx response, returning
// a classified EngineError. Callers of a successful response instead use
// decodeJSON or readText, which also bound and close the body.
func checkStatus(resp *http.Response, op string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	class := ErrClassConfiguration
	if resp.StatusCode >= 500 {
		class = ErrClassTransient
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return &EngineError{Class: class, Op: op, Status: resp.StatusCode, Err: errors.New(msg)}
}

func decodeJSON(resp *http.Response, op string, v any) error {
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody))
	if err := dec.Decode(v); err != nil {
		return &EngineError{Class: ErrClassMalformed, Op: op, Err: err}
	}
	return nil
}

func readText(resp *http.Response, op string) (string, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return "", &EngineError{Class: ErrClassMalformed, Op: op, Err: err}
	}
	return string(body), nil
}

// Ping calls the Engine API's liveness endpoint. It is the cheapest way to
// distinguish "socket unreachable" (transient) from "socket reachable but
// the response is not what a healthy engine returns" (malformed).
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "ping", http.MethodGet, "/_ping")
	if err != nil {
		return err
	}
	if err := checkStatus(resp, "ping"); err != nil {
		return err
	}
	body, err := readText(resp, "ping")
	if err != nil {
		return err
	}
	if strings.TrimSpace(body) != "OK" {
		return &EngineError{Class: ErrClassMalformed, Op: "ping", Err: fmt.Errorf("unexpected ping body %q", body)}
	}
	return nil
}

// VersionInfo is the subset of GET /version fields this client relies on.
type VersionInfo struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// Version calls GET /version and returns the engine's reported version
// information.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	resp, err := c.do(ctx, "version", http.MethodGet, "/version")
	if err != nil {
		return VersionInfo{}, err
	}
	if err := checkStatus(resp, "version"); err != nil {
		return VersionInfo{}, err
	}
	var v VersionInfo
	if err := decodeJSON(resp, "version", &v); err != nil {
		return VersionInfo{}, err
	}
	if v.APIVersion == "" || v.MinAPIVersion == "" {
		return VersionInfo{}, &EngineError{Class: ErrClassMalformed, Op: "version", Err: errors.New("engine version response missing API version fields")}
	}
	return v, nil
}

// Probe is the engine half of the plan's startup capability probe: it pings
// the daemon, reads its version, and confirms the engine's supported API
// version range covers the version this client pins, so a missing or
// incompatible engine fails closed at enrollment rather than mid-deployment.
//
// It deliberately does not check for buildx. buildx is a separate CLI
// plugin with no Engine HTTP API surface, so probing it does not belong in
// an Engine API HTTP client; it belongs with the build path (Step 5), which
// is the first and only caller that actually needs buildx.
func (c *Client) Probe(ctx context.Context) (VersionInfo, error) {
	if err := c.Ping(ctx); err != nil {
		return VersionInfo{}, err
	}
	v, err := c.Version(ctx)
	if err != nil {
		return VersionInfo{}, err
	}
	if !apiVersionSupported(v) {
		return VersionInfo{}, &EngineError{
			Class: ErrClassConfiguration,
			Op:    "probe",
			Err:   fmt.Errorf("engine API version range %s..%s does not support pinned client version %s", v.MinAPIVersion, v.APIVersion, apiVersion),
		}
	}
	return v, nil
}

type apiVersionNumber struct{ major, minor int }

func parseAPIVersion(s string) apiVersionNumber {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return apiVersionNumber{}
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || major <= 0 {
		return apiVersionNumber{}
	}
	return apiVersionNumber{major, minor}
}

func (a apiVersionNumber) less(b apiVersionNumber) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	return a.minor < b.minor
}

func apiVersionSupported(v VersionInfo) bool {
	client := parseAPIVersion(strings.TrimPrefix(apiVersion, "v"))
	max := parseAPIVersion(v.APIVersion)
	min := parseAPIVersion(v.MinAPIVersion)
	zero := apiVersionNumber{}
	if client == zero || max == zero || min == zero {
		return false
	}
	return !client.less(min) && !max.less(client)
}

// --- Container/network Engine API calls (Step 6a) ---
//
// The wire structs below are cut down to exactly the fields this package
// ever sets. There is deliberately no field anywhere in containerCreateRequest
// for PortBindings or PublishAllPorts, and NetworkMode is always a resolved
// Docker network ID rather than a free-form name: the only constructor for
// this struct is buildContainerCreatePayload (run.go), so a published port
// or host-networking container is not a validation this package might one
// day forget -- the wire struct itself has nowhere to put one.

type containerCreateRequest struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
	// Env carries "NAME=value" entries only -- see RunContainerRequest.Env
	// and buildEnvEntries, the only constructor. Never populated from
	// os.Environ() or any ambient process environment.
	Env        []string            `json:"Env,omitempty"`
	HostConfig containerHostConfig `json:"HostConfig"`
}

type containerHostConfig struct {
	// NetworkMode is always a resolved Docker network ID (see
	// requireNetworkID in run.go): never "host", "none", "bridge", or a
	// user-supplied name. Joining more than one network at creation is not
	// representable here -- there is exactly one NetworkMode field, and this
	// package makes no separate network-connect call.
	NetworkMode   string                 `json:"NetworkMode"`
	RestartPolicy containerRestartPolicy `json:"RestartPolicy"`
	NanoCPUs      int64                  `json:"NanoCpus,omitempty"`
	Memory        int64                  `json:"Memory"`
}

type containerRestartPolicy struct {
	Name string `json:"Name"`
}

type containerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// ErrContainerNameConflict is returned by CreateContainer when the Engine
// API refuses the create because a container already holds the requested
// name (HTTP 409). Under this package's naming scheme (run.go's
// containerName, derived deterministically from the immutable runtime-
// instance ID) that can only happen if the retry-lookup label-selector
// query -- which always runs first and should find any legitimate prior
// attempt by its full identity -- missed a container that nonetheless holds
// the name: local label/fence state disagrees with what Docker actually
// has. Callers must not paper over this by deleting or renaming anything;
// FailureClass maps it to corrupt_state precisely so it fails closed into
// reconciliation instead of silently overwriting the name collision.
var ErrContainerNameConflict = errors.New("docker engine: container name already in use")

// CreateContainer calls POST /containers/create?name=<name>. req.Env can
// carry secret plaintext (see containerCreateRequest.Env); see EngineError's
// doc comment for why a non-2xx response here still gets logged/handled the
// same untrusted-text-must-be-redacted way as every other EngineError, even
// though Docker does not echo request bodies back today.
func (c *Client) CreateContainer(ctx context.Context, name string, req containerCreateRequest) (string, error) {
	if name == "" {
		return "", &EngineError{Class: ErrClassConfiguration, Op: "container-create", Err: errors.New("empty container name")}
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return "", &EngineError{Class: ErrClassConfiguration, Op: "container-create", Err: err}
	}
	path := "/containers/create?name=" + url.QueryEscape(name)
	resp, err := c.doBody(ctx, "container-create", http.MethodPost, path, bytes.NewReader(encoded), "application/json")
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusConflict {
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return "", fmt.Errorf("%w: name %q", ErrContainerNameConflict, name)
	}
	if err := checkStatus(resp, "container-create"); err != nil {
		return "", err
	}
	var out containerCreateResponse
	if err := decodeJSON(resp, "container-create", &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", &EngineError{Class: ErrClassMalformed, Op: "container-create", Err: errors.New("empty container id in create response")}
	}
	return out.ID, nil
}

// StartContainer calls POST /containers/{id}/start. A 304 ("already
// started") is treated as success: this package's own retry path may call
// Start against a container it itself already started, and that must be
// idempotent rather than an error.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "container-start", http.MethodPost, "/containers/"+url.PathEscape(id)+"/start")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotModified {
		resp.Body.Close()
		return nil
	}
	if err := checkStatus(resp, "container-start"); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ContainerInspect is the subset of GET /containers/{id}/json this package
// relies on: running state (the health gate's "must remain running"
// invariant) and per-network IP addresses (the health gate probes the
// candidate directly on its environment-network IP, since release
// containers publish no ports).
type ContainerInspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		// OOMKilled distinguishes a container the kernel killed under
		// memory pressure from one whose own process exited. The two are
		// indistinguishable by exit code alone, and they classify
		// differently: an OOM kill is a host condition worth retrying,
		// an application that exits on its own is not.
		OOMKilled bool `json:"OOMKilled"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			NetworkID string `json:"NetworkID"`
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// InspectContainer calls GET /containers/{id}/json.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	if err := c.getJSON(ctx, "container-inspect", "/containers/"+url.PathEscape(id)+"/json", &out); err != nil {
		return ContainerInspect{}, err
	}
	if out.ID == "" {
		return ContainerInspect{}, &EngineError{Class: ErrClassMalformed, Op: "container-inspect", Err: errors.New("empty container id in inspect response")}
	}
	return out, nil
}

// ContainerSummary is one entry from GET /containers/json.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// ListContainersByLabels returns every container (running or not) whose
// labels satisfy every key/value pair in labels -- an exact AND match, never
// a partial one. This is the retry-idempotency lookup this package's
// callers run before creating anything: ADR 0005 requires cleanup and
// lookup alike to select by the resource's full immutable identity, never a
// broadened selector, so an empty selector is refused outright rather than
// silently matching everything.
func (c *Client) ListContainersByLabels(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	if len(labels) == 0 {
		return nil, &EngineError{Class: ErrClassConfiguration, Op: "container-list", Err: errors.New("refusing an empty label selector")}
	}
	filterValues := make([]string, 0, len(labels))
	for k, v := range labels {
		filterValues = append(filterValues, k+"="+v)
	}
	filters, err := json.Marshal(map[string][]string{"label": filterValues})
	if err != nil {
		return nil, &EngineError{Class: ErrClassConfiguration, Op: "container-list", Err: err}
	}
	q := url.Values{}
	q.Set("all", "true")
	q.Set("filters", string(filters))
	var out []ContainerSummary
	if err := c.getJSON(ctx, "container-list", "/containers/json?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NetworkInspect is the subset of GET /networks/{id-or-name} this package
// relies on: the network's canonical ID, which becomes
// containerHostConfig.NetworkMode on create so a container is joined by a
// resolved, verified-to-exist ID rather than a name that could collide with
// a Docker-magic value like "host" or "none".
type NetworkInspect struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// InspectNetwork calls GET /networks/{nameOrID}. Environment networks are
// provisioned outside this package -- ADR 0005 is explicit that network
// creation is environment provisioning, not per-deployment work, and
// selectors.go's doc comment already establishes that environment
// infrastructure is never selected by label from this package -- so
// InspectNetwork never creates one. A 404 here means the environment was
// never provisioned (or was provisioned under a different identifier) and
// is exactly checkStatus's existing 4xx-is-configuration classification:
// retrying the deployment unchanged will not fix it.
func (c *Client) InspectNetwork(ctx context.Context, nameOrID string) (NetworkInspect, error) {
	var out NetworkInspect
	if err := c.getJSON(ctx, "network-inspect", "/networks/"+url.PathEscape(nameOrID), &out); err != nil {
		return NetworkInspect{}, err
	}
	if out.ID == "" {
		return NetworkInspect{}, &EngineError{Class: ErrClassMalformed, Op: "network-inspect", Err: errors.New("empty network id in inspect response")}
	}
	return out, nil
}
