package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

// Removal exists for retention cleanup only (step 20E, owner decision B-D6).
// Both calls are deliberately NON-FORCE and take no force parameter: a running
// container or an image still used by any container is refused by the Engine
// (409) and surfaces as a typed error, never overridden. Anonymous volumes are
// not removed (v=false); the callers verify exact AutoDeploy labels first.

// ErrContainerRunning is returned (wrapped) when the Engine refuses to remove a
// container because it is running (409).
var ErrContainerRunning = errors.New("docker engine: container is running or otherwise cannot be removed")

// ErrImageNotFound is returned (wrapped) when the Engine answers 404 for an image.
var ErrImageNotFound = errors.New("docker engine: image not found")

// ErrImageInUse is returned (wrapped) when the Engine refuses to remove an image
// because a container (running or stopped) still uses it, or it has dependent
// child images (409). The image is kept.
var ErrImageInUse = errors.New("docker engine: image is in use")

var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// RemoveContainer calls DELETE /containers/{id}?force=false&v=false&link=false.
// id must be a 12 or 64 hex container ID or an AutoDeploy container name. 204
// is success; 404 is ErrContainerNotFound; 409 is ErrContainerRunning.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	const op = "container-remove"
	if err := requireContainerTarget(id); err != nil {
		return &EngineError{Class: ErrClassConfiguration, Op: op, Err: err}
	}
	resp, err := c.do(ctx, op, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=false&v=false&link=false")
	if err != nil {
		return err
	}
	return c.removeResult(resp, op, ErrContainerNotFound, ErrContainerRunning)
}

// RemoveImage calls DELETE /images/{id}?force=false&noprune=false. ref must be
// exactly "sha256:<64 lowercase hex>" (an image ID, never a tag or name). 200 is
// success; 404 is ErrImageNotFound; 409 is ErrImageInUse.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	const op = "image-remove"
	if !imageIDPattern.MatchString(ref) {
		return &EngineError{Class: ErrClassConfiguration, Op: op, Err: fmt.Errorf("%w: image id", ErrInvalidIdentifier)}
	}
	resp, err := c.do(ctx, op, http.MethodDelete, "/images/"+url.PathEscape(ref)+"?force=false&noprune=false")
	if err != nil {
		return err
	}
	return c.removeResult(resp, op, ErrImageNotFound, ErrImageInUse)
}

func (c *Client) removeResult(resp *http.Response, op string, errNotFound, errConflict error) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		resp.Body.Close()
		return nil
	case http.StatusNotFound:
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("%w: %s", errNotFound, op)
	case http.StatusConflict:
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return fmt.Errorf("%w: %s", errConflict, op)
	}
	if err := checkStatus(resp, op); err != nil {
		var ee *EngineError
		if errors.As(err, &ee) {
			ee.Err = errors.New(sanitizeStopMessage(ee.Err.Error()))
		}
		return err
	}
	resp.Body.Close()
	return &EngineError{Class: ErrClassMalformed, Op: op, Status: resp.StatusCode, Err: errors.New("unexpected success status")}
}
