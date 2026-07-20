package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	// DefaultReportDeadline bounds one report-work verification pass.
	DefaultReportDeadline = 10 * time.Second
	// MaxProviderRuntime is the absolute ceiling regardless of caller deadline.
	MaxProviderRuntime = 30 * time.Second
	// maxReferencesPerRole caps how many IDs are checked per role. The cap is
	// never silent: overflow emits an explicit coverage result.
	maxReferencesPerRole = 8
)

// ReportReference is one session-owned role/ID pair. Type is resolved from
// the stage tables, never supplied by the agent.
type ReportReference struct {
	Role         string
	Type         ResourceType
	ID           string
	ReportedNode int
}

// ReportRequest contains the full bounded context for automatic report-work
// verification. CompletedAt is the prospective report time: verification runs
// before the node transition.
type ReportRequest struct {
	SessionID       string
	BlueprintID     string
	BlueprintDigest string
	NodeID          string
	NodeNumber      int
	StartedAt       *time.Time
	CompletedAt     *time.Time
	References      []ReportReference
	Deadline        time.Time
}

// ReportVerifier performs one process-local, read-only Stripe pass. It owns
// no global credentials and never persists API payloads.
type ReportVerifier struct {
	reader  Reader
	account AccountContext
}

// NewReportVerifier explicitly injects the reader and account context. A nil
// reader is retained as an unavailable read condition (fails open at the
// workflow gate) rather than a construction failure.
func NewReportVerifier(reader Reader, account AccountContext) *ReportVerifier {
	return &ReportVerifier{reader: reader, account: account}
}

// Verify runs the stage's checks over the newly reported and retained
// references. Failed results are deterministic contradictions that gate
// report-work; unavailable results fail open.
func (verifier *ReportVerifier) Verify(ctx context.Context, request ReportRequest) (verification.ResultSet, error) {
	if ctx == nil {
		return verification.ResultSet{}, errors.New("report verification context is required")
	}
	expectedDigest, supported := frozenBlueprintDigests[request.BlueprintID]
	if !supported {
		return verification.NewResultSet(), nil
	}
	roles, stageKnown := stageRoles[request.BlueprintID][request.NodeID]
	if !stageKnown {
		return verification.NewResultSet(), nil
	}
	if request.BlueprintDigest == "" || request.BlueprintDigest != expectedDigest {
		return verification.NewResultSet(verification.Result{
			ID: "overlay-binding", Status: verification.StatusUnavailable,
			Detail: "Stripe resource verification is unavailable because the session blueprint digest does not match",
		}), nil
	}
	if request.Deadline.IsZero() {
		return verification.ResultSet{}, errors.New("report verification deadline is required")
	}
	deadline := request.Deadline
	if maximum := time.Now().Add(MaxProviderRuntime); deadline.After(maximum) {
		deadline = maximum
	}
	runContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	c := &stageCtx{
		ctx:     runContext,
		request: request,
		roles:   roles,
		refs:    map[string][]ReportReference{},
	}
	if verifier == nil || verifier.reader == nil || verifier.account.Mode != ModeTest {
		for _, role := range roles {
			c.unavailableResult(existsID(role.Role, ""), "Stripe authentication or the resource reader is unavailable; "+role.Role+" was not verified")
		}
		return verification.NewResultSet(c.results...), nil
	}
	c.reader = verifier.reader

	c.groupReferences()
	c.reportMissingRoles()
	if check := stageChecks[request.BlueprintID][request.NodeID]; check != nil {
		check(c)
	}

	sort.Slice(c.results, func(left, right int) bool { return c.results[left].ID < c.results[right].ID })
	results := capResults(c.results)
	return verification.NewResultSet(results...), nil
}

// capResults bounds the result count. Failed results are never dropped by
// the cap: the truncated set must reach the workflow gate with every
// contradiction intact, and the cap itself is announced with a marker.
func capResults(results []verification.Result) []verification.Result {
	if len(results) <= verification.MaxResultsPerNode {
		return results
	}
	kept := make([]verification.Result, 0, verification.MaxResultsPerNode-1)
	var open []verification.Result
	for _, result := range results {
		if result.Status == verification.StatusFailed {
			kept = append(kept, result)
		} else {
			open = append(open, result)
		}
	}
	if len(kept) > verification.MaxResultsPerNode-1 {
		kept = kept[:verification.MaxResultsPerNode-1]
	}
	for _, result := range open {
		if len(kept) >= verification.MaxResultsPerNode-1 {
			break
		}
		kept = append(kept, result)
	}
	dropped := len(results) - len(kept)
	kept = append(kept, verification.Result{
		ID: "coverage:truncated", Status: verification.StatusUnavailable,
		Detail: fmt.Sprintf("%d verification results were dropped by the per-node result cap; treat coverage as incomplete", dropped),
	})
	sort.Slice(kept, func(left, right int) bool { return kept[left].ID < kept[right].ID })
	return kept
}

// groupReferences filters the session references down to this stage's roles,
// validates them, dedupes, sorts, and applies the per-role cap with explicit
// overflow markers.
func (c *stageCtx) groupReferences() {
	types := make(map[string]ResourceType, len(c.roles))
	for _, role := range c.roles {
		types[role.Role] = role.Type
	}
	seen := map[string]struct{}{}
	for _, reference := range c.request.References {
		expectedType, relevant := types[reference.Role]
		if !relevant || expectedType != reference.Type || validateResourceRef(ResourceRef{Type: reference.Type, ID: reference.ID}) != nil {
			continue
		}
		key := reference.Role + "\x00" + reference.ID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		c.refs[reference.Role] = append(c.refs[reference.Role], reference)
	}
	overflowRoles := make([]string, 0, len(c.refs))
	for role := range c.refs {
		sort.Slice(c.refs[role], func(left, right int) bool { return c.refs[role][left].ID < c.refs[role][right].ID })
		if len(c.refs[role]) > maxReferencesPerRole {
			overflowRoles = append(overflowRoles, role)
		}
	}
	sort.Strings(overflowRoles)
	for _, role := range overflowRoles {
		total := len(c.refs[role])
		c.refs[role] = c.refs[role][:maxReferencesPerRole]
		c.unavailableResult("coverage:"+roleToken(role)+"-overflow",
			fmt.Sprintf("%d Stripe resource IDs were reported for role %s; only the %d lowest-sorted IDs were checked", total, role, maxReferencesPerRole))
	}
}

// reportMissingRoles emits one result per declared role with no reference:
// blocking for required roles, an explicit verification gap for best-effort
// v2 roles.
func (c *stageCtx) reportMissingRoles() {
	for _, role := range c.roles {
		if len(c.refs[role.Role]) > 0 {
			continue
		}
		if BestEffortResourceType(role.Type) {
			c.unavailableResult(existsID(role.Role, ""),
				"no Stripe resource ID was reported for role "+role.Role+"; this portion of the blueprint is unavailable, not verified")
			continue
		}
		c.failedResult(existsID(role.Role, ""), "no Stripe resource ID was reported for role "+role.Role)
	}
}

func fingerprint(values ...string) string {
	joined := ""
	for index, value := range values {
		if index > 0 {
			joined += "\x00"
		}
		joined += value
	}
	digest := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(digest[:6])
}
