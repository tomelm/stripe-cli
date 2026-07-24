package coop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Store manages session persistence with atomic writes.
type Store struct {
	baseDir string
}

const MaxSessionFileBytes = 4 << 20

var (
	ErrInvalidSessionID      = errors.New("invalid session id")
	ErrSessionNotFound       = errors.New("session not found")
	ErrVersionConflict       = errors.New("version conflict")
	ErrLockTimeout           = errors.New("timed out waiting for session lock")
	ErrCorruptSession        = errors.New("corrupt session")
	ErrObserverLeaseHeld     = errors.New("session observer lease is already held")
	ErrAgentProcessPulseHeld = errors.New("session agent process pulse is already held")
)

var (
	sessionLockTimeout      = 5 * time.Second
	sessionLockPollInterval = 25 * time.Millisecond
	sessionLockMaxWait      = 30 * time.Second
	// sessionLockStale bounds how long a lock file may sit untouched before it's
	// treated as abandoned by a crashed writer and reclaimed. Healthy writers hold
	// the lock only for the duration of a single write (well under a second), so a
	// much larger threshold reliably distinguishes a crash from an active writer.
	sessionLockStale = 30 * time.Second
	// observerLeaseHeartbeatInterval is capped below relative to
	// sessionLockStale so platforms without process-liveness support never
	// reclaim a healthy, long-lived observer.
	observerLeaseHeartbeatInterval   = 10 * time.Second
	agentPulseLeaseHeartbeatInterval = 2 * time.Second
)

// AgentProcessPulseFreshFor is the bounded window in which a TUI may present
// the launcher-owned process pulse as fresh. It is deliberately distinct from
// await-review's heartbeat, which means only that a review waiter is running.
const AgentProcessPulseFreshFor = 10 * time.Second

// NewStore creates a Store, ensuring the coop directory exists.
func NewStore(configFolder string) (*Store, error) {
	dir := filepath.Join(configFolder, "coop")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating coop directory: %w", err)
	}
	return &Store{baseDir: dir}, nil
}

// NewStoreAt creates a Store at a specific path (for testing).
func NewStoreAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating coop directory: %w", err)
	}
	return &Store{baseDir: dir}, nil
}

func (s *Store) sessionPath(id string) (string, error) {
	// Validate ID to prevent path traversal
	base := filepath.Base(id)
	if base != id || id == "" || id == "." || id == ".." {
		return "", fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	return filepath.Join(s.baseDir, id+".json"), nil
}

// Write atomically persists a session (write to .tmp then rename).
// Uses optimistic locking: checks that the file's current version matches
// the session's version before writing. Returns an error on conflict.
func (s *Store) Write(session *Session) error {
	path, err := s.sessionPath(session.ID)
	if err != nil {
		return err
	}
	return s.writePath(path, session)
}

func (s *Store) writePath(path string, session *Session) error {
	unlock, err := s.acquireSessionLock(path)
	if err != nil {
		return err
	}
	defer unlock()

	if existing, err := readBoundedSessionFile(path); err == nil {
		var current Session
		if err := json.Unmarshal(existing, &current); err != nil {
			return fmt.Errorf("%w: parsing existing session %q: %v", ErrCorruptSession, session.ID, err)
		}
		if err := validateSessionIdentity(session.ID, &current); err != nil {
			return err
		}
		if current.Version != session.Version {
			return fmt.Errorf("%w: expected %d, file has %d", ErrVersionConflict, session.Version, current.Version)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading existing session %q: %w", session.ID, err)
	}

	next := *session
	next.UpdatedAt = time.Now().UTC()
	next.Version++
	if err := s.writeUnlocked(path, &next); err != nil {
		return err
	}
	*session = next
	return nil
}

func replaceSessionFile(tmpPath, path string) error {
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	// Windows does not replace an existing destination with os.Rename.
	// Writers are already serialized by the session lock, but readers may
	// briefly hold the existing file. Retry the remove/rename pair so polling
	// readers do not make writes spuriously fail.
	deadline := time.Now().Add(500 * time.Millisecond)
	var lastErr error
	for {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			lastErr = fmt.Errorf("removing existing session file: %w", err)
		} else if err := os.Rename(tmpPath, path); err != nil {
			lastErr = fmt.Errorf("renaming temp file: %w", err)
		} else {
			return nil
		}

		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) acquireSessionLock(path string) (func(), error) {
	lockPath := path + ".lock"
	releaseProcessLock, err := acquireProcessSessionLock(lockPath, sessionLockTimeout)
	if err != nil {
		return nil, err
	}
	processLockHeld := true
	defer func() {
		if processLockHeld {
			releaseProcessLock()
		}
	}()

	started := time.Now()
	wait := sessionLockWait{deadline: started.Add(sessionLockTimeout), limit: started.Add(sessionLockMaxWait)}

	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			// Record the owning pid and time so a lock abandoned by a crashed
			// writer can be recognized and reclaimed below.
			fmt.Fprintf(f, "%d\n%d\n", os.Getpid(), time.Now().UnixNano())
			f.Close()
			processLockHeld = false
			return func() {
				_ = os.Remove(lockPath)
				releaseProcessLock()
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating lock file: %w", err)
		}

		// Reclaim a lock only when its owner is gone. O_EXCL on the next iteration
		// ensures a single contender wins the recreate.
		if s.lockAbandoned(lockPath) {
			os.Remove(lockPath)
			continue
		}

		if wait.timedOut(time.Now(), sessionLockFingerprintFor(lockPath), sessionLockTimeout) {
			return nil, fmt.Errorf("%w: %s is still present; if no stripe coop command is running, remove this lock file and retry", ErrLockTimeout, lockPath)
		}
		time.Sleep(sessionLockPollInterval)
	}
}

type processSessionLock struct {
	ready      chan struct{}
	references int
}

var processSessionLocks struct {
	sync.Mutex
	locks map[string]*processSessionLock
}

// acquireProcessSessionLock gives callers in this process a path-specific turn
// before they contend on the cross-process file lock. Local queueing uses the
// short timeout: critical sections should be brief, and recursive acquisition
// must fail rather than accumulating behind the file lock's longer ceiling.
func acquireProcessSessionLock(lockPath string, timeout time.Duration) (func(), error) {
	key, err := filepath.Abs(lockPath)
	if err != nil {
		return nil, fmt.Errorf("resolving session lock path: %w", err)
	}

	processSessionLocks.Lock()
	if processSessionLocks.locks == nil {
		processSessionLocks.locks = make(map[string]*processSessionLock)
	}
	lock := processSessionLocks.locks[key]
	if lock == nil {
		lock = &processSessionLock{ready: make(chan struct{}, 1)}
		lock.ready <- struct{}{}
		processSessionLocks.locks[key] = lock
	}
	lock.references++
	processSessionLocks.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-lock.ready:
		var releaseOnce sync.Once
		return func() {
			releaseOnce.Do(func() {
				lock.ready <- struct{}{}
				releaseProcessSessionLock(key, lock)
			})
		}, nil
	case <-timer.C:
		releaseProcessSessionLock(key, lock)
		return nil, fmt.Errorf("%w: %s is still in use by this process", ErrLockTimeout, key)
	}
}

func releaseProcessSessionLock(key string, lock *processSessionLock) {
	processSessionLocks.Lock()
	defer processSessionLocks.Unlock()
	lock.references--
	if lock.references == 0 && processSessionLocks.locks[key] == lock {
		delete(processSessionLocks.locks, key)
	}
}

type sessionLockFingerprint struct {
	pid     int
	created int64
}

type sessionLockWait struct {
	deadline    time.Time
	limit       time.Time
	fingerprint sessionLockFingerprint
}

// timedOut treats a new owner/creation pair as progress, so sustained active
// handoffs may extend the inactivity deadline, but never the absolute limit.
func (wait *sessionLockWait) timedOut(now time.Time, fingerprint sessionLockFingerprint, timeout time.Duration) bool {
	if fingerprint != (sessionLockFingerprint{}) && fingerprint != wait.fingerprint {
		if wait.fingerprint != (sessionLockFingerprint{}) {
			wait.deadline = now.Add(timeout)
		}
		wait.fingerprint = fingerprint
	}
	return now.After(wait.deadline) || now.After(wait.limit)
}

func sessionLockFingerprintFor(lockPath string) sessionLockFingerprint {
	pid, created, ok := readLock(lockPath)
	if !ok || created.IsZero() {
		return sessionLockFingerprint{}
	}
	return sessionLockFingerprint{pid: pid, created: created.UnixNano()}
}

// lockAbandoned reports whether a lock file can be safely reclaimed. The lock
// records its owner's PID and creation time:
//   - If that process is alive the lock is left alone, even if old, so a slow
//     Store.Update callback can't have its lock deleted out from under it.
//   - Unless the process started *after* the lock was created: then the original
//     writer crashed and the PID was reused by an unrelated process, so the lock
//     is stale and reclaimed.
//   - A known-dead owner's lock is reclaimed immediately.
//   - When the owner can't be determined — an unparseable PID, or a platform
//     where liveness can't be checked (e.g. Windows) — fall back to reclaiming by
//     age so a crashed writer still can't wedge the session forever.
func (s *Store) lockAbandoned(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return false
	}
	if pid, created, ok := readLock(lockPath); ok {
		if alive, known := processAlive(pid); known {
			if !alive {
				return true
			}
			// PID reuse guard: if we know when both the lock and the process began
			// and the process started after the lock, it can't be the writer that
			// created the lock. Only reclaim when we're certain.
			if start, ok := processStartTime(pid); ok && !created.IsZero() && start.After(created) {
				return true
			}
			return false
		}
	}
	return time.Since(info.ModTime()) > sessionLockStale
}

// readLock parses the owning PID and creation time from a lock file. The lock
// format is two lines: the PID, then the creation time as Unix nanoseconds.
func readLock(lockPath string) (pid int, created time.Time, ok bool) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, time.Time{}, false
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 3)
	pid, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return 0, time.Time{}, false
	}
	if len(lines) >= 2 {
		if nanos, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err == nil {
			created = time.Unix(0, nanos)
		}
	}
	return pid, created, true
}

// processAlive reports whether the process with the given PID is running. The
// second return value is false when liveness can't be determined (e.g. Windows,
// where signal 0 is unsupported), so callers can fall back to another heuristic.
func processAlive(pid int) (alive bool, known bool) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, false
	}
	switch err := proc.Signal(syscall.Signal(0)); {
	case err == nil:
		return true, true // signal accepted → process exists
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false, true // no such process → dead
	case errors.Is(err, syscall.EPERM):
		return true, true // exists but not permitted to signal → alive
	default:
		return false, false // indeterminate (e.g. unsupported platform)
	}
}

// Read loads a session from disk.
func (s *Store) Read(id string) (*Session, error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return nil, err
	}
	data, err := readSessionFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("reading session %q: %w", id, err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("%w: parsing session %q: %v", ErrCorruptSession, id, err)
	}
	if err := validateSessionIdentity(id, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func readSessionFile(path string) ([]byte, error) {
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		data, err := readBoundedSessionFile(path)
		if err == nil || !isWindowsTransientReadError(err) || time.Now().After(deadline) {
			return data, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readBoundedSessionFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxSessionFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSessionFileBytes {
		return nil, fmt.Errorf("%w: session file exceeds %d bytes", ErrCorruptSession, MaxSessionFileBytes)
	}
	return data, nil
}

func isWindowsTransientReadError(err error) bool {
	return runtime.GOOS == "windows" && (os.IsNotExist(err) || strings.Contains(err.Error(), "being used by another process"))
}

// Update loads, mutates, and atomically writes a session with optimistic locking.
func (s *Store) Update(id string, fn func(*Session) error) (*Session, error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return nil, err
	}

	unlock, err := s.acquireSessionLock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	data, err := readBoundedSessionFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
		}
		return nil, fmt.Errorf("reading session %q: %w", id, err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("%w: parsing session %q: %v", ErrCorruptSession, id, err)
	}
	if err := validateSessionIdentity(id, &session); err != nil {
		return nil, err
	}
	if err := fn(&session); err != nil {
		return nil, err
	}
	if err := validateSessionIdentity(id, &session); err != nil {
		return nil, err
	}
	updatedData, err := json.MarshalIndent(&session, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling session: %w", err)
	}
	if bytes.Equal(data, updatedData) {
		return &session, nil
	}
	session.UpdatedAt = time.Now().UTC()
	session.Version++

	if err := s.writeUnlocked(path, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func validateSessionIdentity(requestedID string, session *Session) error {
	if session == nil || session.ID != requestedID {
		actualID := ""
		if session != nil {
			actualID = session.ID
		}
		return fmt.Errorf(
			"%w: session file %q contains identity %q",
			ErrCorruptSession,
			requestedID,
			actualID,
		)
	}
	return nil
}

// PinStripeAccount records the first usable Stripe account identity for an
// active session. Once present, the pin is immutable: later calls are
// idempotent even when they carry a different account. Callers can compare the
// returned session with their current identity and fail closed on a mismatch.
func (s *Store) PinStripeAccount(id, accountID string) (*Session, error) {
	accountID = strings.TrimSpace(accountID)
	if !validSessionStripeAccountID(accountID) {
		return nil, errors.New("a valid Stripe account ID is required")
	}
	return s.Update(id, func(session *Session) error {
		if session.StripeAccountID != "" {
			return nil
		}
		if session.Status != SessionActive {
			return fmt.Errorf("session %q is not active", id)
		}
		session.StripeAccountID = accountID
		return nil
	})
}

func validSessionStripeAccountID(accountID string) bool {
	if !strings.HasPrefix(accountID, "acct_") || len(accountID) < 6 || len(accountID) > 128 {
		return false
	}
	for _, character := range accountID {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func (s *Store) writeUnlocked(path string, session *Session) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}
	if len(data) > MaxSessionFileBytes {
		return fmt.Errorf("session exceeds %d bytes", MaxSessionFileBytes)
	}

	tmp, err := os.CreateTemp(s.baseDir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	// Flush file contents before the rename so a crash can't leave a renamed but
	// empty/truncated session file (the atomic rename guarantees name-swap
	// atomicity, not data durability).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := replaceSessionFile(tmpPath, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir best-effort fsyncs a directory so a completed rename survives a crash.
// Errors are ignored: directory fsync is not supported on all platforms and a
// failure here shouldn't fail an otherwise-successful write.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ModTime returns the file modification time (for polling).
func (s *Store) ModTime(id string) (time.Time, error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// List returns all session IDs found on disk.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	return ids, nil
}

type sessionEntry struct {
	id      string
	modTime time.Time
}

func (s *Store) sortedSessionEntries() ([]sessionEntry, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no coop sessions found")
	}

	var entries []sessionEntry
	for _, id := range ids {
		mt, err := s.ModTime(id)
		if err != nil {
			continue
		}
		entries = append(entries, sessionEntry{id: id, modTime: mt})
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no readable coop sessions found")
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.After(entries[j].modTime)
	})

	return entries, nil
}

// LatestSession returns the most recently updated readable session. Unreadable
// or corrupt session files are skipped so a single bad file (e.g. a truncated
// write) doesn't mask other valid sessions from "status"/"join".
func (s *Store) LatestSession() (*Session, error) {
	entries, err := s.sortedSessionEntries()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		session, err := s.Read(e.id)
		if err != nil {
			continue
		}
		return session, nil
	}
	return nil, fmt.Errorf("no readable coop sessions found")
}

// LatestActiveSession returns the most recently updated session with status "active".
func (s *Store) LatestActiveSession() (*Session, error) {
	entries, err := s.sortedSessionEntries()
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		session, err := s.Read(e.id)
		if err != nil {
			continue
		}
		if session.Status == SessionActive {
			return session, nil
		}
	}

	return nil, fmt.Errorf("no active coop sessions found")
}

// Delete removes a session file.
func (s *Store) Delete(id string) error {
	path, err := s.sessionPath(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// AcquireObserverLease gives one TUI process ownership of a session's passive
// streams. A lease abandoned by a dead process is reclaimed on the next join.
func (s *Store) AcquireObserverLease(id string) (func(), error) {
	return s.acquireRenewedLease(
		id,
		".observer",
		"observer",
		ErrObserverLeaseHeld,
		observerLeaseHeartbeatInterval,
	)
}

// AcquireAgentProcessPulse gives one launcher sidecar ownership of a session's
// agent-process pulse. The generated launcher holds this lease only while the
// foreground agent command is running. It never creates, updates, or removes
// await-review's separate heartbeat file.
func (s *Store) AcquireAgentProcessPulse(id string) (func(), error) {
	return s.acquireRenewedLease(
		id,
		".agent-pulse",
		"agent process pulse",
		ErrAgentProcessPulseHeld,
		agentPulseLeaseHeartbeatInterval,
	)
}

// AgentProcessPulseAge returns the age of the launcher-owned agent process
// pulse. It returns -1 when no pulse lease exists.
func (s *Store) AgentProcessPulseAge(id string) (time.Duration, error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path + ".agent-pulse")
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return 0, err
	}
	return time.Since(info.ModTime()), nil
}

func (s *Store) acquireRenewedLease(
	id string,
	suffix string,
	label string,
	heldErr error,
	renewEvery time.Duration,
) (func(), error) {
	path, err := s.sessionPath(id)
	if err != nil {
		return nil, err
	}
	leasePath := path + suffix
	unlock, err := s.acquireSessionLock(leasePath)
	if err != nil {
		return nil, fmt.Errorf("locking %s lease: %w", label, err)
	}
	if _, statErr := os.Stat(leasePath); statErr == nil {
		if !s.lockAbandoned(leasePath) {
			unlock()
			return nil, fmt.Errorf("%w: %s", heldErr, id)
		}
		if removeErr := os.Remove(leasePath); removeErr != nil && !os.IsNotExist(removeErr) {
			unlock()
			return nil, fmt.Errorf("reclaiming %s lease: %w", label, removeErr)
		}
	} else if !os.IsNotExist(statErr) {
		unlock()
		return nil, fmt.Errorf("reading %s lease: %w", label, statErr)
	}

	contents := fmt.Sprintf("%d\n%d\n", os.Getpid(), time.Now().UnixNano())
	file, err := os.OpenFile(leasePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("creating %s lease: %w", label, err)
	}
	if _, writeErr := file.WriteString(contents); writeErr != nil {
		_ = file.Close()
		_ = os.Remove(leasePath)
		unlock()
		return nil, fmt.Errorf("writing %s lease: %w", label, writeErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = os.Remove(leasePath)
		unlock()
		return nil, fmt.Errorf("closing %s lease: %w", label, closeErr)
	}
	unlock()

	stop := make(chan struct{})
	done := make(chan struct{})
	go s.renewLease(leasePath, contents, renewEvery, stop, done)
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			close(stop)
			<-done
			unlock, lockErr := s.acquireSessionLock(leasePath)
			if lockErr != nil {
				return
			}
			defer unlock()
			if current, readErr := os.ReadFile(leasePath); readErr == nil && string(current) == contents {
				_ = os.Remove(leasePath)
			}
		})
	}, nil
}

func (s *Store) renewLease(
	leasePath string,
	contents string,
	interval time.Duration,
	stop <-chan struct{},
	done chan<- struct{},
) {
	defer close(done)
	if sessionLockStale > 0 {
		maxInterval := sessionLockStale / 3
		if maxInterval > 0 && (interval <= 0 || interval > maxInterval) {
			interval = maxInterval
		}
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			unlock, err := s.acquireSessionLock(leasePath)
			if err != nil {
				return
			}
			current, readErr := os.ReadFile(leasePath)
			if readErr != nil || string(current) != contents {
				unlock()
				return
			}
			timestamp := now.UTC()
			if err := os.Chtimes(leasePath, timestamp, timestamp); err != nil {
				unlock()
				return
			}
			unlock()
		}
	}
}
