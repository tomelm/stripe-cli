package coop

// AsyncHandlerStateVerifiedSummary is the shared human- and agent-facing
// description for an async handler whose required Stripe checks passed. Those
// checks verify Stripe state, not whether application code processed an event.
const AsyncHandlerStateVerifiedSummary = "Stripe state verified · handler processing unverified"

// AsyncHandlerCompletionSummary returns the narrow completion disclosure for
// an async handler whose required direct checks all passed. Other unverified
// completions keep their unavailable or no-rule presentation.
func AsyncHandlerCompletionSummary(node *SessionNode) string {
	if node == nil || node.Type != NodeAsyncHandler || node.State != NodeDone || len(node.Attempts) == 0 {
		return ""
	}
	attempt := &node.Attempts[len(node.Attempts)-1]
	if attempt.EndReason != AttemptCompletedUnverified {
		return ""
	}

	assessment := AssessAttempts(attempt)
	if assessment.Required == 0 ||
		assessment.RequiredPassed != assessment.Required ||
		!assessment.RequiredStatePassed {
		return ""
	}
	return AsyncHandlerStateVerifiedSummary
}
