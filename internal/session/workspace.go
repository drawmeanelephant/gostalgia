package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"gostalgia/internal/vfs"
)

// DefaultWorkspacePath is the standard VFS location for user workspace state.
const DefaultWorkspacePath = "/users/guest/config/workspace.json"

// MaxHistoryEntries bounds the retained shell command history.
const MaxHistoryEntries = 500

// WorkspaceState holds the persistent state for a user's workspace/session.
// Note: Boot tokens, session tokens, and credentials MUST NEVER be stored here.
type WorkspaceState struct {
	Version         int       `json:"version"`
	SessionID       string    `json:"session_id,omitempty"`
	CurrentDir      string    `json:"cwd"`
	ActiveView      string    `json:"active_view"`
	History         []string  `json:"history"`
	HistoryDisabled bool      `json:"history_disabled,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

var (
	hexTokenPattern     = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	sensitiveArgPattern = regexp.MustCompile(`(?i)(--(?:token|secret|password|api-?key)|(?:token|secret|password|bearer|api_?key)[=:])\s*([^\s]+)`)
)

// IsSensitiveCommand returns true if line contains sensitive arguments (tokens, passwords, secrets)
// or is an auth/token command that should not be persisted in command history.
func IsSensitiveCommand(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	cmd := strings.ToLower(fields[0])
	if cmd == "auth" || cmd == "token" || cmd == "login" {
		return true
	}
	if hexTokenPattern.MatchString(trimmed) {
		return true
	}
	if sensitiveArgPattern.MatchString(trimmed) {
		return true
	}
	return false
}

// SanitizeHistoryEntry cleans a command line for history persistence.
// If the command is entirely sensitive (e.g. auth login, token generate), ok is false.
// If it contains sensitive arguments, they are redacted.
func SanitizeHistoryEntry(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	if cmd == "auth" || cmd == "token" || cmd == "login" {
		return "", false
	}
	// Redact long hex tokens
	cleaned := hexTokenPattern.ReplaceAllString(trimmed, "[REDACTED]")
	// Redact sensitive flags/args
	cleaned = sensitiveArgPattern.ReplaceAllString(cleaned, "$1 [REDACTED]")
	return cleaned, true
}

// WorkspaceStore manages atomic persistence and retrieval of workspace state.
type WorkspaceStore struct {
	mu         sync.RWMutex
	fsys       vfs.FS
	path       string
	maxHistory int
	state      WorkspaceState
}

// NewWorkspaceStore creates a WorkspaceStore backed by fsys at the given path.
// If path is empty, DefaultWorkspacePath is used.
func NewWorkspaceStore(fsys vfs.FS, storePath string) *WorkspaceStore {
	if storePath == "" {
		storePath = DefaultWorkspacePath
	}
	return &WorkspaceStore{
		fsys:       fsys,
		path:       storePath,
		maxHistory: MaxHistoryEntries,
		state: WorkspaceState{
			Version:    1,
			CurrentDir: "/users/guest",
			ActiveView: "prompt",
			History:    make([]string, 0),
			UpdatedAt:  time.Now(),
		},
	}
}

// setFS swaps the backing filesystem under the store lock so a late
// Manager.SetVFS cannot race an in-flight Load or Save.
func (s *WorkspaceStore) setFS(fsys vfs.FS) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fsys = fsys
}

func fsPath(p string) string {
	if norm, err := vfs.Normalize(p); err == nil {
		return norm
	}
	return strings.TrimPrefix(p, "/")
}

// Load reads workspace state from the VFS. If the file is missing, default state is kept.
// If the file is corrupted, a fallback default state is returned without failing fatal operations.
func (s *WorkspaceStore) Load() (*WorkspaceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fsys == nil {
		copy := s.state
		return &copy, nil
	}

	target := fsPath(s.path)
	data, err := s.fsys.ReadFile(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			copy := s.state
			return &copy, nil
		}
		return nil, fmt.Errorf("session: read workspace state: %w", err)
	}

	var loaded WorkspaceState
	if err := json.Unmarshal(data, &loaded); err != nil {
		// Stale or corrupted partial state: reset safely to defaults to guarantee recoverability.
		s.state.UpdatedAt = time.Now()
		copy := s.state
		return &copy, fmt.Errorf("session: corrupt workspace state at %s, reset to defaults: %w", s.path, err)
	}

	// Sanitize loaded history entries defensively
	sanitized := make([]string, 0, len(loaded.History))
	for _, h := range loaded.History {
		if clean, ok := SanitizeHistoryEntry(h); ok {
			sanitized = append(sanitized, clean)
		}
	}
	loaded.History = sanitized
	if len(loaded.History) > s.maxHistory {
		loaded.History = loaded.History[len(loaded.History)-s.maxHistory:]
	}
	if loaded.CurrentDir == "" {
		loaded.CurrentDir = "/users/guest"
	}
	if loaded.ActiveView == "" {
		loaded.ActiveView = "prompt"
	}
	loaded.Version = 1

	s.state = loaded
	copy := s.state
	return &copy, nil
}

// Save commits the state to the VFS using atomic write semantics.
func (s *WorkspaceStore) Save(state WorkspaceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure no sensitive entries
	sanitized := make([]string, 0, len(state.History))
	for _, h := range state.History {
		if clean, ok := SanitizeHistoryEntry(h); ok {
			sanitized = append(sanitized, clean)
		}
	}
	state.History = sanitized
	if len(state.History) > s.maxHistory {
		state.History = state.History[len(state.History)-s.maxHistory:]
	}
	state.Version = 1
	state.UpdatedAt = time.Now()
	s.state = state

	return s.saveLocked()
}

func (s *WorkspaceStore) saveLocked() error {
	if s.fsys == nil {
		return nil
	}

	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal workspace state: %w", err)
	}

	target := fsPath(s.path)
	dir := path.Dir(target)
	if err := s.fsys.MkdirAll(dir); err != nil {
		return fmt.Errorf("session: mkdir workspace state dir %s: %w", dir, err)
	}

	if err := s.fsys.SaveAtomic(target, data, 0o644); err != nil {
		return fmt.Errorf("session: save workspace state: %w", err)
	}
	return nil
}

// Get returns the current workspace state.
func (s *WorkspaceStore) Get() WorkspaceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copy := s.state
	if s.state.History == nil {
		copy.History = make([]string, 0)
	} else {
		copy.History = append([]string(nil), s.state.History...)
	}
	return copy
}

// SetCurrentDir updates the working directory and saves.
func (s *WorkspaceStore) SetCurrentDir(cwd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.CurrentDir = cwd
	s.state.UpdatedAt = time.Now()
	return s.saveLocked()
}

// SetActiveView updates the active view name and saves.
func (s *WorkspaceStore) SetActiveView(view string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.ActiveView = view
	s.state.UpdatedAt = time.Now()
	return s.saveLocked()
}

// AppendHistory appends a command to history if history is enabled and the command
// is not sensitive or duplicate of the last entry.
func (s *WorkspaceStore) AppendHistory(cmd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.HistoryDisabled {
		return nil
	}

	clean, ok := SanitizeHistoryEntry(cmd)
	if !ok || clean == "" {
		return nil
	}

	// Avoid duplicate consecutive entries
	n := len(s.state.History)
	if n > 0 && s.state.History[n-1] == clean {
		return nil
	}

	s.state.History = append(s.state.History, clean)
	if len(s.state.History) > s.maxHistory {
		s.state.History = s.state.History[len(s.state.History)-s.maxHistory:]
	}
	s.state.UpdatedAt = time.Now()
	return s.saveLocked()
}

// ClearHistory removes all command history and persists the change.
func (s *WorkspaceStore) ClearHistory() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.History = make([]string, 0)
	s.state.UpdatedAt = time.Now()
	return s.saveLocked()
}

// SetHistoryDisabled toggles history recording.
func (s *WorkspaceStore) SetHistoryDisabled(disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.HistoryDisabled = disabled
	s.state.UpdatedAt = time.Now()
	return s.saveLocked()
}
