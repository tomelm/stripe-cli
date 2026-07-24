package coop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Agent process presence is launcher metadata, so it lives in a small sidecar
// rather than becoming blueprint work or agent-authored session evidence.
type AgentProcessPhase string

const (
	AgentProcessLaunched AgentProcessPhase = "launched"
	AgentProcessRunning  AgentProcessPhase = "running"
	AgentProcessStopped  AgentProcessPhase = "stopped"

	maxAgentProcessLifecycleBytes = 4 << 10
)

var (
	ErrAgentProcessAlreadyActive = errors.New("a launched agent process is already active")
	ErrAgentProcessSuperseded    = errors.New("agent process launch was superseded")
)

type AgentProcessLifecycle struct {
	LaunchID   string            `json:"launch_id"`
	Phase      AgentProcessPhase `json:"phase"`
	LaunchedAt time.Time         `json:"launched_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ExitStatus *int              `json:"exit_status,omitempty"`
}

func (s *Store) agentProcessLifecyclePath(id string) (string, error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return "", err
	}
	return path + ".agent-process", nil
}

// AgentProcessLifecycle returns nil when this session was not launched by
// `stripe coop start`; callers must present that as unknown, not stopped.
func (s *Store) AgentProcessLifecycle(id string) (*AgentProcessLifecycle, error) {
	path, err := s.agentProcessLifecyclePath(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading agent process lifecycle: %w", err)
	}
	if info.Size() > maxAgentProcessLifecycleBytes {
		return nil, errors.New("agent process lifecycle is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading agent process lifecycle: %w", err)
	}
	if len(data) > maxAgentProcessLifecycleBytes {
		return nil, errors.New("agent process lifecycle is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lifecycle AgentProcessLifecycle
	if err := decoder.Decode(&lifecycle); err != nil {
		return nil, fmt.Errorf("parsing agent process lifecycle: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("parsing agent process lifecycle: trailing JSON value")
	}
	if err := validateAgentProcessLifecycle(&lifecycle); err != nil {
		return nil, fmt.Errorf("invalid agent process lifecycle: %w", err)
	}
	return &lifecycle, nil
}

func (s *Store) StartAgentProcess(id, launchID string) error {
	if err := validateAgentProcessLaunchID(launchID); err != nil {
		return err
	}
	return s.updateAgentProcessLifecycle(id, func(current *AgentProcessLifecycle, now time.Time) (*AgentProcessLifecycle, error) {
		if current != nil && current.Phase != AgentProcessStopped {
			pulseAge, err := s.AgentProcessPulseAge(id)
			if err != nil {
				return nil, err
			}
			recentLaunch := current.Phase == AgentProcessLaunched && now.Sub(current.UpdatedAt) < AgentProcessPulseFreshFor
			livePulse := pulseAge >= 0 && pulseAge < AgentProcessPulseFreshFor
			if recentLaunch || livePulse {
				return nil, fmt.Errorf("%w: %s", ErrAgentProcessAlreadyActive, id)
			}
		}
		return &AgentProcessLifecycle{
			LaunchID: launchID, Phase: AgentProcessLaunched,
			LaunchedAt: now, UpdatedAt: now,
		}, nil
	})
}

// MarkAgentProcessRunning is called only after the renewable process pulse
// acquires its lease. The TUI requires both this state and a fresh pulse.
func (s *Store) MarkAgentProcessRunning(id, launchID string) error {
	if err := validateAgentProcessLaunchID(launchID); err != nil {
		return err
	}
	return s.updateAgentProcessLifecycle(id, func(current *AgentProcessLifecycle, now time.Time) (*AgentProcessLifecycle, error) {
		if current == nil || current.LaunchID != launchID || current.Phase == AgentProcessStopped {
			return nil, fmt.Errorf("%w: %s", ErrAgentProcessSuperseded, id)
		}
		current.Phase = AgentProcessRunning
		current.UpdatedAt = now
		return current, nil
	})
}

// StopAgentProcess is idempotent for one launch so pulse cleanup cannot replace
// a useful launcher exit status with an unknown one.
func (s *Store) StopAgentProcess(id, launchID string, exitStatus *int) error {
	if err := validateAgentProcessLaunchID(launchID); err != nil {
		return err
	}
	if exitStatus != nil && (*exitStatus < 0 || *exitStatus > 255) {
		return errors.New("agent process exit status must be between 0 and 255")
	}
	return s.updateAgentProcessLifecycle(id, func(current *AgentProcessLifecycle, now time.Time) (*AgentProcessLifecycle, error) {
		if current == nil || current.LaunchID != launchID {
			return nil, fmt.Errorf("%w: %s", ErrAgentProcessSuperseded, id)
		}
		if current.Phase != AgentProcessStopped {
			current.Phase = AgentProcessStopped
			current.UpdatedAt = now
			current.ExitStatus = exitStatus
		}
		return current, nil
	})
}

func (s *Store) updateAgentProcessLifecycle(
	id string,
	update func(*AgentProcessLifecycle, time.Time) (*AgentProcessLifecycle, error),
) error {
	sessionPath, err := s.sessionPath(id)
	if err != nil {
		return err
	}
	path := sessionPath + ".agent-process"
	unlock, err := s.acquireSessionLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(sessionPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %q", ErrSessionNotFound, id)
		}
		return err
	}
	current, err := s.AgentProcessLifecycle(id)
	if err != nil {
		return err
	}
	next, err := update(current, time.Now().UTC())
	if err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing agent process lifecycle: %w", err)
	}
	if err := replaceSessionFile(tmp, path); err != nil {
		return err
	}
	syncDir(s.baseDir)
	return nil
}

func validateAgentProcessLaunchID(id string) error {
	if id == "" || len(id) > 128 || strings.TrimSpace(id) != id {
		return errors.New("a valid agent process launch ID is required")
	}
	for _, character := range id {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", character) {
			return errors.New("a valid agent process launch ID is required")
		}
	}
	return nil
}

func validateAgentProcessLifecycle(lifecycle *AgentProcessLifecycle) error {
	if lifecycle == nil || lifecycle.LaunchedAt.IsZero() || lifecycle.UpdatedAt.IsZero() {
		return errors.New("lifecycle timestamps are required")
	}
	if err := validateAgentProcessLaunchID(lifecycle.LaunchID); err != nil {
		return err
	}
	switch lifecycle.Phase {
	case AgentProcessLaunched, AgentProcessRunning:
		if lifecycle.ExitStatus != nil {
			return errors.New("nonterminal lifecycle contains an exit status")
		}
	case AgentProcessStopped:
		if lifecycle.ExitStatus != nil && (*lifecycle.ExitStatus < 0 || *lifecycle.ExitStatus > 255) {
			return errors.New("exit status is outside 0-255")
		}
	default:
		return fmt.Errorf("unknown phase %q", lifecycle.Phase)
	}
	return nil
}
