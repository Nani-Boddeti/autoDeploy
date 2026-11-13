package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"autodeploy/internal/auth"
	"autodeploy/internal/store/postgres"
)

// RollbackStore is the narrow store surface behind POST /deployments/rollback
// and the detail page's "Roll back to this release" form (step 20D). Both
// methods are owner-scoped by the repository. *postgres.WebhookJobRepository
// satisfies it. A nil RollbackStore 404s the route and hides the form.
type RollbackStore interface {
	RollbackEligible(ctx context.Context, principal auth.Principal, targetReleaseID string) (bool, error)
	EnqueueRollback(ctx context.Context, principal auth.Principal, targetReleaseID, nonce string) (postgres.RollbackResult, error)
}

// rollbackConfirmValue is the only accepted value of the confirmation checkbox.
const rollbackConfirmValue = "rollback"

// offerRollback shows the Roll back form only for a retained release the store
// currently reports eligible. The check is advisory (the store re-evaluates
// under locks on submit). It returns false after writing an error response.
func (h *Handler) offerRollback(w http.ResponseWriter, r *http.Request, p auth.Principal, d postgres.DeploymentDetail, view *deploymentDetailView) bool {
	if h.config.Rollback == nil || d.Release == nil || d.Release.State != "retained" {
		return true
	}
	eligible, err := h.config.Rollback.RollbackEligible(r.Context(), p, d.Release.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rollback eligibility failed: %v\n", err)
		plain(w, http.StatusInternalServerError, "the page could not be loaded; retry\n")
		return false
	}
	if !eligible {
		return true
	}
	nonce, nonceErr := newManualNonce()
	csrf, csrfErr := auth.NewSessionCSRF(h.config.CSRFKeys, p.SessionID, h.config.PublicOrigin)
	if nonceErr != nil || csrfErr != nil {
		plain(w, http.StatusInternalServerError, "the page could not be loaded; retry\n")
		return false
	}
	view.CanRollback, view.RollbackCSRF, view.RollbackNonce, view.RollbackReleaseID = true, csrf, nonce, d.Release.ID
	return true
}

// deploymentRollback enqueues a rollback to a retained release. The release id
// is the only client-chosen value; everything else is derived server-side from
// the owner-scoped rows. Failures are fixed generic texts.
func (h *Handler) deploymentRollback(w http.ResponseWriter, r *http.Request) {
	if h.provenance(r) != nil {
		badRequest(w)
		return
	}
	v, err := parseURLForm(r)
	if err != nil || len(v) != 4 || len(v["csrf"]) != 1 || len(v["release_id"]) != 1 || len(v["request_nonce"]) != 1 || len(v["confirm"]) != 1 {
		badRequest(w)
		return
	}
	_, p, ok := h.session(r.Context(), r)
	if !ok || !auth.ValidateSessionCSRF(h.config.CSRFKeys, v.Get("csrf"), p.SessionID, h.config.PublicOrigin) {
		redirect(w, "/login")
		return
	}
	releaseID := v.Get("release_id")
	if v.Get("confirm") != rollbackConfirmValue || !validOperationsID(releaseID) || v.Get("request_nonce") == "" {
		badRequest(w)
		return
	}
	result, err := h.config.Rollback.EnqueueRollback(r.Context(), p, releaseID, v.Get("request_nonce"))
	if err != nil {
		rollbackFailure(w, err)
		return
	}
	redirect(w, detailHref(string(result.DeploymentID)))
}

func rollbackFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrRollbackInvalid):
		badRequest(w)
	case errors.Is(err, postgres.ErrRollbackUnauthorized):
		plain(w, http.StatusForbidden, "this session is not authorized to roll back\n")
	case errors.Is(err, postgres.ErrRollbackNotFound):
		plain(w, http.StatusNotFound, "that release was not found\n")
	case errors.Is(err, postgres.ErrRollbackInFlight):
		plain(w, http.StatusConflict, "a deployment is in progress in this environment; wait for it to finish\n")
	case errors.Is(err, postgres.ErrRollbackSecretRevoked):
		plain(w, http.StatusConflict, "this release uses a secret version that was revoked; redeploy instead\n")
	case errors.Is(err, postgres.ErrRollbackNotEligible):
		plain(w, http.StatusConflict, "this release can no longer be rolled back to; reload the page\n")
	case errors.Is(err, postgres.ErrRollbackConflict):
		plain(w, http.StatusConflict, "this form was already submitted with different values; reload the page\n")
	default:
		fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
		plain(w, http.StatusInternalServerError, "the rollback could not be queued; retry\n")
	}
}
