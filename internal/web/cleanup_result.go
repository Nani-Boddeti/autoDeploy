package web

import (
	"strconv"

	"autodeploy/internal/store/postgres"
)

// cleanupView is the "Retention cleanup" row of the release block on the
// deployment detail page (step 20E): the latest cleanup of the release.
type cleanupView struct {
	Label  string
	Tone   string
	Detail string
}

func newCleanupView(c *postgres.ReleaseCleanup) *cleanupView {
	if c == nil {
		return nil
	}
	v := &cleanupView{}
	switch c.Result {
	case "":
		v.Label, v.Tone, v.Detail = "in progress", "busy", "waiting for the agent to remove the release"
	case postgres.CleanupResultRemoved:
		v.Label, v.Tone, v.Detail = "removed", "ok", "container removed"
		if c.ImageKept {
			v.Detail = "container removed; image kept (still in use)"
		}
	default:
		v.Label, v.Tone = c.Result, "bad"
		v.Detail = c.ErrorClass
		if c.Summary != "" {
			v.Detail += ": " + c.Summary
		}
		v.Detail += " (attempt " + strconv.Itoa(c.Attempts) + ")"
	}
	return v
}
