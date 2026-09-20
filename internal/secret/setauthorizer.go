package secret

import "context"

// AuthorizationKey identifies exactly one materializable secret version: its
// full scope plus the version id. Two keys are equal only when every field
// matches, so a key can never stand in for a different project,
// environment, purpose, delivery scope, or version.
type AuthorizationKey struct {
	Scope     Scope
	VersionID string
}

// SetAuthorizer admits exactly the keys it was built with and nothing else.
//
// It exists because Service requires an Authorizer and an always-allow
// implementation would let any code path in the process decrypt any secret.
// The agent materialization route has already re-derived, from locked rows,
// the precise frozen set one attempt may read; wrapping that set in an
// authorizer means the decryption step itself enforces the same boundary, so
// a future bug in the calling loop cannot widen what is decrypted beyond
// what the database authorized.
//
// A SetAuthorizer is built per authorized request and is immutable
// afterwards, so it carries no cross-request state and nothing to
// invalidate. It is safe for concurrent use.
type SetAuthorizer struct {
	allowed map[AuthorizationKey]struct{}
}

// NewSetAuthorizer returns an authorizer admitting exactly keys. An empty or
// nil set admits nothing, which is the correct fail-closed default: a
// deployment with no frozen secrets must decrypt nothing rather than
// everything.
func NewSetAuthorizer(keys []AuthorizationKey) *SetAuthorizer {
	allowed := make(map[AuthorizationKey]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	return &SetAuthorizer{allowed: allowed}
}

// AuthorizeSecret admits a request only when its exact scope and version were
// in the authorized set. Frozen is not part of the key: whether a superseded
// version may be read is Service's own rule, and letting a caller flip that
// flag must not change which secret it can reach.
func (a *SetAuthorizer) AuthorizeSecret(_ context.Context, request AuthorizationRequest) error {
	if a == nil || len(a.allowed) == 0 {
		return ErrUnauthorized
	}
	if _, ok := a.allowed[AuthorizationKey{Scope: request.Scope, VersionID: request.VersionID}]; !ok {
		return ErrUnauthorized
	}
	return nil
}
