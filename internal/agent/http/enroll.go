package agenthttp

import (
	"encoding/hex"
	"net/http"

	"autodeploy/internal/github"
	"autodeploy/internal/server"
)

// enrollRequest is deliberately never sent a raw credential token. The agent
// generates its own credential token locally (crypto/rand, via
// server.NewCredentialToken) and keeps the raw value only in its own 0600
// credential file; it presents this request's CredentialDigest instead --
// the domain-separated digest of that locally-generated token, computed
// with server.CredentialTokenDigest. The control plane therefore never
// observes, transmits, or stores the raw credential; it only ever sees and
// stores its digest, exactly like every credential this repository knows
// about (see internal/store/postgres/server_repository.go). EnrollmentToken
// is the single-use token an administrator printed out of band and gave to
// whoever is provisioning the agent; it authorizes this one enroll call and
// is itself never persisted, only its digest.
type enrollRequest struct {
	ServerID         string `json:"server_id"`
	EnrollmentToken  string `json:"enrollment_token"`
	CredentialDigest string `json:"credential_digest"`
}

func (req enrollRequest) parse() (serverID string, enrollmentDigest, credentialDigest [32]byte, ok bool) {
	if !github.ValidOpaqueID(req.ServerID) {
		return "", enrollmentDigest, credentialDigest, false
	}
	token, err := server.ParseEnrollmentToken(req.EnrollmentToken)
	if err != nil {
		return "", enrollmentDigest, credentialDigest, false
	}
	enrollmentDigest, err = server.EnrollmentTokenDigest(token)
	if err != nil {
		return "", enrollmentDigest, credentialDigest, false
	}
	credentialDigest, ok = decodeDigest(req.CredentialDigest)
	if !ok {
		return "", enrollmentDigest, credentialDigest, false
	}
	return req.ServerID, enrollmentDigest, credentialDigest, true
}

type enrollResponse struct {
	CredentialVersion int `json:"credential_version"`
}

func (h Handler) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if h.Enroller == nil {
		h.writeError(w, http.StatusServiceUnavailable, "service_unavailable")
		return
	}
	var req enrollRequest
	if !h.readJSON(w, r, MaxBodyBytes, &req) {
		return
	}
	serverID, enrollmentDigest, credentialDigest, ok := req.parse()
	if !ok {
		h.writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	version, err := h.Enroller.Enroll(r.Context(), serverID, enrollmentDigest, credentialDigest)
	if err != nil {
		h.mapError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, enrollResponse{CredentialVersion: version})
}

// decodeDigest parses a lowercase-or-uppercase hex-encoded 32-byte digest.
// It is deliberately strict about length: a truncated or padded value is
// rejected rather than silently zero-extended.
func decodeDigest(s string) ([32]byte, bool) {
	var d [32]byte
	if len(s) != hex.EncodedLen(len(d)) {
		return d, false
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(d) {
		return d, false
	}
	copy(d[:], b)
	return d, true
}
