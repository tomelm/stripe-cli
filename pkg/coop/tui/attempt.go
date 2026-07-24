package tui

import "github.com/stripe/stripe-cli/pkg/coop"

// presentationAttempt returns the newest attempt, whether it is still open or
// retained as completed history. TUI projections should not read the legacy
// node-level implementation and verification fields.
func presentationAttempt(node *coop.SessionNode) *coop.NodeAttempt {
	if node == nil || len(node.Attempts) == 0 {
		return nil
	}
	return &node.Attempts[len(node.Attempts)-1]
}

func completedWithoutAutomaticVerification(node *coop.SessionNode) bool {
	attempt := presentationAttempt(node)
	return node != nil && node.State == coop.NodeDone && attempt != nil && attempt.EndReason == coop.AttemptCompletedUnverified
}

func completedWithUnavailableVerification(node *coop.SessionNode) bool {
	if !completedWithoutAutomaticVerification(node) {
		return false
	}
	return len(coop.AssessAttempts(presentationAttempt(node)).RequiredUnavailable) > 0
}

func completedWithVerificationOverride(node *coop.SessionNode) bool {
	attempt := presentationAttempt(node)
	return node != nil && node.State == coop.NodeDone && attempt != nil && attempt.Override != nil
}
