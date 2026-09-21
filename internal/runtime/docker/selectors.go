package docker

// This file builds the exact-match label selectors used to look up or
// clean up Docker resources. ADR 0005 requires cleanup to select resources
// by exact label match on their full immutable identity, never by a partial
// or name-only match, so each selector here reuses the same validated
// identity struct and the same required-field set as the matching label
// builder in labels.go: a resource's own creation labels are, by
// construction, an exact-match selector for that one resource.
//
// Environment infrastructure (network, persistent volume) deliberately has
// no selector function in this package. ADR 0005 states environment
// infrastructure is never selected by release cleanup; any lookup of it
// must go through its own stored identifier from the database, not a label
// filter. Not exposing a selector here makes that structurally true rather
// than a convention callers could violate.

// ReleaseRuntimeContainerSelector builds the exact-match label filter for
// one release's runtime container.
func ReleaseRuntimeContainerSelector(id ReleaseResourceIdentity) (map[string]string, error) {
	return buildLabels(KindReleaseRuntimeContainer, id.fields())
}

// ReleaseEphemeralVolumeSelector builds the exact-match label filter for
// one release-scoped ephemeral volume.
func ReleaseEphemeralVolumeSelector(id ReleaseResourceIdentity) (map[string]string, error) {
	return buildLabels(KindReleaseEphemeralVolume, id.fields())
}

// BuiltImageSelector builds the exact-match label filter for one built
// image, on owner/project/source-digest/build-operation identity -- the
// fields BuiltImageIdentity actually carries. It never includes attempt,
// fence, or the image's own image/config digest: those are runtime/
// output-only facts a label on the image itself cannot carry (see
// BuiltImageIdentity), so per ADR 0005 a specific built image is resolved
// by this selector together with the database's own image-ID/digest record
// and reference checks, not by a digest-shaped label a real image could
// never match.
func BuiltImageSelector(id BuiltImageIdentity) (map[string]string, error) {
	return buildLabels(KindBuiltImage, id.fields())
}
