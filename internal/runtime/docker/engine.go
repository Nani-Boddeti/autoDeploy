package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, &EngineError{Class: ErrClassConfiguration, Op: op, Err: err}
	}
	req.Host = dummyHost
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
