// Package runtime runs Co-op verification providers for one declared session.
package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	core "github.com/stripe/stripe-cli/pkg/coop/verification"
)

const defaultSessionPollInterval = 250 * time.Millisecond

// Session is the bounded session metadata passed to providers.
type Session struct {
	ID        string
	Blueprint string
	Nodes     []Node
}

// Node is the provider-facing metadata for one canonical session node.
type Node struct {
	Number int
	Key    string
	Type   coop.NodeType
	Method string
	Path   string
	Events []string
}

// Emit writes one result for a 1-based session node.
type Emit func(nodeNumber int, result core.Result) error

// Provider produces verification results until it finishes or ctx is canceled.
// Implementations must return promptly after cancellation.
type Provider interface {
	Run(ctx context.Context, session Session, emit Emit) error
}

// Store is the small session-store surface required by Runner.
type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
}

// Runner owns registered providers for the lifetime of one session.
type Runner struct {
	store        Store
	providers    []Provider
	sanitizer    core.Sanitizer
	pollInterval time.Duration
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

// WithPollInterval overrides session completion polling, primarily for tests.
func WithPollInterval(interval time.Duration) Option {
	return func(runner *Runner) {
		if interval > 0 {
			runner.pollInterval = interval
		}
	}
}

// New constructs a Runner without starting providers.
func New(store Store, options ...Option) *Runner {
	runner := &Runner{
		store:        store,
		sanitizer:    core.NewSanitizer(),
		pollInterval: defaultSessionPollInterval,
	}
	for _, option := range options {
		option(runner)
	}
	return runner
}

// Register adds a provider to this Runner before Run is called.
func (runner *Runner) Register(provider Provider) error {
	if provider == nil {
		return fmt.Errorf("verification provider is required")
	}
	runner.providers = append(runner.providers, provider)
	return nil
}

// Run opens sessionID, starts every registered provider, and waits until all
// providers finish, the caller cancels, or the session reaches a terminal state.
// Provider errors are advisory and do not change the session lifecycle.
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
	if session.Status != coop.SessionActive || len(runner.providers) == 0 {
		return nil
	}

	providerSession := sessionMetadata(session)
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()

	var providers sync.WaitGroup
	providers.Add(len(runner.providers))
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
	for _, registered := range runner.providers {
		provider := registered
		go func() {
			defer providers.Done()
			_ = provider.Run(runContext, cloneSession(providerSession), emit)
		}()
	}
	go func() {
		providers.Wait()
		close(done)
	}()

	ticker := time.NewTicker(runner.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return nil
		case <-done:
			return nil
		case <-ticker.C:
			current, err := runner.store.Read(sessionID)
			if err != nil {
				cancel()
				<-done
				return err
			}
			if current.Status != coop.SessionActive || current.IsComplete() {
				cancel()
				<-done
				return nil
			}
		}
	}
}

func cloneSession(session Session) Session {
	cloned := session
	cloned.Nodes = append([]Node(nil), session.Nodes...)
	for index := range cloned.Nodes {
		cloned.Nodes[index].Events = append([]string(nil), session.Nodes[index].Events...)
	}
	return cloned
}

func sessionMetadata(session *coop.Session) Session {
	metadata := Session{
		ID:        session.ID,
		Blueprint: session.Blueprint,
		Nodes:     make([]Node, 0, session.TotalNodes()),
	}
	nodeNumber := 0
	for _, step := range session.Steps {
		for _, node := range step.Nodes {
			nodeNumber++
			entry := Node{
				Number: nodeNumber,
				Key:    node.Key,
				Type:   node.Type,
				Events: append([]string(nil), node.Events...),
			}
			if node.Request != nil {
				entry.Method = node.Request.Method
				entry.Path = node.Request.Path
			}
			metadata.Nodes = append(metadata.Nodes, entry)
		}
	}
	return metadata
}
