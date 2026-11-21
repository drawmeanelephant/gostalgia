package vfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// AppDataPrefix is the root path for application private storage.
const AppDataPrefix = "/apps/data"

// AccessMode specifies the allowed filesystem operation type on a path.
type AccessMode string

const (
	AccessRead      AccessMode = "read"
	AccessReadWrite AccessMode = "read-write"
)

// NormalizeAccessMode converts string variations to standard AccessMode.
func NormalizeAccessMode(s string) (AccessMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read", "ro", "r":
		return AccessRead, nil
	case "read-write", "readwrite", "rw", "w", "write":
		return AccessReadWrite, nil
	default:
		return "", fmt.Errorf("vfs: invalid access mode %q (must be 'read' or 'read-write')", s)
	}
}

// AppDataDir returns the canonical private storage directory for an app.
func AppDataDir(appID string) string {
	return AppDataPrefix + "/" + appID
}

// ParseAppPrivatePath checks if envPath falls inside an app-private storage partition.
// Returns true and the owning appID if it is, or false and empty string otherwise.
func ParseAppPrivatePath(envPath string) (isPrivate bool, ownerApp string) {
	norm, err := Normalize(envPath)
	if err != nil {
		return false, ""
	}
	if norm == "apps/data" || !strings.HasPrefix(norm, "apps/data/") {
		return false, ""
	}
	sub := strings.TrimPrefix(norm, "apps/data/")
	parts := strings.Split(sub, "/")
	if len(parts) == 0 || parts[0] == "" {
		return false, ""
	}
	return true, parts[0]
}

// IsAppPrivatePath reports whether envPath is within appID's private storage partition.
func IsAppPrivatePath(appID, envPath string) bool {
	norm, err := Normalize(envPath)
	if err != nil {
		return false
	}
	appNorm := "apps/data/" + appID
	return norm == appNorm || strings.HasPrefix(norm, appNorm+"/")
}

// IsProfileTrashPath reports whether envPath is inside a profile trash tree
// (/users/<profile_id>/.trash or beneath it). Trash internals are
// system-managed: no grant may cover them, so applications cannot author or
// tamper with trash metadata or payloads through the ordinary filesystem.
func IsProfileTrashPath(envPath string) bool {
	norm, err := Normalize(envPath)
	if err != nil {
		return false
	}
	parts := strings.Split(norm, "/")
	return len(parts) >= 3 && parts[0] == "users" && parts[2] == ".trash"
}

// Grant represents a scoped authorization for an application to access a path.
type Grant struct {
	ID        string     `json:"id"`
	AppID     string     `json:"app_id"`
	Path      string     `json:"path"`      // environment path, e.g. "/users/guest/documents/notes.txt"
	Access    AccessMode `json:"access"`    // "read" or "read-write"
	Recursive bool       `json:"recursive"` // whether descendants are included
	// SessionBound grants live only for the application's current run —
	// document handoffs use them — and the runtime revokes them when the
	// app exits. Standing grants (install-time path grants, operator
	// fs/grant) survive restarts.
	SessionBound bool      `json:"session_bound,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Revoked      bool      `json:"revoked"`
	RevokedAt    time.Time `json:"revoked_at,omitempty"`
}

// GrantStore manages scoped VFS grants, validation, and revocation.
// Safe for concurrent use.
type GrantStore struct {
	mu     sync.RWMutex
	grants map[string]*Grant
	byApp  map[string][]string // appID -> []grantID
}

// NewGrantStore creates an empty grant store.
func NewGrantStore() *GrantStore {
	return &GrantStore{
		grants: make(map[string]*Grant),
		byApp:  make(map[string][]string),
	}
}

// Issue generates and records a new path-scoped grant for an application.
func (s *GrantStore) Issue(appID, envPath string, access AccessMode, recursive bool) (*Grant, error) {
	return s.issue(appID, envPath, access, recursive, false)
}

// IssueSession is Issue for grants bound to the application's current
// run — a document handoff confers access only while the receiving app
// runs. The runtime revokes session-bound grants when the app exits.
func (s *GrantStore) IssueSession(appID, envPath string, access AccessMode, recursive bool) (*Grant, error) {
	return s.issue(appID, envPath, access, recursive, true)
}

func (s *GrantStore) issue(appID, envPath string, access AccessMode, recursive, sessionBound bool) (*Grant, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, errors.New("vfs: app ID is required")
	}
	norm, err := Normalize(envPath)
	if err != nil {
		return nil, fmt.Errorf("vfs: invalid grant path: %w", err)
	}
	if norm == "." {
		return nil, errors.New("vfs: cannot grant root")
	}
	cleanPath := "/" + norm

	// Cross-app isolation: cannot grant access to another app's private storage
	if isPriv, owner := ParseAppPrivatePath(cleanPath); isPriv && owner != appID {
		return nil, fmt.Errorf("vfs: cannot grant access to another application's private storage (%s)", owner)
	}
	if norm == "apps/data" {
		return nil, errors.New("vfs: cannot grant access to app private storage root")
	}
	if IsProfileTrashPath(cleanPath) {
		return nil, errors.New("vfs: cannot grant access to profile trash")
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("vfs: generate grant id: %w", err)
	}
	id := "grant-" + hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	g := &Grant{
		ID:           id,
		AppID:        appID,
		Path:         cleanPath,
		Access:       access,
		Recursive:    recursive,
		SessionBound: sessionBound,
		CreatedAt:    time.Now(),
	}
	s.grants[id] = g
	s.byApp[appID] = append(s.byApp[appID], id)
	return &Grant{
		ID:           g.ID,
		AppID:        g.AppID,
		Path:         g.Path,
		Access:       g.Access,
		Recursive:    g.Recursive,
		SessionBound: g.SessionBound,
		CreatedAt:    g.CreatedAt,
	}, nil
}

// Revoke immediately invalidates a grant by its ID.
func (s *GrantStore) Revoke(grantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[grantID]
	if !ok {
		return fmt.Errorf("vfs: grant %q not found", grantID)
	}
	if !g.Revoked {
		g.Revoked = true
		g.RevokedAt = time.Now()
	}
	return nil
}

// RevokeApp invalidates all grants associated with an application ID.
func (s *GrantStore) RevokeApp(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, id := range s.byApp[appID] {
		if g, ok := s.grants[id]; ok && !g.Revoked {
			g.Revoked = true
			g.RevokedAt = now
		}
	}
}

// RevokeAppSession invalidates an application's session-bound grants —
// those issued for a single run, like document handoffs — when that run
// ends. Standing grants are untouched: install-time path grants and
// operator fs/grant authorizations survive app restarts.
func (s *GrantStore) RevokeAppSession(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, id := range s.byApp[appID] {
		if g, ok := s.grants[id]; ok && g.SessionBound && !g.Revoked {
			g.Revoked = true
			g.RevokedAt = now
		}
	}
}

// Get returns a copy of a grant by its ID.
func (s *GrantStore) Get(grantID string) (Grant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.grants[grantID]
	if !ok {
		return Grant{}, false
	}
	return *g, true
}

// List returns all grants matching appID (or all grants if appID is empty).
func (s *GrantStore) List(appID string) []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Grant
	for _, g := range s.grants {
		if appID == "" || g.AppID == appID {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// CheckAccess evaluates whether appID is authorized to perform mode on envPath.
// If access is permitted, it returns nil.
// If access is denied, it returns a structured *Error with code ErrPermission or ErrReadOnly.
func (s *GrantStore) CheckAccess(appID string, envPath string, mode AccessMode) error {
	if strings.TrimSpace(appID) == "" {
		return &Error{Op: "access", Path: envPath, Code: ErrPermission, Message: "permission denied: app ID is required"}
	}
	normPath, err := Normalize(envPath)
	if err != nil {
		return &Error{Op: "access", Path: envPath, Code: ErrInvalid, Message: fmt.Sprintf("invalid path: %v", err)}
	}

	// 1. Check if the path is in app-private storage (/apps/data/...)
	isPrivate, owner := ParseAppPrivatePath(envPath)
	if isPrivate {
		if owner == appID {
			// Accessing own private storage partition: full read/write authorized
			return nil
		}
		// Cross-app private storage access attempt
		return &Error{
			Op:      "access",
			Path:    envPath,
			Code:    ErrPermission,
			Message: fmt.Sprintf("permission denied: cross-app access to private storage of %q is forbidden", owner),
		}
	}

	// Deny enumeration or access to the app-private storage root itself
	if normPath == "apps/data" {
		return &Error{
			Op:      "access",
			Path:    envPath,
			Code:    ErrPermission,
			Message: "permission denied: cannot access app private storage root",
		}
	}

	// Profile trash trees are system-managed and ungrantable: even a grant
	// covering the profile's home directory must not reach them.
	if IsProfileTrashPath(envPath) {
		return &Error{
			Op:      "access",
			Path:    envPath,
			Code:    ErrPermission,
			Message: "permission denied: cannot access profile trash",
		}
	}

	// 2. Check path-scoped grants
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchedRevoked bool
	var matchedReadOnly bool

	for _, g := range s.grants {
		if g.AppID != appID {
			continue
		}
		grantNorm, err := Normalize(g.Path)
		if err != nil {
			continue
		}

		// Exact match or recursive descendant match
		matches := (normPath == grantNorm)
		if !matches && g.Recursive {
			matches = strings.HasPrefix(normPath, grantNorm+"/")
		}

		if matches {
			if g.Revoked {
				matchedRevoked = true
				continue
			}

			// Active grant matched: check access mode
			if mode == AccessRead {
				return nil // both read and read-write grants permit read
			}
			if mode == AccessReadWrite {
				if g.Access == AccessReadWrite {
					return nil
				}
				matchedReadOnly = true
			}
		}
	}

	if matchedRevoked {
		return &Error{
			Op:      "access",
			Path:    envPath,
			Code:    ErrPermission,
			Message: "permission denied: grant revoked",
		}
	}
	if matchedReadOnly {
		return &Error{
			Op:      "access",
			Path:    envPath,
			Code:    ErrReadOnly,
			Message: fmt.Sprintf("permission denied: path %q granted read-only, write access requires read-write grant", envPath),
		}
	}

	return &Error{
		Op:      "access",
		Path:    envPath,
		Code:    ErrPermission,
		Message: fmt.Sprintf("permission denied: path %q is outside app-private storage and has no grant for app %q", envPath, appID),
	}
}

// FindMatchingGrant returns the active grant authorizing access to envPath for appID, if any.
func (s *GrantStore) FindMatchingGrant(appID, envPath string) (*Grant, bool) {
	normPath, err := Normalize(envPath)
	if err != nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.grants {
		if g.AppID != appID || g.Revoked {
			continue
		}
		grantNorm, err := Normalize(g.Path)
		if err != nil {
			continue
		}
		if normPath == grantNorm || (g.Recursive && strings.HasPrefix(normPath, grantNorm+"/")) {
			copy := *g
			return &copy, true
		}
	}
	return nil, false
}
