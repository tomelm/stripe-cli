// Package runtime runs Co-op verification providers for one declared session.
package runtime

import (
	"context"
	"fmt"
	"sort"

	"github.com/stripe/stripe-cli/pkg/coop"
	core "github.com/stripe/stripe-cli/pkg/coop/verification"
)

// Session is the bounded session metadata passed to providers.
type Session struct {
	ID        string
	Blueprint string
	Nodes     []Node
}

// Node is the provider-facing metadata for one canonical session node.
type Node struct {
	Number   int
	Step     int
	Key      string
	Type     coop.NodeType
	Requests []Request
	Events   []string
}

// Request is canonical request metadata from a session node. ParamKeys are
// the top-level parameter names the blueprint declares for the request —
// names only, never example values.
type Request struct {
	Method    string
	Path      string
	ParamKeys []string
}

// Emit writes one result for a 1-based session node.
type Emit func(nodeNumber int, result core.Result) error

// Provider produces verification results until it finishes or ctx is canceled.
// Implementations must return promptly after cancellation and own terminal
// detection: they return once the session leaves the active state.
type Provider interface {
	Run(ctx context.Context, session Session, emit Emit) error
}

// Store is the small session-store surface required by Runner.
type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
}

// Runner owns one provider for the lifetime of one session.
type Runner struct {
	store     Store
	provider  Provider
	sanitizer core.Sanitizer
}

// Option configures process-local Runner behavior.
type Option func(*Runner)

// WithCredentials adds credential values that must be removed before results
// reach durable session state. The values remain in this process only.
func WithCredentials(credentials ...string) Option {
	copyOfCredentials := append([]string(nil), credentials...)
	return func(runner *Runner) {
		runner.sanitizer = core.NewSanitizer(copyOfCredentials...)
	}
}

// New constructs a Runner for one provider without starting it.
func New(store Store, provider Provider, options ...Option) *Runner {
	runner := &Runner{
		store:     store,
		provider:  provider,
		sanitizer: core.NewSanitizer(),
	}
	for _, option := range options {
		option(runner)
	}
	return runner
}

// Run opens sessionID, starts the provider, and waits until it finishes or
// the caller cancels. The provider owns terminal detection (it polls the
// session as part of its work); provider errors are advisory and do not
// change the session lifecycle.
func (runner *Runner) Run(ctx context.Context, sessionID string) error {
	if ctx == nil {
		return fmt.Errorf("verification context is required")
	}
	if runner.store == nil {
		return fmt.Errorf("verification store is required")
	}
	session, err := runner.store.Read(sessionID)
	if err != nil {
		return err
	}
	if session.Status != coop.SessionActive || runner.provider == nil {
		return nil
	}

	providerSession := SessionMetadata(session)
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	emit := func(nodeNumber int, result core.Result) error {
		_, err := runner.store.Update(sessionID, func(current *coop.Session) error {
			node, err := current.NodeByNumber(nodeNumber)
			if err != nil {
				return err
			}
			return core.UpsertResult(&node.VerificationResults, result, runner.sanitizer)
		})
		return err
	}
	go func() {
		defer close(done)
		_ = runner.provider.Run(runContext, cloneSession(providerSession), emit)
	}()

	select {
	case <-ctx.Done():
		cancel()
		<-done
		return nil
	case <-done:
		return nil
	}
}

func cloneSession(session Session) Session {
	cloned := session
	cloned.Nodes = append([]Node(nil), session.Nodes...)
	for index := range cloned.Nodes {
		cloned.Nodes[index].Requests = append([]Request(nil), session.Nodes[index].Requests...)
		for requestIndex := range cloned.Nodes[index].Requests {
			cloned.Nodes[index].Requests[requestIndex].ParamKeys = append([]string(nil), cloned.Nodes[index].Requests[requestIndex].ParamKeys...)
		}
		cloned.Nodes[index].Events = append([]string(nil), session.Nodes[index].Events...)
	}
	return cloned
}

// paramKeys extracts the sorted top-level parameter names from a blueprint
// request's params value. Values are never retained.
func paramKeys(params interface{}) []string {
	object, ok := params.(map[string]interface{})
	if !ok || len(object) == 0 {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// SessionMetadata returns the bounded canonical metadata visible to providers.
func SessionMetadata(session *coop.Session) Session {
	metadata := Session{
		ID:        session.ID,
		Blueprint: session.Blueprint,
		Nodes:     make([]Node, 0, session.TotalNodes()),
	}
	nodeNumber := 0
	for stepIndex, step := range session.Steps {
		for _, node := range step.Nodes {
			nodeNumber++
			entry := Node{
				Number: nodeNumber,
				Step:   stepIndex,
				Key:    node.Key,
				Type:   node.Type,
				Events: append([]string(nil), node.Events...),
			}
			if node.Request != nil {
				entry.Requests = append(entry.Requests, Request{Method: node.Request.Method, Path: node.Request.Path, ParamKeys: paramKeys(node.Request.Params)})
			}
			for _, request := range node.TestRequests {
				entry.Requests = append(entry.Requests, Request{Method: request.Method, Path: request.Path, ParamKeys: paramKeys(request.Params)})
			}
			metadata.Nodes = append(metadata.Nodes, entry)
		}
	}
	return metadata
}
