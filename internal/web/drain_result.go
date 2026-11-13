package web

import "autodeploy/internal/store/postgres"

// drainResultView is the "Predecessor drain" row of the deployment detail page
// (step 20B). A stop failure uses the existing failure pill class.
type drainResultView struct {
	Outcome string
	Summary string
	Failed  bool
}

func newDrainResultView(r *postgres.DrainResultView) *drainResultView {
	if r == nil {
		return nil
	}
	return &drainResultView{
		Outcome: r.Outcome, Summary: r.Summary,
		Failed: r.Outcome != postgres.DrainOutcomeStopped && r.Outcome != postgres.DrainOutcomeAlreadyStopped && r.Outcome != postgres.DrainOutcomeGone,
	}
}
