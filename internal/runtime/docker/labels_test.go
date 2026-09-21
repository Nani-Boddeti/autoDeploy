package docker

import (
	"errors"
	"testing"
)

const (
	validID     = "01hqxk3z9m0000000000000000"
	validDigest = "sha256:" + "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4"
	validSHA    = "1234567890abcdef1234567890abcdef12345678"
	validRev    = "42"
)

func validReleaseIdentity() ReleaseResourceIdentity {
	return ReleaseResourceIdentity{
		OwnerID:           validID,
		ProjectID:         validID,
		EnvironmentID:     validID,
		ServerID:          validID,
		DeploymentID:      validID,
		ReleaseID:         validID,
		JobID:             validID,
		AttemptID:         validID,
		FenceID:           validID,
		RuntimeInstanceID: validID,
		CommitSHA:         validSHA,
		ImageDigest:       validDigest,
		ConfigDigest:      validDigest,
	}
}

func validImageIdentity() BuiltImageIdentity {
	return BuiltImageIdentity{
		OwnerID:          validID,
		ProjectID:        validID,
		SourceDigest:     validDigest,
		BuildOperationID: validID,
	}
}

func validEnvIdentity() EnvironmentInfrastructureIdentity {
	return EnvironmentInfrastructureIdentity{
		OwnerID:       validID,
		ProjectID:     validID,
		EnvironmentID: validID,
		ServerID:      validID,
		Revision:      validRev,
	}
}

func TestReleaseRuntimeContainerLabelsPositive(t *testing.T) {
	labels, err := ReleaseRuntimeContainerLabels(validReleaseIdentity())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		LabelManaged:           "true",
		LabelSchemaVersion:     labelSchemaVersion,
		LabelKind:              KindReleaseRuntimeContainer,
		LabelOwnerID:           validID,
		LabelProjectID:         validID,
		LabelEnvironmentID:     validID,
		LabelServerID:          validID,
		LabelDeploymentID:      validID,
		LabelReleaseID:         validID,
		LabelJobID:             validID,
		LabelAttemptID:         validID,
		LabelFenceID:           validID,
		LabelRuntimeInstanceID: validID,
		LabelCommitSHA:         validSHA,
		LabelImageDigest:       validDigest,
		LabelConfigDigest:      validDigest,
	}
	if len(labels) != len(want) {
		t.Fatalf("got %d labels, want %d: %v", len(labels), len(want), labels)
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, labels[k], v)
		}
	}
}

func TestReleaseEphemeralVolumeLabelsKindDiffers(t *testing.T) {
	container, err := ReleaseRuntimeContainerLabels(validReleaseIdentity())
	if err != nil {
		t.Fatalf("container: %v", err)
	}
	volume, err := ReleaseEphemeralVolumeLabels(validReleaseIdentity())
	if err != nil {
		t.Fatalf("volume: %v", err)
	}
	if container[LabelKind] == volume[LabelKind] {
		t.Fatalf("container and volume must not share a kind label: %q", container[LabelKind])
	}
	if volume[LabelKind] != KindReleaseEphemeralVolume {
		t.Fatalf("volume kind = %q, want %q", volume[LabelKind], KindReleaseEphemeralVolume)
	}
}

func TestReleaseResourceLabelsMissingRequiredField(t *testing.T) {
	base := validReleaseIdentity()
	cases := []struct {
		name   string
		mutate func(*ReleaseResourceIdentity)
	}{
		{"OwnerID", func(id *ReleaseResourceIdentity) { id.OwnerID = "" }},
		{"ProjectID", func(id *ReleaseResourceIdentity) { id.ProjectID = "" }},
		{"EnvironmentID", func(id *ReleaseResourceIdentity) { id.EnvironmentID = "" }},
		{"ServerID", func(id *ReleaseResourceIdentity) { id.ServerID = "" }},
		{"DeploymentID", func(id *ReleaseResourceIdentity) { id.DeploymentID = "" }},
		{"ReleaseID", func(id *ReleaseResourceIdentity) { id.ReleaseID = "" }},
		{"JobID", func(id *ReleaseResourceIdentity) { id.JobID = "" }},
		{"AttemptID", func(id *ReleaseResourceIdentity) { id.AttemptID = "" }},
		{"FenceID", func(id *ReleaseResourceIdentity) { id.FenceID = "" }},
		{"RuntimeInstanceID", func(id *ReleaseResourceIdentity) { id.RuntimeInstanceID = "" }},
		{"CommitSHA", func(id *ReleaseResourceIdentity) { id.CommitSHA = "" }},
		{"ImageDigest", func(id *ReleaseResourceIdentity) { id.ImageDigest = "" }},
		{"ConfigDigest", func(id *ReleaseResourceIdentity) { id.ConfigDigest = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := base
			tc.mutate(&id)
			if _, err := ReleaseRuntimeContainerLabels(id); !errors.Is(err, ErrMissingIdentifier) {
				t.Fatalf("field %s: got err=%v, want ErrMissingIdentifier", tc.name, err)
			}
		})
	}
}

func TestReleaseResourceLabelsInvalidValues(t *testing.T) {
	base := validReleaseIdentity()
	cases := []struct {
		name   string
		mutate func(*ReleaseResourceIdentity)
	}{
		{"OwnerID has dot (hostname-shaped)", func(id *ReleaseResourceIdentity) { id.OwnerID = "evil.example.com" }},
		{"ImageDigest wrong length", func(id *ReleaseResourceIdentity) { id.ImageDigest = "sha256:abcd" }},
		{"ImageDigest uppercase hex", func(id *ReleaseResourceIdentity) { id.ImageDigest = validDigest[:len(validDigest)-2] + "FF" }},
		{"CommitSHA abbreviated", func(id *ReleaseResourceIdentity) { id.CommitSHA = validSHA[:7] }},
		{"CommitSHA non-hex", func(id *ReleaseResourceIdentity) { id.CommitSHA = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz" }},
		{"AttemptID contains space", func(id *ReleaseResourceIdentity) { id.AttemptID = "att empt" }},
		{"FenceID contains slash (path-shaped)", func(id *ReleaseResourceIdentity) { id.FenceID = "a/b" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := base
			tc.mutate(&id)
			if _, err := ReleaseRuntimeContainerLabels(id); !errors.Is(err, ErrInvalidIdentifier) {
				t.Fatalf("got err=%v, want ErrInvalidIdentifier", err)
			}
		})
	}
}

func TestBuiltImageLabelsPositive(t *testing.T) {
	labels, err := BuiltImageLabels(validImageIdentity())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if labels[LabelKind] != KindBuiltImage {
		t.Fatalf("kind = %q, want %q", labels[LabelKind], KindBuiltImage)
	}
	// Built images are shared across runtimes/attempts: they must never
	// carry release/attempt/fence/environment/server scoping labels.
	forbidden := []string{LabelReleaseID, LabelAttemptID, LabelFenceID, LabelJobID, LabelDeploymentID, LabelEnvironmentID, LabelServerID, LabelRuntimeInstanceID}
	for _, k := range forbidden {
		if _, ok := labels[k]; ok {
			t.Errorf("built image labels must not carry %s", k)
		}
	}
}

func TestBuiltImageLabelsMissingRequiredField(t *testing.T) {
	base := validImageIdentity()
	cases := []struct {
		name   string
		mutate func(*BuiltImageIdentity)
	}{
		{"OwnerID", func(id *BuiltImageIdentity) { id.OwnerID = "" }},
		{"ProjectID", func(id *BuiltImageIdentity) { id.ProjectID = "" }},
		{"SourceDigest", func(id *BuiltImageIdentity) { id.SourceDigest = "" }},
		{"BuildOperationID", func(id *BuiltImageIdentity) { id.BuildOperationID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := base
			tc.mutate(&id)
			if _, err := BuiltImageLabels(id); !errors.Is(err, ErrMissingIdentifier) {
				t.Fatalf("field %s: got err=%v, want ErrMissingIdentifier", tc.name, err)
			}
		})
	}
}

func TestEnvironmentInfrastructureLabelsPositive(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		fn   func(EnvironmentInfrastructureIdentity) (map[string]string, error)
	}{
		{"network", KindEnvironmentNetwork, EnvironmentNetworkLabels},
		{"volume", KindEnvironmentVolume, EnvironmentVolumeLabels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels, err := tc.fn(validEnvIdentity())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if labels[LabelKind] != tc.kind {
				t.Fatalf("kind = %q, want %q", labels[LabelKind], tc.kind)
			}
			// Environment infrastructure must never carry release-scoped
			// attempt/fence/release identity: it is not owned by any one
			// release, attempt, or fence.
			forbidden := []string{LabelReleaseID, LabelAttemptID, LabelFenceID, LabelJobID, LabelDeploymentID, LabelRuntimeInstanceID, LabelCommitSHA, LabelImageDigest, LabelConfigDigest}
			for _, k := range forbidden {
				if _, ok := labels[k]; ok {
					t.Errorf("environment infrastructure labels must not carry %s", k)
				}
			}
		})
	}
}

func TestEnvironmentInfrastructureLabelsMissingRequiredField(t *testing.T) {
	base := validEnvIdentity()
	cases := []struct {
		name   string
		mutate func(*EnvironmentInfrastructureIdentity)
	}{
		{"OwnerID", func(id *EnvironmentInfrastructureIdentity) { id.OwnerID = "" }},
		{"ProjectID", func(id *EnvironmentInfrastructureIdentity) { id.ProjectID = "" }},
		{"EnvironmentID", func(id *EnvironmentInfrastructureIdentity) { id.EnvironmentID = "" }},
		{"ServerID", func(id *EnvironmentInfrastructureIdentity) { id.ServerID = "" }},
		{"Revision", func(id *EnvironmentInfrastructureIdentity) { id.Revision = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := base
			tc.mutate(&id)
			if _, err := EnvironmentNetworkLabels(id); !errors.Is(err, ErrMissingIdentifier) {
				t.Fatalf("field %s: got err=%v, want ErrMissingIdentifier", tc.name, err)
			}
		})
	}
}

func TestEnvironmentInfrastructureLabelsInvalidRevision(t *testing.T) {
	id := validEnvIdentity()
	id.Revision = "not-a-number"
	if _, err := EnvironmentNetworkLabels(id); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("got err=%v, want ErrInvalidIdentifier", err)
	}
}
