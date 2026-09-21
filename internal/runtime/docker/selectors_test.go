package docker

import (
	"errors"
	"reflect"
	"testing"
)

func TestReleaseRuntimeContainerSelectorMatchesItsOwnLabels(t *testing.T) {
	id := validReleaseIdentity()
	labels, err := ReleaseRuntimeContainerLabels(id)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	selector, err := ReleaseRuntimeContainerSelector(id)
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if !reflect.DeepEqual(labels, selector) {
		t.Fatalf("selector must exact-match a resource's own creation labels\nlabels:   %v\nselector: %v", labels, selector)
	}
}

func TestReleaseEphemeralVolumeSelectorKindMatchesVolumeNotContainer(t *testing.T) {
	id := validReleaseIdentity()
	selector, err := ReleaseEphemeralVolumeSelector(id)
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if selector[LabelKind] != KindReleaseEphemeralVolume {
		t.Fatalf("kind = %q, want %q (selector must not select the container kind)", selector[LabelKind], KindReleaseEphemeralVolume)
	}
}

func TestBuiltImageSelectorHasNoAttemptOrFence(t *testing.T) {
	selector, err := BuiltImageSelector(validImageIdentity())
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	for _, k := range []string{LabelAttemptID, LabelFenceID} {
		if _, ok := selector[k]; ok {
			t.Errorf("built image selector must not require %s", k)
		}
	}
}

// TestBuiltImageSelectorMatchesLabelsTheBuildActuallyApplies guards against
// the selector silently drifting from what Step 5's build path (build.go's
// BuiltImageLabels(req.Identity) call) actually bakes into a built image.
// If they ever disagreed, BuiltImageSelector would match zero images
// forever -- a cleanup path built on it would silently leak disk while
// reporting success. BuiltImageIdentity deliberately carries no
// ImageDigest/ConfigDigest (see its doc comment), so this also proves the
// selector never demands those fields either.
func TestBuiltImageSelectorMatchesLabelsTheBuildActuallyApplies(t *testing.T) {
	id := validImageIdentity()
	labels, err := BuiltImageLabels(id)
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	selector, err := BuiltImageSelector(id)
	if err != nil {
		t.Fatalf("selector: %v", err)
	}
	if !reflect.DeepEqual(labels, selector) {
		t.Fatalf("selector must exact-match the labels a build actually applies\nlabels:   %v\nselector: %v", labels, selector)
	}
	for _, k := range []string{LabelImageDigest, LabelConfigDigest} {
		if _, ok := selector[k]; ok {
			t.Errorf("built image selector must not require %s: no built image can ever carry its own digest as a label", k)
		}
	}
}

func TestSelectorsPropagateValidationErrors(t *testing.T) {
	id := validReleaseIdentity()
	id.FenceID = ""
	if _, err := ReleaseRuntimeContainerSelector(id); !errors.Is(err, ErrMissingIdentifier) {
		t.Fatalf("got err=%v, want ErrMissingIdentifier", err)
	}
}
