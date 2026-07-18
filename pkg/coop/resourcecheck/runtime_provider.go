package resourcecheck

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

const defaultRuntimeProviderDeadline = 10 * time.Second

// RuntimeProviderConfig binds one process-local reader and explicit caller
// inputs to the shared Co-op verification runtime.
type RuntimeProviderConfig struct {
	Reader       Reader
	Account      AccountContext
	References   map[string]ResourceReference
	ActionWindow *CreationWindow
	Values       map[string]JSONScalar
	Deadline     time.Duration
}

// RuntimeProvider adapts the bounded resource provider to the shared Co-op
// provider lifecycle.
type RuntimeProvider struct {
	config RuntimeProviderConfig
}

// NewRuntimeProvider copies process-local provider configuration. The reader
// itself remains injected and owned by the caller.
func NewRuntimeProvider(config RuntimeProviderConfig) *RuntimeProvider {
	cloned := config
	cloned.References = cloneResourceReferences(config.References)
	cloned.Values = cloneProviderValues(config.Values)
	if config.ActionWindow != nil {
		window := *config.ActionWindow
		cloned.ActionWindow = &window
	}
	if cloned.Deadline <= 0 || cloned.Deadline > MaxProviderRuntime {
		cloned.Deadline = defaultRuntimeProviderDeadline
	}
	return &RuntimeProvider{config: cloned}
}

// Run verifies a supported frozen blueprint once and emits every result onto
// the corresponding canonical session node. Unsupported blueprints are inert.
func (provider *RuntimeProvider) Run(ctx context.Context, session verificationruntime.Session, emit verificationruntime.Emit) error {
	if provider == nil {
		return fmt.Errorf("resource runtime provider is required")
	}
	declaration, ok := DeclarationForBlueprint(session.Blueprint)
	if !ok {
		return nil
	}
	set, err := Verify(ctx, ProviderRequest{
		Declaration:  declaration,
		Scope:        VerificationScope{SessionID: session.ID, BlueprintDigest: declaration.BlueprintDigest},
		References:   cloneResourceReferences(provider.config.References),
		ActionWindow: provider.config.ActionWindow,
		Values:       cloneProviderValues(provider.config.Values),
		Account:      provider.config.Account,
		Deadline:     time.Now().Add(provider.config.Deadline),
		Reader:       provider.config.Reader,
	})
	if err != nil {
		return err
	}
	resultNodes := declarationResultNodes(declaration)
	for _, result := range set.Results {
		nodeID, exists := resultNodes[result.ID]
		if !exists {
			return fmt.Errorf("resource result %q has no declared node", result.ID)
		}
		nodeNumber, exists := runtimeNodeNumber(session, nodeID)
		if !exists {
			return fmt.Errorf("resource result %q targets unavailable node %q", result.ID, nodeID)
		}
		if err := emit(nodeNumber, result); err != nil {
			return err
		}
	}
	return nil
}

func declarationResultNodes(declaration BlueprintDeclaration) map[verification.ResultID]string {
	nodes := make(map[verification.ResultID]string, declarationResultCount(declaration))
	resources := make(map[string]ResourceDeclaration, len(declaration.Resources))
	for _, resource := range declaration.Resources {
		resources[resource.Key] = resource
		nodes[resource.ExistenceResultID] = resource.NodeID
		if resource.ApplicationCorrelationResultID != "" {
			nodes[resource.ApplicationCorrelationResultID] = resource.NodeID
		}
	}
	for _, field := range declaration.Fields {
		nodes[field.ResultID] = resources[field.Resource].NodeID
	}
	for _, link := range declaration.Links {
		nodes[link.ResultID] = resources[link.Source].NodeID
	}
	for _, entitlement := range declaration.ActiveEntitlements {
		nodes[entitlement.ResultID] = entitlement.NodeID
	}
	for _, usage := range declaration.MeterUsages {
		nodes[usage.ResultID] = usage.NodeID
	}
	return nodes
}

func runtimeNodeNumber(session verificationruntime.Session, nodeID string) (int, bool) {
	nodeKey := nodeID
	if separator := strings.LastIndexByte(nodeID, '.'); separator >= 0 {
		nodeKey = nodeID[separator+1:]
	}
	for _, node := range session.Nodes {
		if strings.EqualFold(node.Key, nodeKey) {
			return node.Number, true
		}
	}
	return 0, false
}

func cloneResourceReferences(references map[string]ResourceReference) map[string]ResourceReference {
	if references == nil {
		return nil
	}
	cloned := make(map[string]ResourceReference, len(references))
	for key, reference := range references {
		cloned[key] = reference
	}
	return cloned
}

func cloneProviderValues(values map[string]JSONScalar) map[string]JSONScalar {
	if values == nil {
		return nil
	}
	cloned := make(map[string]JSONScalar, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

var _ verificationruntime.Provider = (*RuntimeProvider)(nil)
