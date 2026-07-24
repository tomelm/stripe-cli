package coop

import (
	"fmt"
	"sort"
	"strings"
)

// AttemptAssessment is a pure projection of persisted evidence. Workflow
// decisions and presentation copy remain with their owning packages.
type AttemptAssessment struct {
	Required, RequiredPassed                  int
	RequiredFailures, Blocking                []CheckResult
	RequiredPending, RequiredUnavailable      []CheckResult
	DirectPassed, DirectPending               int
	DirectUnavailable, AdvisoryDirectIssues   []CheckResult
	Supporting, SupportingFailures            int
	RequiredCoverageUnavailable               int
	AdvisoryCoverageUnavailable               int
	AgentChecks, AgentChecksPassed            int
	RequiredStatePassed, HasObservedCandidate bool
}

// AssessAttempts classifies attempts in caller order without mutating them.
func AssessAttempts(attempts ...*NodeAttempt) AttemptAssessment {
	var assessment AttemptAssessment
	for _, attempt := range attempts {
		if attempt == nil {
			continue
		}
		for _, binding := range attempt.Resources {
			if binding.Source == BindingObservedCandidate {
				assessment.HasObservedCandidate = true
				break
			}
		}
		for _, result := range attempt.Results {
			if result.Importance == CheckRequired {
				assessment.Required++
				switch result.Status {
				case CheckPassed:
					assessment.RequiredPassed++
					assessment.RequiredStatePassed = assessment.RequiredStatePassed || result.Kind == CheckState
				case CheckFailed:
					assessment.RequiredFailures = append(assessment.RequiredFailures, result)
					assessment.Blocking = append(assessment.Blocking, result)
					continue
				case CheckPending:
					assessment.RequiredPending = append(assessment.RequiredPending, result)
				case CheckUnavailable:
					assessment.RequiredUnavailable = append(assessment.RequiredUnavailable, result)
				}
			}
			switch result.Kind {
			case CheckRequest, CheckEvent:
				if result.Status == CheckObserved || result.Status == CheckFailed {
					assessment.Supporting++
				}
				if result.Status == CheckFailed {
					assessment.SupportingFailures++
				}
			case CheckResource, CheckState:
				switch result.Status {
				case CheckPassed:
					assessment.DirectPassed++
				case CheckPending:
					assessment.DirectPending++
				case CheckUnavailable:
					assessment.DirectUnavailable = append(assessment.DirectUnavailable, result)
					assessment.addAdvisoryDirectIssue(result)
				case CheckFailed:
					assessment.addAdvisoryDirectIssue(result)
				}
			case CheckCoverage:
				if result.Status == CheckUnavailable {
					if result.Importance == CheckRequired {
						assessment.RequiredCoverageUnavailable++
					} else {
						assessment.AdvisoryCoverageUnavailable++
					}
				}
			}
		}

		latest := make(map[string]bool, len(attempt.AgentChecks))
		for _, check := range attempt.AgentChecks {
			assessment.AgentChecks++
			if check.Passed {
				assessment.AgentChecksPassed++
			}
			if label := strings.TrimSpace(check.Check); label != "" {
				latest[label] = check.Passed
			}
		}
		var failed []string
		for label, passed := range latest {
			if !passed {
				failed = append(failed, label)
			}
		}
		sort.Strings(failed)
		for index, label := range failed {
			assessment.Blocking = append(assessment.Blocking, CheckResult{
				ID: fmt.Sprintf("agent.reported.%d", index+1), Kind: CheckApp,
				Importance: CheckRequired, Status: CheckFailed,
				Detail: "Agent reported a failing check: " + label,
				Repair: "Fix the issue and rerun this check before reporting the attempt.",
			})
		}
	}
	return assessment
}

func (assessment *AttemptAssessment) addAdvisoryDirectIssue(result CheckResult) {
	if result.Importance == CheckAdvisory {
		assessment.AdvisoryDirectIssues = append(assessment.AdvisoryDirectIssues, result)
	}
}
