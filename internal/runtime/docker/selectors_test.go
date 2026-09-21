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

func TestSelectorsPropagateValidationErrors(t *testing.T) {
	id := validReleaseIdentity()
	id.FenceID = ""
	if _, err := ReleaseRuntimeContainerSelector(id); !errors.Is(err, ErrMissingIdentifier) {
		t.Fatalf("got err=%v, want ErrMissingIdentifier", err)
	}
}
