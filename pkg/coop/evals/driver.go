package evals

import (
	"context"
	"fmt"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

func driveHuman(ctx context.Context, store *coop.Store, sessionID string, plan []HumanAction) ([]DriverAction, error) {
	service := workflow.NewService(store)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var actions []DriverAction
	reviewCount := 0
	completionSelected := false
	for {
		select {
		case <-ctx.Done():
			return actions, ctx.Err()
		case <-ticker.C:
		}

		session, err := store.Read(sessionID)
		if err != nil {
			return actions, err
		}
		if session.IsComplete() {
			if session.NextSteps != nil && len(session.NextSteps.Suggestions) > 0 && !completionSelected {
				action := selectCompletionAction(plan)
				selected := action.Select
				if selected == "" {
					selected = "done"
				}
				_, err := store.Update(sessionID, func(session *coop.Session) error {
					if session.NextSteps == nil {
						session.NextSteps = &coop.NextStepsState{}
					}
					session.NextSteps.Selected = selected
					return nil
				})
				if err != nil {
					return actions, err
				}
				completionSelected = true
				actions = append(actions, DriverAction{When: "completion", Action: "select", Selected: selected, At: time.Now().UTC()})
			}
			if completionSelected {
				return actions, nil
			}
			continue
		}

		target, ok := reviewTargetForSession(session)
		if !ok {
			continue
		}
		heartbeatAge, err := store.HeartbeatAge(sessionID)
		if err != nil {
			return actions, err
		}
		heartbeatSeen := heartbeatAge >= 0 && heartbeatAge < 5*time.Second
		if !heartbeatSeen {
			continue
		}
		next := reviewActionFor(plan, reviewCount)
		if next.Action == "" {
			next = HumanAction{When: "review", Action: "confirm"}
		}
		driverAction := DriverAction{
			When:          next.When,
			Action:        next.Action,
			Steps:         append([]int(nil), target.steps...),
			Chapter:       target.chapter,
			Note:          next.Note,
			HeartbeatSeen: heartbeatSeen,
			At:            time.Now().UTC(),
		}
		switch next.Action {
		case "request_changes":
			note := next.Note
			if note == "" {
				note = "Please tighten this implementation and report concrete verification."
			}
			driverAction.Note = note
			if _, err := service.RequestChanges(sessionID, target.steps, note); err != nil {
				return actions, err
			}
		case "confirm", "":
			if _, err := service.ConfirmReview(sessionID, target.steps); err != nil {
				return actions, err
			}
		default:
			return actions, fmt.Errorf("unsupported human action %q", next.Action)
		}
		actions = append(actions, driverAction)
		reviewCount++
	}
}

type reviewTarget struct {
	steps   []int
	chapter string
}

func reviewTargetForSession(session *coop.Session) (reviewTarget, bool) {
	for stepIndex, stepDef := range session.Steps {
		if !session.StepReadyForReview(stepIndex) || !session.StepHasReview(stepIndex) {
			continue
		}
		var steps []int
		step := 0
		for i := range session.Steps {
			for j := range session.Steps[i].Nodes {
				step++
				if i == stepIndex && session.Steps[i].Nodes[j].State == coop.NodeReview {
					steps = append(steps, step)
				}
			}
		}
		if len(steps) > 0 {
			return reviewTarget{steps: steps, chapter: stepDef.Title}, true
		}
	}
	step := 0
	for i := range session.Steps {
		for j := range session.Steps[i].Nodes {
			step++
			node := session.Steps[i].Nodes[j]
			if node.State == coop.NodeReview {
				return reviewTarget{steps: []int{step}, chapter: session.Steps[i].Title}, true
			}
		}
	}
	return reviewTarget{}, false
}

func reviewActionFor(plan []HumanAction, reviewCount int) HumanAction {
	for _, action := range plan {
		switch action.When {
		case "first_review":
			if reviewCount == 0 {
				return action
			}
		case "next_review":
			if reviewCount > 0 {
				return action
			}
		case "review", "every_review":
			return action
		}
	}
	return HumanAction{When: "review", Action: "confirm"}
}

func selectCompletionAction(plan []HumanAction) HumanAction {
	for _, action := range plan {
		if action.When == "completion" {
			return action
		}
	}
	return HumanAction{When: "completion", Action: "select", Select: "done"}
}
