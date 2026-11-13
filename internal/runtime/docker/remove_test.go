package docker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const removeImageRef = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRemoveContainerIsNonForceAndMapsStatuses(t *testing.T) {
	for status, want := range map[int]error{http.StatusNotFound: ErrContainerNotFound, http.StatusConflict: ErrContainerRunning, http.StatusNoContent: nil} {
		var method, path, query string
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
			w.WriteHeader(status)
		}))
		err := c.RemoveContainer(context.Background(), stopTestID)
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Fatalf("status %d: err = %v, want %v", status, err, want)
		}
		if method != http.MethodDelete || path != "/v1.44/containers/"+stopTestID || query != "force=false&v=false&link=false" {
			t.Fatalf("status %d: %s %s ?%s", status, method, path, query)
		}
	}
}

func TestRemoveContainerRefusesBadTargetsWithoutCalling(t *testing.T) {
	called := false
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	for _, id := range []string{"", "../x", "abc?force=true", "name with space", "ABCDEF123456"} {
		if err := c.RemoveContainer(context.Background(), id); err == nil {
			t.Fatalf("%q accepted", id)
		}
	}
	if called {
		t.Fatal("engine called for an invalid target")
	}
}

func TestRemoveImageIsNonForceAndMapsStatuses(t *testing.T) {
	for status, want := range map[int]error{http.StatusNotFound: ErrImageNotFound, http.StatusConflict: ErrImageInUse, http.StatusOK: nil} {
		var method, path, query string
		c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path, query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
			w.WriteHeader(status)
			if status == http.StatusOK {
				w.Write([]byte(`[{"Deleted":"x"}]`))
			}
		}))
		err := c.RemoveImage(context.Background(), removeImageRef)
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Fatalf("status %d: err = %v, want %v", status, err, want)
		}
		if method != http.MethodDelete || !strings.HasPrefix(path, "/v1.44/images/sha256") || query != "force=false&noprune=false" {
			t.Fatalf("status %d: %s %s ?%s", status, method, path, query)
		}
	}
}

func TestRemoveImageRefusesTagsAndNames(t *testing.T) {
	c := stopClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("engine called") }))
	for _, ref := range []string{"", "latest", "nginx:latest", "sha256:abc", "sha256:" + strings.Repeat("A", 64), removeImageRef + "?force=true"} {
		if err := c.RemoveImage(context.Background(), ref); err == nil {
			t.Fatalf("%q accepted", ref)
		}
	}
}
