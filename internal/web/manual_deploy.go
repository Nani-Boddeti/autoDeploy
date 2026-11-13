package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"

	"autodeploy/internal/auth"
	"autodeploy/internal/githubapp"
	"autodeploy/internal/store/postgres"
)

// ManualDeployStore is the narrow store surface behind POST
// /deployments/manual (step 19d). Both methods are owner-scoped by the
// repository. *postgres.WebhookJobRepository satisfies it. A nil
// ManualDeployStore (or a nil CommitResolver) disables the route (404) and
// hides the Deploy form: they are only configured when the GitHub App
// subsystem is enabled.
type ManualDeployStore interface {
	ManualDeployTarget(ctx context.Context, principal auth.Principal, projectID string) (installationID, repositoryID int64, fullRef string, err error)
	EnqueueManualDeploy(ctx context.Context, principal auth.Principal, in postgres.ManualDeployInput) (postgres.ManualDeployResult, error)
}

const (
	manualModeHead = "head"
	manualModeSHA  = "sha"
	// manualNonceBytes of crypto/rand entropy, base64url without padding (32
	// chars, inside the store's 16..128 [A-Za-z0-9_-] nonce rule).
	manualNonceBytes = 24
)

func (h *Handler) manualDeployEnabled() bool {
	return h.config.ManualDeploy != nil && h.config.CommitResolver != nil
}

// newManualNonce returns a fresh per-render request nonce. It uses the process
// CSPRNG directly: the nonce is an idempotency key, never a secret, but two
// renders must never share one.
func newManualNonce() (string, error) {
	b := make([]byte, manualNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validLowerHexSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// deploymentManual enqueues a deployment of the bound branch head or of a
// specific commit reachable from the bound branch. The repository and ref come
// from the owner-scoped binding (ManualDeployTarget), never from the form; the
// form carries only the project, the idempotency nonce and the owner's choice.
// Failures are fixed generic texts: no GitHub body, token or internal error is
// ever echoed.
func (h *Handler) deploymentManual(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 5 || len(v["csrf"]) != 1 || len(v["project_id"]) != 1 || len(v["request_nonce"]) != 1 || len(v["mode"]) != 1 || len(v["commit_sha"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	projectID, mode, sha := v.Get("project_id"), v.Get("mode"), v.Get("commit_sha")
	if !validOperationsID(projectID) || v.Get("request_nonce") == "" {
		badRequest(w)
		return
	}
	switch {
	case mode == manualModeHead && sha == "":
	case mode == manualModeSHA && validLowerHexSHA(sha):
	default:
		badRequest(w)
		return
	}
	installationID, repositoryID, fullRef, err := h.config.ManualDeploy.ManualDeployTarget(r.Context(), p, projectID)
	if err != nil {
		manualDeployFailure(w, err)
		return
	}
	if mode == manualModeHead {
		sha, err = h.config.CommitResolver.ResolveBranchHead(r.Context(), installationID, repositoryID, fullRef)
	} else {
		err = h.config.CommitResolver.VerifyCommitOnRef(r.Context(), installationID, repositoryID, fullRef, sha)
	}
	if err != nil {
		manualDeployFailure(w, err)
		return
	}
	result, err := h.config.ManualDeploy.EnqueueManualDeploy(r.Context(), p, postgres.ManualDeployInput{ProjectID: projectID, CommitSHA: sha, RequestNonce: v.Get("request_nonce"), ExpectedInstallationID: installationID, ExpectedRepositoryID: repositoryID, ExpectedRef: fullRef})
	if err != nil {
		manualDeployFailure(w, err)
		return
	}
	redirect(w, detailHref(string(result.DeploymentID)))
}

func manualDeployFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrManualDeployInvalid), errors.Is(err, githubapp.ErrInvalidRef), errors.Is(err, githubapp.ErrInvalidCommitSHA):
		badRequest(w)
	case errors.Is(err, postgres.ErrManualDeployUnauthorized), errors.Is(err, githubapp.ErrRepositoryUnauthorized):
		plain(w, http.StatusForbidden, "this repository is not currently authorized for this project; reconcile the installation\n")
	case errors.Is(err, githubapp.ErrRefNotFound):
		plain(w, http.StatusUnprocessableEntity, "the bound branch was not found\n")
	case errors.Is(err, githubapp.ErrCommitNotFound):
		plain(w, http.StatusUnprocessableEntity, "that commit was not found in the repository\n")
	case errors.Is(err, githubapp.ErrCommitNotOnRef):
		plain(w, http.StatusUnprocessableEntity, "that commit is not on the bound branch\n")
	case errors.Is(err, postgres.ErrManualDeployInFlight):
		plain(w, http.StatusConflict, "this commit is already deploying in this environment\n")
	case errors.Is(err, postgres.ErrManualDeployConflict):
		plain(w, http.StatusConflict, "this form was already submitted with different values; reload the page\n")
	case errors.Is(err, githubapp.ErrGitHubUnavailable):
		plain(w, http.StatusServiceUnavailable, "GitHub is unavailable; try again\n")
	default:
		plain(w, http.StatusInternalServerError, "the deployment could not be queued; retry\n")
	}
}
