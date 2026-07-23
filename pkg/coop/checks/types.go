// Package checks compiles the declarative verification facts already present
// in co-op blueprints into concrete resource and state checks.
package checks

// PredicateKind is the closed set of comparisons understood by verification
// evaluators. The catalog deliberately cannot express arbitrary paths,
// operators, or executable behavior.
type PredicateKind string

const (
	PredicateEq            PredicateKind = "eq"
	PredicateOneOf         PredicateKind = "one_of"
	PredicatePresent       PredicateKind = "present"
	PredicatePositive      PredicateKind = "positive"
	PredicateEqualsInput   PredicateKind = "equals_input"
	PredicateEqualsBinding PredicateKind = "equals_binding"
)

// RuleID identifies one of the two concrete verification behaviors. These
// identifiers are durable result keys, not dynamically registered handlers.
type RuleID string

const (
	RuleResourceMatches RuleID = "resource.matches"
	RuleStateMatches    RuleID = "state.matches"
)

// Importance tells workflow policy whether a definitive contradiction should
// return work to the agent or remain supporting evidence.
type Importance string

const (
	ImportanceBlocking Importance = "blocking"
	ImportanceAdvisory Importance = "advisory"
)

// Catalog is the embedded, code-owned Stripe verification knowledge shared by
// every blueprint. Blueprints provide request and event facts; this catalog
// describes only how supported Stripe objects can be read and checked.
type Catalog struct {
	Rules     []RuleDefinition `json:"rules"`
	Resources []ResourceRule   `json:"resources"`
	Events    []EventRule      `json:"events"`
}

// RuleDefinition owns the stable policy and repair copy for a code-backed
// rule. The catalog cannot define new execution behavior.
type RuleDefinition struct {
	ID         RuleID     `json:"id"`
	Importance Importance `json:"importance"`
	Repair     string     `json:"repair"`
}

// RequestPattern identifies an exact Stripe API operation.
type RequestPattern struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// ResourceRule describes one resource created by an exact API operation.
// Predicates are applied only when their declared input exists on that
// operation; unsupported input-to-output mappings are intentionally absent.
type ResourceRule struct {
	Type       string              `json:"type"`
	Role       string              `json:"role"`
	Create     RequestPattern      `json:"create"`
	Retrieve   string              `json:"retrieve,omitempty"`
	IDPrefixes []string            `json:"id_prefixes,omitempty"`
	Predicates []PredicateTemplate `json:"predicates,omitempty"`
	Evidence   []EvidenceRule      `json:"evidence,omitempty"`
}

// EvidenceRule declares one bounded follow-up read rooted in the created
// resource. When FromField is empty, {id} is the created resource ID (for a
// child collection). Otherwise {id} is read from that field and validated
// against IDPrefixes before the related object is fetched. Eventual marks a
// relation that may not exist until later state is reached. Evidence verifies
// facts about an object, but it never establishes that an account-wide event
// belongs to one attempt.
type EvidenceRule struct {
	ID         string              `json:"id"`
	Retrieve   string              `json:"retrieve"`
	FromField  string              `json:"from_field,omitempty"`
	IDPrefixes []string            `json:"id_prefixes,omitempty"`
	WhenInput  string              `json:"when_input,omitempty"`
	Eventual   bool                `json:"eventual,omitempty"`
	Predicates []PredicateTemplate `json:"predicates"`
	Repair     string              `json:"repair"`
}

// EventRule maps one exact event type to state predicates on its data object.
// Predicates are the ANDed success condition. TerminalFailures are individual
// conditions that prove the expected state can no longer be reached.
type EventRule struct {
	Type             string                 `json:"type"`
	Resource         string                 `json:"resource"`
	Predicates       []PredicateTemplate    `json:"predicates"`
	TerminalFailures []TerminalFailTemplate `json:"terminal_fail,omitempty"`
}

// TerminalFailTemplate declares one state that is definitively wrong rather
// than merely still progressing. Repair overrides the state rule's generic
// guidance when this condition matches.
type TerminalFailTemplate struct {
	Predicate PredicateTemplate `json:"when"`
	Repair    string            `json:"repair,omitempty"`
}

// PredicateTemplate is the strictly validated catalog representation of a
// predicate. Input is a dotted path in the consuming API request. Value and
// Values are available only to eq and one_of respectively.
type PredicateTemplate struct {
	Kind   PredicateKind `json:"op"`
	Field  string        `json:"field"`
	Input  string        `json:"input,omitempty"`
	Value  string        `json:"value,omitempty"`
	Values []string      `json:"values,omitempty"`
}

// Source identifies the immutable blueprint node (and, for test helpers, the
// named request) that declared a check.
type Source struct {
	Step    string
	Node    string
	Request string
}

// BindingRef identifies a value produced by an earlier blueprint node.
// Request is populated for named test-helper requests or indexed event
// outputs. Field is the referenced output field, usually "id".
type BindingRef struct {
	Step    string
	Node    string
	Request string
	Field   string
}

// Predicate is a compiled predicate. Equals-binding predicates carry the
// exact parsed node reference; equals-input predicates retain the supported
// request input path for an evaluator to compare with the actual request.
type Predicate struct {
	Kind    PredicateKind
	Field   string
	Input   string
	Value   string
	Values  []string
	Binding *BindingRef
}

// TerminalFail is one compiled, independently matching terminal condition.
type TerminalFail struct {
	Predicate Predicate
	Repair    string
}

// CheckMeta is shared immutable identity and policy carried by every compiled
// check. Source.Node is the owning node for persistence and agent feedback.
type CheckMeta struct {
	ID         string
	RuleID     RuleID
	Importance Importance
	Repair     string
	Source     Source
}

// ResourceCheck checks the object created by a request against its supported
// structural inputs and bindings.
type ResourceCheck struct {
	CheckMeta
	ResourceType string
	Role         string
	RetrievePath string
	IDPrefixes   []string
	Predicates   []Predicate
	Evidence     []EvidenceCheck
}

// EvidenceCheck is the compiled form of one catalog follow-up read.
type EvidenceCheck struct {
	ID           string
	RetrievePath string
	FromField    string
	IDPrefixes   []string
	Eventual     bool
	Predicates   []Predicate
	Repair       string
}

// StateCheck verifies authoritative resource state associated with a declared
// event. Observing the event supplies a trigger or candidate ID, never proof.
type StateCheck struct {
	CheckMeta
	EventType        string
	ResourceType     string
	Role             string
	RetrievePath     string
	Predicates       []Predicate
	TerminalFailures []TerminalFail
}

// CoverageGap records a blueprint fact for which no resource/state rule is
// available. It is explicit and node-owned so unsupported coverage can never
// be mistaken for a pass.
type CoverageGap struct {
	CheckMeta
	Reason string
}

// StepPlan is the deterministic result of compiling one session step.
type StepPlan struct {
	StepKey      string
	Resources    []ResourceCheck
	States       []StateCheck
	CoverageGaps []CoverageGap
}

// ForNode returns the checks declared by one node without changing the
// compiled step plan. A UI review may deliberately consume the full plan,
// while ordinary agent work remains node-scoped.
func (plan StepPlan) ForNode(nodeKey string) StepPlan {
	result := StepPlan{StepKey: plan.StepKey}
	for _, check := range plan.Resources {
		if check.Source.Node == nodeKey {
			result.Resources = append(result.Resources, check)
		}
	}
	for _, check := range plan.States {
		if check.Source.Node == nodeKey {
			result.States = append(result.States, check)
		}
	}
	for _, gap := range plan.CoverageGaps {
		if gap.Source.Node == nodeKey {
			result.CoverageGaps = append(result.CoverageGaps, gap)
		}
	}
	return result
}
