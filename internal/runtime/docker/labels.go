package docker

// labelSchemaVersion is bumped whenever the meaning or required set of
// labels for any kind changes. Selectors always match it exactly, so a
// schema change cannot accidentally select resources created under a prior
// (differently-scoped) schema.
const labelSchemaVersion = "v1"

// Label keys. All AutoDeploy-managed Docker resources are namespaced under
// io.autodeploy.*; no other key is ever written by this package.
const (
	LabelManaged           = "io.autodeploy.managed"
	LabelSchemaVersion     = "io.autodeploy.label-schema"
	LabelKind              = "io.autodeploy.kind"
	LabelOwnerID           = "io.autodeploy.owner-id"
	LabelProjectID         = "io.autodeploy.project-id"
	LabelEnvironmentID     = "io.autodeploy.environment-id"
	LabelServerID          = "io.autodeploy.server-id"
	LabelDeploymentID      = "io.autodeploy.deployment-id"
	LabelReleaseID         = "io.autodeploy.release-id"
	LabelJobID             = "io.autodeploy.job-id"
	LabelAttemptID         = "io.autodeploy.attempt-id"
	LabelFenceID           = "io.autodeploy.fence-id"
	LabelRuntimeInstanceID = "io.autodeploy.runtime-instance-id"
	LabelCommitSHA         = "io.autodeploy.commit-sha"
	LabelImageDigest       = "io.autodeploy.image-digest"
	LabelConfigDigest      = "io.autodeploy.config-digest"
	LabelSourceDigest      = "io.autodeploy.source-digest"
	LabelBuildOperationID  = "io.autodeploy.build-operation-id"
	LabelRevision          = "io.autodeploy.revision"
)

// Resource kinds. Each is a distinct value of LabelKind; ADR 0005 requires
// cleanup to select resources by exact label match on, among others, this
// key, so kinds are never mixed within one label set.
const (
	KindReleaseRuntimeContainer = "release-runtime-container"
	KindReleaseEphemeralVolume  = "release-ephemeral-volume"
	KindBuiltImage              = "built-image"
	KindEnvironmentNetwork      = "environment-network"
	KindEnvironmentVolume       = "environment-volume"
)

// labelField pairs one label key with the raw value supplied by the caller
// and the validator that must accept it before it can appear in a label
// map. This is the structural choke point: a label builder function only
// ever emits values that pass one of these named validators, so a caller
// can never pass a hostname, secret, or other free-form string through to a
// Docker label, whether accidentally or otherwise.
type labelField struct {
	key      string
	value    string
	validate identifierValidator
}

func buildLabels(kind string, fields []labelField) (map[string]string, error) {
	out := map[string]string{
		LabelManaged:       "true",
		LabelSchemaVersion: labelSchemaVersion,
		LabelKind:          kind,
	}
	for _, f := range fields {
		v, err := f.validate(f.key, f.value)
		if err != nil {
			return nil, err
		}
		out[f.key] = v
	}
	return out, nil
}

// ReleaseResourceIdentity identifies a release-scoped container or
// ephemeral volume. Every field is required: ADR 0005 requires
// release-scoped Docker resources to carry the full deployment/release/job/
// attempt/fence/runtime-instance identity, plus content digests, so cleanup
// can select exactly one resource by exact label match alone. There is no
// field for mutable state (no "active" flag, no timestamp): labels are
// creation facts, and this struct has no place to put anything else.
type ReleaseResourceIdentity struct {
	OwnerID           string
	ProjectID         string
	EnvironmentID     string
	ServerID          string
	DeploymentID      string
	ReleaseID         string
	JobID             string
	AttemptID         string
	FenceID           string
	RuntimeInstanceID string
	CommitSHA         string
	ImageDigest       string
	ConfigDigest      string
}

func (id ReleaseResourceIdentity) fields() []labelField {
	return []labelField{
		{LabelOwnerID, id.OwnerID, requireOpaqueID},
		{LabelProjectID, id.ProjectID, requireOpaqueID},
		{LabelEnvironmentID, id.EnvironmentID, requireOpaqueID},
		{LabelServerID, id.ServerID, requireOpaqueID},
		{LabelDeploymentID, id.DeploymentID, requireOpaqueID},
		{LabelReleaseID, id.ReleaseID, requireOpaqueID},
		{LabelJobID, id.JobID, requireOpaqueID},
		{LabelAttemptID, id.AttemptID, requireOpaqueID},
		{LabelFenceID, id.FenceID, requireOpaqueID},
		{LabelRuntimeInstanceID, id.RuntimeInstanceID, requireOpaqueID},
		{LabelCommitSHA, id.CommitSHA, requireCommitSHA},
		{LabelImageDigest, id.ImageDigest, requireHexDigest},
		{LabelConfigDigest, id.ConfigDigest, requireHexDigest},
	}
}

// ReleaseRuntimeContainerLabels builds the creation labels for a release's
// runtime container.
func ReleaseRuntimeContainerLabels(id ReleaseResourceIdentity) (map[string]string, error) {
	return buildLabels(KindReleaseRuntimeContainer, id.fields())
}

// ReleaseEphemeralVolumeLabels builds the creation labels for a
// release-scoped ephemeral volume.
func ReleaseEphemeralVolumeLabels(id ReleaseResourceIdentity) (map[string]string, error) {
	return buildLabels(KindReleaseEphemeralVolume, id.fields())
}

// BuiltImageIdentity identifies a built image. Images may be shared by
// multiple runtime instances and are deleted only after reference checks,
// never by a single-owner label, so this identity deliberately excludes
// environment, server, release, attempt, and fence: an image does not
// belong to any one of them.
type BuiltImageIdentity struct {
	OwnerID          string
	ProjectID        string
	ImageDigest      string
	ConfigDigest     string
	SourceDigest     string
	BuildOperationID string
}

func (id BuiltImageIdentity) fields() []labelField {
	return []labelField{
		{LabelOwnerID, id.OwnerID, requireOpaqueID},
		{LabelProjectID, id.ProjectID, requireOpaqueID},
		{LabelImageDigest, id.ImageDigest, requireHexDigest},
		{LabelConfigDigest, id.ConfigDigest, requireHexDigest},
		{LabelSourceDigest, id.SourceDigest, requireHexDigest},
		{LabelBuildOperationID, id.BuildOperationID, requireOpaqueID},
	}
}

// BuiltImageLabels builds the creation labels for a built image.
func BuiltImageLabels(id BuiltImageIdentity) (map[string]string, error) {
	return buildLabels(KindBuiltImage, id.fields())
}

// EnvironmentInfrastructureIdentity identifies stable environment
// infrastructure: the environment's network or its persistent volume. It
// carries environment-infrastructure identity and revision only. There is
// no release, job, attempt, or fence field on this struct, which is the
// structural guarantee that environment infrastructure labels can never
// claim to belong to one release, attempt, or fence, and can never be
// mistaken for a release-scoped resource.
type EnvironmentInfrastructureIdentity struct {
	OwnerID       string
	ProjectID     string
	EnvironmentID string
	ServerID      string
	Revision      string
}

func (id EnvironmentInfrastructureIdentity) fields() []labelField {
	return []labelField{
		{LabelOwnerID, id.OwnerID, requireOpaqueID},
		{LabelProjectID, id.ProjectID, requireOpaqueID},
		{LabelEnvironmentID, id.EnvironmentID, requireOpaqueID},
		{LabelServerID, id.ServerID, requireOpaqueID},
		{LabelRevision, id.Revision, requireRevision},
	}
}

// EnvironmentNetworkLabels builds the creation labels for an environment's
// stable bridge network.
func EnvironmentNetworkLabels(id EnvironmentInfrastructureIdentity) (map[string]string, error) {
	return buildLabels(KindEnvironmentNetwork, id.fields())
}

// EnvironmentVolumeLabels builds the creation labels for an environment's
// persistent volume.
func EnvironmentVolumeLabels(id EnvironmentInfrastructureIdentity) (map[string]string, error) {
	return buildLabels(KindEnvironmentVolume, id.fields())
}
