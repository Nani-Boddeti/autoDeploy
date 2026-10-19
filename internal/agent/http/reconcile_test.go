package agenthttp

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"autodeploy/internal/server"
	"autodeploy/internal/store/postgres"
)

// reconcileBody builds a raw reconcile body so a test can omit or malform
// the outcome key, which marshaling reconcileRequest could not express.
func reconcileBody(outcomeField string) string {
	if outcomeField != "" {
		outcomeField += ","
	}
	return fmt.Sprintf(`{%s"environment_id":"env_1","attempt_id":5,"fence":3,"outcome_digest":%q}`, outcomeField, hexDigest(digestFixture(4)))
}

func TestReconcileOutcomeIsRequiredAndAllowListed(t *testing.T) {
	tok, _ := server.NewCredentialToken(rand.Reader)
	post := func(h Handler, body string) (int, string) {
		w := do(h, newRequestTLS(newRequest(t, pathReconcile, body, bearerPrefix+string(tok), "server_1")))
		var got errorBody
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got.Error
	}

	for name, field := range map[string]string{
		"missing key":   "",
		"empty string":  `"outcome":""`,
		"unknown value": `"outcome":"cleared"`,
		"wrong case":    `"outcome":"Activated"`,
		"padded":        `"outcome":"activated "`,
		"null":          `"outcome":null`,
		"number":        `"outcome":5`,
		// encoding/json keeps the last duplicate, so the invalid last value
		// is what the allow-list sees.
		"duplicate key, invalid last": `"outcome":"activated","outcome":"cleared"`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			fr := &fakeReconciler{}
			code, msg := post(Handler{Reconciler: fr}, reconcileBody(field))
			if code != http.StatusBadRequest || msg != "invalid_request" {
				t.Fatalf("status=%d code=%q, want 400 invalid_request", code, msg)
			}
			if fr.calls != 0 {
				t.Fatalf("repository called %d times for an invalid outcome", fr.calls)
			}
		})
	}

	for _, outcome := range []string{postgres.ActivationOutcomeActivated, postgres.ActivationOutcomeNotActivated, postgres.ActivationOutcomeNoActivation} {
		t.Run("forwards "+outcome, func(t *testing.T) {
			fr := &fakeReconciler{}
			code, _ := post(Handler{Reconciler: fr}, reconcileBody(`"outcome":"`+outcome+`"`))
			if code != http.StatusOK || fr.calls != 1 {
				t.Fatalf("status=%d calls=%d, want 200 and one call", code, fr.calls)
			}
			if fr.gotOutcome != outcome || fr.gotEnv != "env_1" || fr.gotAttempt != 5 || fr.gotFence != 3 || fr.gotOutDigst != digestFixture(4) {
				t.Fatalf("repository saw %+v, want the request unchanged", fr)
			}
		})
	}

	t.Run("differently cased key", func(t *testing.T) {
		// encoding/json matches keys case-insensitively, so "Outcome" binds
		// to the same field as "outcome". That is safe: the value still goes
		// through the same allow-list, so no unvalidated outcome can reach
		// the repository by spelling the key differently.
		fr := &fakeReconciler{}
		code, _ := post(Handler{Reconciler: fr}, reconcileBody(`"Outcome":"activated"`))
		if code != http.StatusOK || fr.calls != 1 || fr.gotOutcome != "activated" {
			t.Fatalf("status=%d calls=%d outcome=%q", code, fr.calls, fr.gotOutcome)
		}
		fr = &fakeReconciler{}
		if code, _ := post(Handler{Reconciler: fr}, reconcileBody(`"Outcome":"cleared"`)); code != http.StatusBadRequest || fr.calls != 0 {
			t.Fatalf("invalid value under the cased key: status=%d calls=%d", code, fr.calls)
		}
	})

	t.Run("unknown extra key still rejected", func(t *testing.T) {
		fr := &fakeReconciler{}
		code, msg := post(Handler{Reconciler: fr}, reconcileBody(`"outcome":"activated","extra":1`))
		if code != http.StatusBadRequest || msg != "invalid_request" || fr.calls != 0 {
			t.Fatalf("status=%d code=%q calls=%d, want 400 invalid_request and no call", code, msg, fr.calls)
		}
	})
}
