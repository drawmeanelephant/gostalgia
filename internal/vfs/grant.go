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

// revokedGrantLimit bounds the retained set of revoked grant tombstones kept
// for diagnostics — the same reaping applied to TokenStore: revocation moves
// the record out of the live set so CheckAccess, FindMatchingGrant, and
// byApp never grow with usage, while a bounded tombstone still lets Get and
// List report the revocation.
const revokedGrantLimit = 256

// GrantStore manages scoped VFS grants, validation, and revocation.
// Safe for concurrent use.
//
// grants and byApp hold only live, unrevoked grants. Revoked grants are
// reaped into a bounded tombstone set so per-check scans stay proportional
// to live authority rather than total history.
type GrantStore struct {
	mu           sync.RWMutex
	grants       map[string]*Grant   // live grants
	byApp        map[string][]string // appID -> live grant IDs
	revoked      map[string]*Grant   // bounded tombstones, grantID -> grant
	revokedOrder []string            // tombstone IDs, oldest first
}

// NewGrantStore creates an empty grant store.
func NewGrantStore() *GrantStore {
	return &GrantStore{
		grants:  make(map[string]*Grant),
		byApp:   make(map[string][]string),
		revoked: make(map[string]*Grant),
	}
}

// Issue generates and records a new path-scoped grant for an application.
func (s *GrantStore) Issue(appID, envPath string, access AccessMode, recursive bool) (*Grant, error) {
	g, _, err := s.issue(appID, envPath, access, recursive, false)
	return g, err
}

// IssueSession is Issue for grants bound to the application's current
// run — a document handoff confers access only while the receiving app
// runs. The runtime revokes session-bound grants when the app exits.
// Re-issuing the same (app, path, mode, scope) reuses the live grant
// instead of accumulating a duplicate per handoff.
func (s *GrantStore) IssueSession(appID, envPath string, access AccessMode, recursive bool) (*Grant, error) {
	g, _, err := s.issue(appID, envPath, access, recursive, true)
	return g, err
}

// IssueSessionOnce is IssueSession but reports whether the returned grant
// was newly minted. Re-opening a document reuses the live session grant for
// the same (app, path, mode, scope); the caller needs to know which so that
// a failed handoff revokes only authority it created and never pulls a
// shared grant out from under a still-open document.
func (s *GrantStore) IssueSessionOnce(appID, envPath string, access AccessMode, recursive bool) (*Grant, bool, error) {
	return s.issue(appID, envPath, access, recursive, true)
}

func (s *GrantStore) issue(appID, envPath string, access AccessMode, recursive, sessionBound bool) (*Grant, bool, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, false, errors.New("vfs: app ID is required")
	}
	norm, err := Normalize(envPath)
	if err != nil {
		return nil, false, fmt.Errorf("vfs: invalid grant path: %w", err)
	}
	if norm == "." {
		return nil, false, errors.New("vfs: cannot grant root")
	}
	cleanPath := "/" + norm

	// Cross-app isolation: cannot grant access to another app's private storage
	if isPriv, owner := ParseAppPrivatePath(cleanPath); isPriv && owner != appID {
		return nil, false, fmt.Errorf("vfs: cannot grant access to another application's private storage (%s)", owner)
	}
	if norm == "apps/data" {
		return nil, false, errors.New("vfs: cannot grant access to app private storage root")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-opening a document reuses the live session grant for the same
	// (app, path, mode, scope) instead of minting a duplicate per handoff.
	// Standing grants are never deduplicated: an explicit fs/grant issue is
	// an operator's deliberate act.
	if sessionBound {
		for _, id := range s.byApp[appID] {
			if g, ok := s.grants[id]; ok && g.SessionBound &&
				g.Path == cleanPath && g.Access == access && g.Recursive == recursive {
				return cloneGrant(g), false, nil
			}
		}
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, false, fmt.Errorf("vfs: generate grant id: %w", err)
	}
	id := "grant-" + hex.EncodeToString(b)

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
	return cloneGrant(g), true, nil
}

func cloneGrant(g *Grant) *Grant {
	copy := *g
	return &copy
}

// unindexLocked drops id from appID's live-grant index, removing empty
// entries. Callers must hold s.mu.
func (s *GrantStore) unindexLocked(appID, id string) {
	ids := s.byApp[appID]
	for i, gid := range ids {
		if gid == id {
			s.byApp[appID] = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(s.byApp[appID]) == 0 {
		delete(s.byApp, appID)
	}
}

// reapLocked marks the grant revoked, removes it from the live set and
// index, and retains a bounded tombstone so Get/List/CheckAccess can still
// report the revocation. Callers must hold s.mu.
func (s *GrantStore) reapLocked(id string, now time.Time) {
	g, ok := s.grants[id]
	if !ok {
		return
	}
	g.Revoked = true
	g.RevokedAt = now
	delete(s.grants, id)
	s.unindexLocked(g.AppID, id)
	if _, ok := s.revoked[id]; !ok {
		s.revokedOrder = append(s.revokedOrder, id)
	}
	s.revoked[id] = g
	for len(s.revokedOrder) > revokedGrantLimit {
		delete(s.revoked, s.revokedOrder[0])
		s.revokedOrder = s.revokedOrder[1:]
	}
}

// Revoke immediately invalidates a grant by its ID.
func (s *GrantStore) Revoke(grantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.revoked[grantID]; ok {
		return nil // already revoked: idempotent
	}
	if _, ok := s.grants[grantID]; !ok {
		return fmt.Errorf("vfs: grant %q not found", grantID)
	}
	s.reapLocked(grantID, time.Now())
	return nil
}

// RevokeApp invalidates all grants associated with an application ID.
func (s *GrantStore) RevokeApp(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, id := range append([]string(nil), s.byApp[appID]...) {
		s.reapLocked(id, now)
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
	for _, id := range append([]string(nil), s.byApp[appID]...) {
		if g, ok := s.grants[id]; ok && g.SessionBound {
			s.reapLocked(id, now)
		}
	}
}

// Get returns a copy of a grant by its ID, including recently revoked
// grants retained in the bounded tombstone set.
func (s *GrantStore) Get(grantID string) (Grant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g, ok := s.grants[grantID]; ok {
		return *g, true
	}
	if g, ok := s.revoked[grantID]; ok {
		return *g, true
	}
	return Grant{}, false
}

// List returns all grants matching appID (or all grants if appID is empty),
// including recently revoked grants retained in the bounded tombstone set.
func (s *GrantStore) List(appID string) []Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Grant
	for _, g := range s.grants {
		if appID == "" || g.AppID == appID {
			out = append(out, *g)
		}
	}
	for _, g := range s.revoked {
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

	// 2. Check path-scoped grants. The byApp index confines the scan to the
	// caller's live grants; revoked tombstones are consulted only to report
	// "grant revoked" rather than "no grant".
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matchedRevoked bool
	var matchedReadOnly bool

	for _, id := range s.byApp[appID] {
		g, ok := s.grants[id]
		if !ok {
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

	for _, g := range s.revoked {
		if g.AppID != appID {
			continue
		}
		grantNorm, err := Normalize(g.Path)
		if err != nil {
			continue
		}
		if normPath == grantNorm || (g.Recursive && strings.HasPrefix(normPath, grantNorm+"/")) {
			matchedRevoked = true
			break
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
	for _, id := range s.byApp[appID] {
		g, ok := s.grants[id]
		if !ok {
			continue
		}
		grantNorm, err := Normalize(g.Path)
		if err != nil {
			continue
		}
		if normPath == grantNorm || (g.Recursive && strings.HasPrefix(normPath, grantNorm+"/")) {
			return cloneGrant(g), true
		}
	}
	return nil, false
}

