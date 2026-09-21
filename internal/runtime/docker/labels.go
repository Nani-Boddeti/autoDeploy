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

// BuiltImageIdentity identifies a built image: owner, project, a digest
// identifying the build's source content, and the build operation's own
// opaque ID. Images may be shared by multiple runtime instances and are
// deleted only after reference checks, never by a single-owner label, so
// this identity deliberately excludes environment, server, release,
// attempt, and fence: an image does not belong to any one of them.
//
// SourceDigest is required to be a 64-hex digest (requireHexDigest below),
// but this package does not compute it and does not require it to be a
// hash of the actual Dockerfile-plus-context tree. Its only caller today,
// internal/agent/executor (Slice 9b Step 6b), derives it as
// sha256(commitSHA) over the exact commit SHA gitcheckout.Checkout has
// already verified (`git rev-parse HEAD` cross-checked against the
// authorized binding) -- not a hash of the checked-out files themselves.
// That is a deliberate, documented choice (avoids a second, TOCTOU-prone
// filesystem walk purely to produce a label value) but it means an auditor
// cannot reproduce this value by hashing the checkout tree; it is only
// reproducible by hashing the commit SHA that produced it. A future caller
// building the same commit through a different Dockerfile would collide on
// this SourceDigest even though the resulting image differs -- acceptable
// today because this slice performs no build caching/sharing decisions off
// SourceDigest alone (see BuildOperationID below), but worth revisiting if
// a later slice adds real image-sharing/cache lookups keyed on it.
//
// It also deliberately excludes the built image's own ImageDigest/
// ConfigDigest. Those are outputs of the very build these labels are
// attached to via `buildx build --label`, and Docker image labels are
// baked into the image config that the image's own digest is computed
// over -- so a built image's own image/config digest can never be a
// self-referencing baked-in label without an artificial multi-pass
// reproducible-build scheme, which this package does not attempt. Step 5
// applies exactly these fields as build-time labels; the resulting
// ImageDigest/ConfigDigest are read back from the build's own result
// (buildx --metadata-file) and belong to the database, not a label.
// Consequently BuiltImageSelector also matches on exactly these fields
// (plus the kind/schema labels every selector matches): per ADR 0005, a
// shared built image is looked up by owner/project/source/build-operation
// identity together with the database's own image-ID/digest record and
// reference checks, not by a digest-shaped label filter that no image
// could ever satisfy.
type BuiltImageIdentity struct {
	OwnerID      string
	ProjectID    string
	SourceDigest string
	// BuildOperationID is this package's caller's own opaque identifier for
	// one build invocation. internal/agent/executor (Slice 9b Step 6b)
	// currently derives it as the same value as
	// ReleaseResourceIdentity.RuntimeInstanceID -- the attempt's own id --
	// because this slice performs one fresh build per attempt with no
	// cross-attempt build caching/sharing. If a later slice adds that
	// sharing (so one image can legitimately serve more than one
	// RuntimeInstanceID), BuildOperationID must be derived independently
	// from RuntimeInstanceID at that point; tying it to the attempt would
	// otherwise make every attempt look like a distinct build operation
	// even when the image was actually reused.
	BuildOperationID string
}

func (id BuiltImageIdentity) fields() []labelField {
	return []labelField{
		{LabelOwnerID, id.OwnerID, requireOpaqueID},
		{LabelProjectID, id.ProjectID, requireOpaqueID},
		{LabelSourceDigest, id.SourceDigest, requireHexDigest},
		{LabelBuildOperationID, id.BuildOperationID, requireOpaqueID},
	}
}

// BuiltImageLabels builds the labels Step 5 applies to a built image at
// build time via `buildx build --label`. See BuiltImageIdentity for why
// ImageDigest/ConfigDigest are excluded.
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
