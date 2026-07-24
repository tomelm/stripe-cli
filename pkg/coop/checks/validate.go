package checks

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const (
	maxCatalogEntries = 256
	maxCatalogText    = 512
	maxIDPrefixBytes  = 32
	maxEvidenceReads  = 2
	maxPredicates     = 8
)

type predicateValidationContext uint8

const (
	predicateStatic predicateValidationContext = iota
	predicateRequest
	predicateEvent
)

var (
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	fieldPattern    = regexp.MustCompile(`^[a-zA-Z0-9_]+(?:\.[a-zA-Z0-9_]+)*$`)
	idPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]*_$`)
)

var credentialPrefixes = []string{"ek_", "ephkey_", "pk_", "rk_", "rkcs_", "sess_", "sk_", "whsec_"}

// Validate checks all cross-references and enforces the catalog's deliberately
// small, non-extensible predicate grammar.
func (catalog Catalog) Validate() error {
	rules, err := catalog.validateRules()
	if err != nil {
		return err
	}
	resources, err := catalog.validateResources()
	if err != nil {
		return err
	}
	if err := catalog.validateEvents(resources); err != nil {
		return err
	}
	for _, required := range []RuleID{RuleResourceMatches, RuleStateMatches} {
		if _, exists := rules[required]; !exists {
			return fmt.Errorf("required rule %q is not declared", required)
		}
	}
	return nil
}

func (catalog Catalog) validateResources() (map[string]ResourceRule, error) {
	if len(catalog.Resources) == 0 || len(catalog.Resources) > maxCatalogEntries {
		return nil, fmt.Errorf("resources must contain 1..%d entries", maxCatalogEntries)
	}

	resources := make(map[string]ResourceRule, len(catalog.Resources))
	operations := make(map[string]string, len(catalog.Resources))
	roles := make(map[string]string, len(catalog.Resources))
	for index, resource := range catalog.Resources {
		where := fmt.Sprintf("resources[%d]", index)
		if !namePattern.MatchString(resource.Type) {
			return nil, fmt.Errorf("%s.type %q must be lower snake case", where, resource.Type)
		}
		if !namePattern.MatchString(resource.Role) {
			return nil, fmt.Errorf("%s.role %q must be lower snake case", where, resource.Role)
		}
		if _, exists := resources[resource.Type]; exists {
			return nil, fmt.Errorf("duplicate resource type %q", resource.Type)
		}
		if previous, exists := roles[resource.Role]; exists {
			return nil, fmt.Errorf("resource role %q is shared by %q and %q", resource.Role, previous, resource.Type)
		}
		if err := validateRequestPattern(resource.Create, where+".create"); err != nil {
			return nil, err
		}
		operation := strings.ToUpper(resource.Create.Method) + " " + resource.Create.Path
		if previous, exists := operations[operation]; exists {
			return nil, fmt.Errorf("creation operation %q is shared by %q and %q", operation, previous, resource.Type)
		}
		if err := validateCatalogPath(resource.Retrieve, where+".retrieve", true); err != nil {
			return nil, err
		}
		if err := validateIDPrefixes(resource.IDPrefixes, where+".id_prefixes"); err != nil {
			return nil, err
		}
		if err := validatePredicates(resource.Predicates, where+".predicates", predicateRequest); err != nil {
			return nil, err
		}
		if err := validateEvidence(resource.Evidence, where+".evidence"); err != nil {
			return nil, err
		}
		resources[resource.Type] = resource
		roles[resource.Role] = resource.Type
		operations[operation] = resource.Type
	}
	return resources, nil
}

func (catalog Catalog) validateEvents(resources map[string]ResourceRule) error {
	if len(catalog.Events) > maxCatalogEntries {
		return fmt.Errorf("events exceeds %d entries", maxCatalogEntries)
	}
	seenEvents := make(map[string]bool, len(catalog.Events))
	for index, event := range catalog.Events {
		where := fmt.Sprintf("events[%d]", index)
		if event.Type == "" || len(event.Type) > maxCatalogText || strings.ContainsAny(event.Type, " \t\r\n") {
			return fmt.Errorf("%s.type must be a non-empty event name without whitespace", where)
		}
		if seenEvents[event.Type] {
			return fmt.Errorf("duplicate event type %q", event.Type)
		}
		if _, exists := resources[event.Resource]; !exists {
			return fmt.Errorf("%s.resource %q is not declared", where, event.Resource)
		}
		if len(event.Predicates) == 0 {
			return fmt.Errorf("%s.predicates must not be empty", where)
		}
		if err := validatePredicates(event.Predicates, where+".predicates", predicateEvent); err != nil {
			return err
		}
		terminalPredicates := make([]PredicateTemplate, 0, len(event.TerminalFailures))
		for terminalIndex, terminal := range event.TerminalFailures {
			terminalWhere := fmt.Sprintf("%s.terminal_fail[%d]", where, terminalIndex)
			if terminal.Repair != "" && (strings.TrimSpace(terminal.Repair) == "" || len(terminal.Repair) > maxCatalogText) {
				return fmt.Errorf("%s.repair must be empty or contain 1..%d bytes", terminalWhere, maxCatalogText)
			}
			terminalPredicates = append(terminalPredicates, terminal.Predicate)
		}
		if err := validatePredicates(terminalPredicates, where+".terminal_fail", predicateStatic); err != nil {
			return err
		}
		if err := validateDistinctStatePredicates(event.Predicates, terminalPredicates, where); err != nil {
			return err
		}
		seenEvents[event.Type] = true
	}
	return nil
}

func validateDistinctStatePredicates(pass, terminal []PredicateTemplate, where string) error {
	seen := make(map[string]bool, len(pass)+len(terminal))
	for _, predicate := range pass {
		seen[predicateKey(predicate)] = true
	}
	for _, predicate := range terminal {
		key := predicateKey(predicate)
		if seen[key] {
			return fmt.Errorf("%s uses predicate %q as both a pass and terminal condition", where, key)
		}
		seen[key] = true
	}
	return nil
}

func predicateKey(predicate PredicateTemplate) string {
	return strings.Join([]string{
		string(predicate.Kind), predicate.Field, predicate.Input,
		predicate.Value, strings.Join(predicate.Values, "\x00"),
	}, "|")
}

func validateEvidence(evidence []EvidenceRule, where string) error {
	if len(evidence) > maxEvidenceReads {
		return fmt.Errorf("%s exceeds %d entries", where, maxEvidenceReads)
	}
	seen := make(map[string]bool, len(evidence))
	for index, item := range evidence {
		itemWhere := fmt.Sprintf("%s[%d]", where, index)
		if !namePattern.MatchString(item.ID) {
			return fmt.Errorf("%s.id %q must be lower snake case", itemWhere, item.ID)
		}
		if seen[item.ID] {
			return fmt.Errorf("%s contains duplicate id %q", where, item.ID)
		}
		seen[item.ID] = true
		if (item.WhenInput == "") != (item.WhenValue == "") {
			return fmt.Errorf("%s.when_input and when_value must be declared together", itemWhere)
		}
		if item.WhenInput != "" && (!fieldPattern.MatchString(item.WhenInput) || len(item.WhenInput) > maxCatalogText) {
			return fmt.Errorf("%s.when_input %q is invalid", itemWhere, item.WhenInput)
		}
		if item.WhenValue != "" && (strings.TrimSpace(item.WhenValue) == "" || len(item.WhenValue) > maxCatalogText) {
			return fmt.Errorf("%s.when_value must contain 1..%d bytes", itemWhere, maxCatalogText)
		}
		if err := validateCatalogPath(item.Retrieve, itemWhere+".retrieve", true); err != nil {
			return err
		}
		if item.FromField == "" {
			if len(item.IDPrefixes) != 0 {
				return fmt.Errorf("%s.id_prefixes requires from_field", itemWhere)
			}
			if item.Eventual {
				return fmt.Errorf("%s.eventual requires from_field", itemWhere)
			}
		} else {
			if !fieldPattern.MatchString(item.FromField) || len(item.FromField) > maxCatalogText {
				return fmt.Errorf("%s.from_field %q is invalid", itemWhere, item.FromField)
			}
			if len(item.IDPrefixes) == 0 {
				return fmt.Errorf("%s.id_prefixes must identify the related object", itemWhere)
			}
			if err := validateIDPrefixes(item.IDPrefixes, itemWhere+".id_prefixes"); err != nil {
				return err
			}
		}
		if len(item.Predicates) == 0 {
			return fmt.Errorf("%s.predicates must not be empty", itemWhere)
		}
		if err := validatePredicates(item.Predicates, itemWhere+".predicates", predicateRequest); err != nil {
			return err
		}
		if strings.TrimSpace(item.Repair) == "" || len(item.Repair) > maxCatalogText {
			return fmt.Errorf("%s.repair must contain 1..%d bytes", itemWhere, maxCatalogText)
		}
	}
	return nil
}

func (catalog Catalog) validateRules() (map[RuleID]RuleDefinition, error) {
	if len(catalog.Rules) == 0 || len(catalog.Rules) > 16 {
		return nil, fmt.Errorf("rules must contain 1..16 entries")
	}
	rules := make(map[RuleID]RuleDefinition, len(catalog.Rules))
	requiredImportance := map[RuleID]Importance{
		RuleResourceMatches: ImportanceBlocking,
		RuleStateMatches:    ImportanceBlocking,
	}
	for index, rule := range catalog.Rules {
		where := fmt.Sprintf("rules[%d]", index)
		switch rule.ID {
		case RuleResourceMatches, RuleStateMatches:
		default:
			return nil, fmt.Errorf("%s.id %q is not a code-backed rule", where, rule.ID)
		}
		if _, exists := rules[rule.ID]; exists {
			return nil, fmt.Errorf("duplicate rule %q", rule.ID)
		}
		if rule.Importance != ImportanceBlocking && rule.Importance != ImportanceAdvisory {
			return nil, fmt.Errorf("%s.importance %q is invalid", where, rule.Importance)
		}
		if rule.Importance != requiredImportance[rule.ID] {
			return nil, fmt.Errorf("%s.importance for %q must be %q", where, rule.ID, requiredImportance[rule.ID])
		}
		if strings.TrimSpace(rule.Repair) == "" || len(rule.Repair) > maxCatalogText {
			return nil, fmt.Errorf("%s.repair must contain 1..%d bytes", where, maxCatalogText)
		}
		rules[rule.ID] = rule
	}
	return rules, nil
}

func validateRequestPattern(pattern RequestPattern, where string) error {
	method := strings.ToUpper(strings.TrimSpace(pattern.Method))
	if method == "" || method != pattern.Method {
		return fmt.Errorf("%s.method must be uppercase", where)
	}
	return validateCatalogPath(pattern.Path, where+".path", false)
}

func validateCatalogPath(path, where string, retrieve bool) error {
	if path == "" || len(path) > maxCatalogText {
		return fmt.Errorf("%s must contain 1..%d bytes", where, maxCatalogText)
	}
	if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/v2/") {
		return fmt.Errorf("%s must be a relative Stripe API path beginning with /v1/ or /v2/; schemes and hosts are not allowed", where)
	}
	if containsPathWhitespace(path) {
		return fmt.Errorf("%s must not contain whitespace or control characters", where)
	}
	if strings.ContainsAny(path, "?#") {
		return fmt.Errorf("%s must not contain a query or fragment", where)
	}

	decoded, err := url.PathUnescape(path)
	if err != nil {
		return fmt.Errorf("%s contains invalid path escaping: %w", where, err)
	}
	if containsPathWhitespace(decoded) {
		return fmt.Errorf("%s must not contain encoded whitespace or control characters", where)
	}
	if strings.Contains(decoded, "\\") {
		return fmt.Errorf("%s must not contain backslashes", where)
	}
	if strings.Contains(decoded, "//") {
		return fmt.Errorf("%s must not contain an empty path segment or double slash", where)
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s must not contain path traversal segments", where)
		}
	}
	if decoded != path {
		return fmt.Errorf("%s must not use percent-encoded path text", where)
	}

	if !retrieve {
		if strings.ContainsAny(path, "{}") {
			return fmt.Errorf("%s must not contain path templates", where)
		}
		return nil
	}
	if strings.Count(path, "{id}") != 1 {
		return fmt.Errorf("%s must contain exactly one {id} placeholder", where)
	}
	withoutID := strings.Replace(path, "{id}", "", 1)
	if strings.ContainsAny(withoutID, "{}") {
		return fmt.Errorf("%s contains an unexpected path template; only {id} is allowed", where)
	}
	for _, segment := range strings.Split(path, "/") {
		if strings.Contains(segment, "{id}") && segment != "{id}" {
			return fmt.Errorf("%s {id} placeholder must occupy one complete path segment", where)
		}
	}
	return nil
}

func containsPathWhitespace(path string) bool {
	return strings.IndexFunc(path, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) >= 0
}

func validateIDPrefixes(prefixes []string, where string) error {
	seen := make(map[string]bool, len(prefixes))
	for index, prefix := range prefixes {
		item := fmt.Sprintf("%s[%d]", where, index)
		if len(prefix) < 2 || len(prefix) > maxIDPrefixBytes || !idPrefixPattern.MatchString(prefix) {
			return fmt.Errorf("%s must be a %d-byte-or-shorter ASCII alphanumeric/underscore Stripe ID prefix ending in underscore", item, maxIDPrefixBytes)
		}
		lower := strings.ToLower(prefix)
		for _, credentialPrefix := range credentialPrefixes {
			if strings.HasPrefix(lower, credentialPrefix) {
				return fmt.Errorf("%s must not use credential prefix %q", item, credentialPrefix)
			}
		}
		if seen[prefix] {
			return fmt.Errorf("%s contains duplicate %q", where, prefix)
		}
		seen[prefix] = true
	}
	return nil
}

func validatePredicates(predicates []PredicateTemplate, where string, context predicateValidationContext) error {
	if len(predicates) > maxPredicates {
		return fmt.Errorf("%s exceeds %d entries", where, maxPredicates)
	}
	seen := make(map[string]bool, len(predicates))
	resultKeys := make(map[string]bool, len(predicates))
	for index, predicate := range predicates {
		item := fmt.Sprintf("%s[%d]", where, index)
		if !fieldPattern.MatchString(predicate.Field) || len(predicate.Field) > maxCatalogText {
			return fmt.Errorf("%s.field %q is invalid", item, predicate.Field)
		}
		key := predicateKey(predicate)
		if seen[key] {
			return fmt.Errorf("%s duplicates predicate %q", item, key)
		}
		seen[key] = true
		if context == predicateRequest {
			resultKey := predicateResultKey(predicate.Field)
			if resultKeys[resultKey] {
				return fmt.Errorf("%s emits duplicate result id %q", item, resultKey)
			}
			resultKeys[resultKey] = true
		}
		if err := validatePredicateShape(predicate, item, context); err != nil {
			return err
		}
	}
	return nil
}

func validatePredicateShape(predicate PredicateTemplate, item string, context predicateValidationContext) error {
	switch predicate.Kind {
	case PredicateEq, PredicateOneOf, PredicatePresent, PredicatePositive:
		return validateStaticPredicateShape(predicate, item)
	case PredicateEqualsInput, PredicateEqualsBinding:
		return validateRequestPredicateShape(predicate, item, context)
	default:
		return fmt.Errorf("%s.op %q is invalid", item, predicate.Kind)
	}
}

func validateStaticPredicateShape(predicate PredicateTemplate, item string) error {
	switch predicate.Kind {
	case PredicateEq:
		if predicate.Value == "" || predicate.Input != "" || len(predicate.Values) != 0 {
			return fmt.Errorf("%s eq requires only field and non-empty value", item)
		}
	case PredicateOneOf:
		if predicate.Value != "" || predicate.Input != "" || len(predicate.Values) < 2 {
			return fmt.Errorf("%s one_of requires only field and at least two values", item)
		}
		return validateStrings(predicate.Values, item+".values", true)
	case PredicatePresent, PredicatePositive:
		if predicate.Value != "" || predicate.Input != "" || len(predicate.Values) != 0 {
			return fmt.Errorf("%s %s requires only field", item, predicate.Kind)
		}
	}
	return nil
}

func validateRequestPredicateShape(predicate PredicateTemplate, item string, context predicateValidationContext) error {
	switch predicate.Kind {
	case PredicateEqualsInput:
		if context != predicateRequest {
			return fmt.Errorf("%s equals_input requires request context", item)
		}
		if !fieldPattern.MatchString(predicate.Input) || predicate.Value != "" || len(predicate.Values) != 0 {
			return fmt.Errorf("%s %s requires only field and dotted input", item, predicate.Kind)
		}
	case PredicateEqualsBinding:
		if context != predicateRequest || !fieldPattern.MatchString(predicate.Input) || predicate.Value != "" ||
			len(predicate.Values) != 0 {
			return fmt.Errorf("%s equals_binding requires only field and dotted request input", item)
		}
	}
	return nil
}

func predicateResultKey(field string) string {
	return "field-" + strings.NewReplacer(".", "-", "_", "-").Replace(field)
}

func validateStrings(values []string, where string, requireTwo bool) error {
	if requireTwo && len(values) < 2 {
		return fmt.Errorf("%s must contain at least two values", where)
	}
	seen := make(map[string]bool, len(values))
	for index, value := range values {
		if value == "" || len(value) > maxCatalogText {
			return fmt.Errorf("%s[%d] must contain 1..%d bytes", where, index, maxCatalogText)
		}
		if seen[value] {
			return fmt.Errorf("%s contains duplicate %q", where, value)
		}
		seen[value] = true
	}
	return nil
}
