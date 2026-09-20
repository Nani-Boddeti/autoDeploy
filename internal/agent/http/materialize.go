package agenthttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"autodeploy/internal/queue"
	"autodeploy/internal/secret"
)

// materializeRequest names no variable and no version deliberately: the
// frozen set recorded at claim time is the authority on what this attempt
// may decrypt, so an agent cannot widen its own request (ADR 0003 "That the
// deployment requires each requested value" is a server-side check, not a
// client-supplied list).
type materializeRequest struct {
	AttemptID int64 `json:"attempt_id"`
	Fence     int64 `json:"fence"`
}

func (req materializeRequest) valid() bool { return req.AttemptID > 0 && req.Fence > 0 }

type materializedSecretView struct {
	VariableName  string `json:"variable_name"`
	DeliveryScope string `json:"delivery_scope"`
	Value         string `json:"value"`
}

type materializeResponse struct {
	Secrets []materializedSecretView `json:"secrets"`
}

// handleMaterializeSecrets re-authorizes from locked rows, then decrypts only
// the frozen set, only for the owning server, only while the lease is live.
// Authorization and decryption are deliberately separate steps: the
// repository call holds row locks and returns references, and decryption
// happens afterwards so no KEK operation runs inside a database transaction.
//
// The response body carries plaintext. It is therefore never logged, never
// audited, and never cached: ADR 0003 makes this transport the one point
// where a version reference becomes a value, and the generic error shape
// below keeps a failure from revealing which check refused.
func (h Handler) handleMaterializeSecrets(w http.ResponseWriter, r *http.Request) {
	serverID, digest, ok := h.identity(r)
	if !ok {
		h.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if h.MaterializationAuthorizer == nil || h.SecretMaterializerFactory == nil {
		// Fail closed: an agent must never receive an empty secret set that
		// it could mistake for "this deployment has no secrets" when the
		// secret subsystem is simply not configured.
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req materializeRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	if !req.valid() {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	authorization, err := h.MaterializationAuthorizer.AuthorizeMaterialization(r.Context(), serverID, digest, req.AttemptID, req.Fence)
	if err != nil {
		h.mapError(w, err)
		return
	}

	// Build a materializer whose authorizer admits exactly this attempt's
	// frozen set, so the decryption step enforces the same boundary the
	// database just authorized and a bug in this loop cannot widen it.
	keys := make([]secret.AuthorizationKey, 0, len(authorization.Secrets))
	for _, ref := range authorization.Secrets {
		keys = append(keys, secret.AuthorizationKey{
			Scope: secret.Scope{
				OwnerID:       authorization.OwnerID,
				ProjectID:     authorization.ProjectID,
				EnvironmentID: authorization.EnvironmentID,
				SecretID:      ref.SecretID,
				Purpose:       ref.Purpose,
				DeliveryScope: ref.DeliveryScope,
			},
			VersionID: ref.SecretVersionID,
		})
	}
	materializer := h.SecretMaterializerFactory.MaterializerForFrozenSet(keys)
	if materializer == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}

	values := make([]materializedSecretView, 0, len(authorization.Secrets))
	for _, ref := range authorization.Secrets {
		auditID, err := materializationAuditID()
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		plaintext, err := materializer.MaterializeAuthorized(r.Context(), secret.AuthorizationRequest{
			Scope: secret.Scope{
				OwnerID:       authorization.OwnerID,
				ProjectID:     authorization.ProjectID,
				EnvironmentID: authorization.EnvironmentID,
				SecretID:      ref.SecretID,
				Purpose:       ref.Purpose,
				DeliveryScope: ref.DeliveryScope,
			},
			VersionID:    ref.SecretVersionID,
			ServiceID:    serverID,
			DeploymentID: authorization.DeploymentID,
			LeaseID:      leaseIdentifier(authorization),
			// Frozen lets a version superseded by a rotation after the freeze
			// still materialize, which ADR 0003 requires for executing work;
			// a revoked version is refused by the authorization step above.
			Frozen: true,
		}, auditID)
		if err != nil {
			// A refusal here means the value could not be produced at all.
			// Returning a partial set would let the agent build a container
			// with some variables silently missing, so the whole request
			// fails and no plaintext is returned.
			h.zeroValues(values)
			h.writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		values = append(values, materializedSecretView{
			VariableName:  ref.VariableName,
			DeliveryScope: ref.DeliveryScope,
			Value:         string(plaintext),
		})
		clear(plaintext)
	}

	h.writeJSON(w, http.StatusOK, materializeResponse{Secrets: values})
}

// zeroValues overwrites already-decrypted values when a later value in the
// same request fails. Go strings are immutable so this cannot scrub the
// underlying bytes; it drops the references so the set is not serialized,
// and is best effort only, exactly as ADR 0003 describes memory clearing.
func (h Handler) zeroValues(values []materializedSecretView) {
	for i := range values {
		values[i] = materializedSecretView{}
	}
}

// leaseIdentifier names the attempt whose lease authorized this
// materialization, for the audit trail. It is an identifier, not a
// credential.
func leaseIdentifier(a queue.MaterializationAuthorization) string {
	return "attempt-" + itoa(a.AttemptID) + "-fence-" + itoa(a.Fence)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func materializationAuditID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "materialize-" + hex.EncodeToString(raw[:]), nil
}

// MaterializationAuthorizer re-derives an attempt's right to materialize its
// frozen secret set from locked rows. It matches
// (*postgres.AgentLeaseRepository).AuthorizeMaterialization exactly.
type MaterializationAuthorizer interface {
	AuthorizeMaterialization(ctx context.Context, serverID string, presentedDigest [32]byte, attemptID, fence int64) (queue.MaterializationAuthorization, error)
}

// SecretMaterializer decrypts one authorized secret version. It matches
// (*secret.Service).MaterializeAuthorized exactly.
type SecretMaterializer interface {
	MaterializeAuthorized(ctx context.Context, request secret.AuthorizationRequest, auditID string) ([]byte, error)
}

// SecretMaterializerFactory builds a materializer scoped to one request's
// authorized frozen set. Production wiring returns a secret.Service whose
// authorizer is a secret.SetAuthorizer over exactly those keys, so the
// process holds no materializer capable of decrypting anything else.
type SecretMaterializerFactory interface {
	MaterializerForFrozenSet(keys []secret.AuthorizationKey) SecretMaterializer
}
