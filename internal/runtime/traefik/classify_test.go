package traefik

import (
	"errors"
	"fmt"
	"testing"

	"autodeploy/internal/queue"
)

func TestClassifyWrite(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want queue.FailureClass
	}{
		{"symlink", ErrSymlinkRefused, queue.FailureClassPermanentConfiguration},
		{"ownership", ErrDirectoryOwnership, queue.FailureClassPermanentConfiguration},
		{"not-directory", ErrNotDirectory, queue.FailureClassPermanentConfiguration},
		{"not-regular-file", ErrNotRegularFile, queue.FailureClassCorruptState},
		{"directory-permissions", ErrDirectoryPermissions, queue.FailureClassPermanentConfiguration},
		{"route-file-too-large", ErrRouteFileTooLarge, queue.FailureClassCorruptState},
		{"invalid-route", ErrInvalidRoute, queue.FailureClassPermanentConfiguration},
		{"generic-io", fmt.Errorf("disk full"), queue.FailureClassTransient},
		{"wrapped-symlink", fmt.Errorf("wrap: %w", ErrSymlinkRefused), queue.FailureClassPermanentConfiguration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyWrite(tc.err); got != tc.want {
				t.Fatalf("ClassifyWrite(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyObservation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want queue.FailureClass
	}{
		{"non-loopback", ErrNonLoopbackBaseURL, queue.FailureClassPermanentConfiguration},
		{"router-not-loaded", ErrRouterNotLoaded, queue.FailureClassTransient},
		{"service-not-loaded", ErrServiceNotLoaded, queue.FailureClassTransient},
		{"config-rejected", ErrConfigRejected, queue.FailureClassPermanentConfiguration},
		{"rule-mismatch", ErrRuleMismatch, queue.FailureClassCorruptState},
		{"tls-mismatch", ErrTLSMismatch, queue.FailureClassCorruptState},
		{"wrong-backend", ErrWrongBackend, queue.FailureClassCorruptState},
		{"transient-transport", &ObservationError{Class: ErrClassTransient, Op: "x", Err: errors.New("dial")}, queue.FailureClassTransient},
		{"malformed-transport", &ObservationError{Class: ErrClassMalformed, Op: "x", Err: errors.New("oversized")}, queue.FailureClassCorruptState},
		{"configuration-transport", &ObservationError{Class: ErrClassConfiguration, Op: "x", Err: errors.New("bad request")}, queue.FailureClassPermanentConfiguration},
		{"unrecognized", errors.New("something else"), queue.FailureClassCorruptState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyObservation(tc.err); got != tc.want {
				t.Fatalf("ClassifyObservation(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}
