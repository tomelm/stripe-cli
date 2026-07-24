package coop

import (
	"fmt"
	"strings"
)

const (
	MaxLifecycleFactsPerBlueprint = 32
	MaxRequiredOutcomesPerNode    = 8
	MaxFactRefsPerOutcome         = 8
	MaxLifecycleIDBytes           = 64
	MaxLifecycleStatementBytes    = MaxCheckResultDetailBytes
)

// validateBlueprintOutcomes rejects malformed or unbounded application
// contracts before they can be persisted in a session or shown to an agent.
func validateBlueprintOutcomes(blueprint *Blueprint) error {
	if blueprint == nil {
		return fmt.Errorf("blueprint is required")
	}
	if len(blueprint.LifecycleFacts) > MaxLifecycleFactsPerBlueprint {
		return fmt.Errorf("lifecycle_facts exceeds %d entries", MaxLifecycleFactsPerBlueprint)
	}

	facts := make(map[string]bool, len(blueprint.LifecycleFacts))
	for index, fact := range blueprint.LifecycleFacts {
		if err := validateLifecycleID(fact.ID); err != nil {
			return fmt.Errorf("lifecycle_facts[%d]: %w", index, err)
		}
		if facts[fact.ID] {
			return fmt.Errorf("duplicate lifecycle fact id %q", fact.ID)
		}
		if err := validateLifecycleStatement("lifecycle fact "+fact.ID, fact.Statement); err != nil {
			return err
		}
		facts[fact.ID] = true
	}

	for _, step := range blueprint.Steps {
		for _, node := range step.Nodes {
			if err := validateNodeOutcomes(step.Key, node, facts); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateNodeOutcomes(stepKey string, node NodeDefinition, facts map[string]bool) error {
	nodeKey := stepKey + "." + node.Key
	if len(node.RequiredOutcomes) > MaxRequiredOutcomesPerNode {
		return fmt.Errorf("node %q required_outcomes exceeds %d entries", nodeKey, MaxRequiredOutcomesPerNode)
	}

	outcomes := make(map[string]bool, len(node.RequiredOutcomes))
	for index, outcome := range node.RequiredOutcomes {
		if err := validateLifecycleID(outcome.ID); err != nil {
			return fmt.Errorf("node %q required_outcomes[%d]: %w", nodeKey, index, err)
		}
		if outcomes[outcome.ID] {
			return fmt.Errorf("node %q has duplicate required outcome id %q", nodeKey, outcome.ID)
		}
		if err := validateLifecycleStatement("node "+nodeKey+" outcome "+outcome.ID, outcome.Statement); err != nil {
			return err
		}
		if len(outcome.FactRefs) == 0 {
			return fmt.Errorf("node %q outcome %q must reference at least one lifecycle fact", nodeKey, outcome.ID)
		}
		if len(outcome.FactRefs) > MaxFactRefsPerOutcome {
			return fmt.Errorf(
				"node %q outcome %q fact_refs exceeds %d entries",
				nodeKey,
				outcome.ID,
				MaxFactRefsPerOutcome,
			)
		}

		refs := make(map[string]bool, len(outcome.FactRefs))
		for refIndex, factID := range outcome.FactRefs {
			if err := validateLifecycleID(factID); err != nil {
				return fmt.Errorf(
					"node %q outcome %q fact_refs[%d]: %w",
					nodeKey,
					outcome.ID,
					refIndex,
					err,
				)
			}
			if refs[factID] {
				return fmt.Errorf(
					"node %q outcome %q references lifecycle fact %q more than once",
					nodeKey,
					outcome.ID,
					factID,
				)
			}
			if !facts[factID] {
				return fmt.Errorf(
					"node %q outcome %q references unknown lifecycle fact %q",
					nodeKey,
					outcome.ID,
					factID,
				)
			}
			refs[factID] = true
		}
		outcomes[outcome.ID] = true
	}
	return nil
}

func validateLifecycleID(id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if len(id) > MaxLifecycleIDBytes {
		return fmt.Errorf("id %q exceeds %d bytes", id, MaxLifecycleIDBytes)
	}
	for index, character := range id {
		valid := character >= 'a' && character <= 'z'
		if index > 0 {
			valid = valid ||
				character >= '0' && character <= '9' ||
				character == '_' ||
				character == '-' ||
				character == '.'
		}
		if !valid {
			return fmt.Errorf(
				"id %q must start with a lowercase letter and contain only lowercase letters, digits, '.', '_' or '-'",
				id,
			)
		}
	}
	return nil
}

func validateLifecycleStatement(label, statement string) error {
	if strings.TrimSpace(statement) == "" {
		return fmt.Errorf("%s statement is required", label)
	}
	if strings.TrimSpace(statement) != statement {
		return fmt.Errorf("%s statement must be trimmed", label)
	}
	if err := ValidateSessionText(label+" statement", statement, MaxLifecycleStatementBytes); err != nil {
		return err
	}
	return nil
}

// LifecycleFactsForNode returns only the canonical facts referenced by a
// node's required outcomes, preserving their blueprint order.
func LifecycleFactsForNode(session *Session, node *SessionNode) []LifecycleFact {
	if session == nil || node == nil || len(node.RequiredOutcomes) == 0 {
		return nil
	}

	referenced := make(map[string]bool)
	for _, outcome := range node.RequiredOutcomes {
		for _, factID := range outcome.FactRefs {
			referenced[factID] = true
		}
	}
	facts := make([]LifecycleFact, 0, len(referenced))
	for _, fact := range session.LifecycleFacts {
		if referenced[fact.ID] {
			facts = append(facts, fact)
		}
	}
	return facts
}

// RequiredOutcomesForNode returns a deep copy of the node contract so callers
// can safely hand it to an evaluator or command response.
func RequiredOutcomesForNode(node *SessionNode) []RequiredOutcome {
	if node == nil {
		return nil
	}
	return cloneRequiredOutcomes(node.RequiredOutcomes)
}

func cloneLifecycleFacts(facts []LifecycleFact) []LifecycleFact {
	return append([]LifecycleFact(nil), facts...)
}

func cloneRequiredOutcomes(outcomes []RequiredOutcome) []RequiredOutcome {
	cloned := make([]RequiredOutcome, len(outcomes))
	for index, outcome := range outcomes {
		cloned[index] = outcome
		cloned[index].FactRefs = append([]string(nil), outcome.FactRefs...)
	}
	return cloned
}
