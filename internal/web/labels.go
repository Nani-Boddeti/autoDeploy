package web

// Human labels for the closed enums the store reports. Unknown values are not
// dropped: they render raw (html/template escapes them), so a new class or
// reason added server-side is visible before the UI learns its label.

// failureClassLabels maps the failure_class enum (queue/retry.go).
var failureClassLabels = map[string]string{
	"transient":               "Temporary failure",
	"authorization":           "Authorization failed",
	"invalid_input":           "Invalid input",
	"cancelled":               "Cancelled",
	"corrupt_state":           "Inconsistent state",
	"permanent_configuration": "Configuration problem",
}

// failureReasonLabels maps the allowlisted health-gate reasons (health.go).
var failureReasonLabels = map[string]string{
	"container_unhealthy":   "Image HEALTHCHECK reported unhealthy",
	"deadline_exceeded":     "Health check deadline exceeded",
	"container_not_running": "Container stopped before passing",
	"context_cancelled":     "Health check cancelled",
}

// reasonView is the argument of the "reason" partial: a failure class pill plus
// the reason text. All label fields are empty when there is nothing to show.
type reasonView struct {
	Class, ClassLabel, Reason, ReasonLabel, Probe string
}

func labelOrRaw(labels map[string]string, key string) string {
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}

// newReasonView builds the view for a failed deployment. Callers pass empty
// strings for anything unknown; an all-empty input yields the zero view.
func newReasonView(class, reason, probe string) reasonView {
	return reasonView{
		Class: class, ClassLabel: labelOrRaw(failureClassLabels, class),
		Reason: reason, ReasonLabel: labelOrRaw(failureReasonLabels, reason),
		Probe: probe,
	}
}

// failedReason returns the reason view for a deployment status: failure fields
// are shown only when the deployment itself failed (the store already limits
// them to a failed latest attempt; this is the second gate).
func failedReason(status, class, reason string) reasonView {
	if status != "failed" {
		return reasonView{}
	}
	return newReasonView(class, reason, "")
}
