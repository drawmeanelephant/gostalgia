// Package security defines the environment's identity and capability
// primitives: users and permission tokens checked at IPC handler
// boundaries. It is deliberately tiny. It provides logical isolation
// only; see docs/security.md for exactly what is and is not protected.
package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"gostalgia/sdk"
)

// User is an environment-level identity. Users exist independently of the
// host OS's user accounts.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PrincipalKind distinguishes operator authority from application authority.
type PrincipalKind string

const (
	PrincipalKindOperator PrincipalKind = "operator"
	PrincipalKindApp      PrincipalKind = "app"
)

// Principal represents the authenticated caller identity.
type Principal struct {
	Kind      PrincipalKind `json:"kind"`
	AppID     string        `json:"app_id,omitempty"`
	ProcessID int32         `json:"process_id,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
	User      User          `json:"user,omitempty"`
}

func (p Principal) IsOperator() bool { return p.Kind == PrincipalKindOperator }
func (p Principal) IsApp() bool      { return p.Kind == PrincipalKindApp }

// OperatorPrincipal creates an operator principal.
func OperatorPrincipal(user User) Principal {
	return Principal{
		Kind: PrincipalKindOperator,
		User: user,
	}
}

// AppPrincipal creates a launch-bound application principal.
func AppPrincipal(appID string, procID int32, sessionID string, user User) Principal {
	return Principal{
		Kind:      PrincipalKindApp,
		AppID:     appID,
		ProcessID: procID,
		SessionID: sessionID,
		User:      user,
	}
}

// Credential holds an active or revoked credential and its bound identity.
type Credential struct {
	Token        string        `json:"token"`
	Principal    Principal     `json:"principal"`
	Capabilities *Capabilities `json:"capabilities"`
	CreatedAt    time.Time     `json:"created_at"`
	Revoked      bool          `json:"revoked"`
	RevokedAt    time.Time     `json:"revoked_at,omitempty"`
}

// revokedCredentialLimit bounds the retained set of revoked credentials kept
// for diagnostics (Lookup and revoked-vs-unknown error reporting). It follows
// the 256-record bound used for the IPC audit history (events.HistoryLimit).
const revokedCredentialLimit = 256

// TokenStore issues, validates, and revokes credentials for operators and
// applications. It enforces server-side identity validation, binding tokens to
// specific approved principals and grants rather than trusting client claims.
// Safe for concurrent use.
type TokenStore struct {
	mu           sync.RWMutex
	credentials  map[string]*Credential         // live credentials only, token -> cred
	byApp        map[string]map[string]struct{} // appID -> live token set
	byProc       map[int32]map[string]struct{}  // procID -> live token set
	revoked      map[string]*Credential         // bounded tombstones, token -> cred
	revokedOrder []string                       // tombstone tokens, oldest first
}

func NewTokenStore() *TokenStore {
	return &TokenStore{
		credentials: make(map[string]*Credential),
		byApp:       make(map[string]map[string]struct{}),
		byProc:      make(map[int32]map[string]struct{}),
		revoked:     make(map[string]*Credential),
	}
}

// RegisterOperator registers a static operator token with admin capabilities.
func (s *TokenStore) RegisterOperator(token string, user User) error {
	if token == "" {
		return errors.New("security: operator token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.credentials[token]; exists {
		return errors.New("security: token already registered")
	}
	s.credentials[token] = &Credential{
		Token:        token,
		Principal:    OperatorPrincipal(user),
		Capabilities: AdminCapabilities(),
		CreatedAt:    time.Now(),
	}
	return nil
}

// SetOperatorUser updates the user for all operator credentials.
func (s *TokenStore) SetOperatorUser(user User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cred := range s.credentials {
		if cred.Principal.IsOperator() {
			cred.Principal.User = user
		}
	}
}

// IssueAppToken generates a launch-bound credential for an application with
// its approved capability grants.
func (s *TokenStore) IssueAppToken(appID string, procID int32, sessionID string, user User, caps ...string) (string, error) {
	if appID == "" {
		return "", errors.New("security: app ID is required")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("security: generate app token: %w", err)
	}
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	cred := &Credential{
		Token:        token,
		Principal:    AppPrincipal(appID, procID, sessionID, user),
		Capabilities: NewCapabilities(caps...),
		CreatedAt:    time.Now(),
	}
	s.credentials[token] = cred
	indexToken(s.byApp, appID, token)
	if procID != 0 {
		indexToken(s.byProc, procID, token)
	}
	return token, nil
}

// BindProcess associates an issued app token with a process ID once launched.
func (s *TokenStore) BindProcess(token string, procID int32) {
	if procID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cred, ok := s.credentials[token]; ok {
		if old := cred.Principal.ProcessID; old != 0 && old != procID {
			unindexToken(s.byProc, old, token)
		}
		cred.Principal.ProcessID = procID
		indexToken(s.byProc, procID, token)
	}
}

// indexToken records token under key in an index map.
func indexToken[K comparable](index map[K]map[string]struct{}, key K, token string) {
	set := index[key]
	if set == nil {
		set = make(map[string]struct{})
		index[key] = set
	}
	set[token] = struct{}{}
}

// unindexToken drops token from key's set, removing empty sets.
func unindexToken[K comparable](index map[K]map[string]struct{}, key K, token string) {
	set := index[key]
	if set == nil {
		return
	}
	delete(set, token)
	if len(set) == 0 {
		delete(index, key)
	}
}

// reapLocked marks the credential revoked, removes it from the live set and
// indexes, and retains a bounded tombstone so diagnostics can still report
// recent revocations. Callers must hold s.mu.
func (s *TokenStore) reapLocked(token string, cred *Credential, now time.Time) {
	cred.Revoked = true
	cred.RevokedAt = now
	delete(s.credentials, token)
	unindexToken(s.byApp, cred.Principal.AppID, token)
	if cred.Principal.ProcessID != 0 {
		unindexToken(s.byProc, cred.Principal.ProcessID, token)
	}
	if _, ok := s.revoked[token]; !ok {
		s.revokedOrder = append(s.revokedOrder, token)
	}
	s.revoked[token] = cred
	for len(s.revokedOrder) > revokedCredentialLimit {
		delete(s.revoked, s.revokedOrder[0])
		s.revokedOrder = s.revokedOrder[1:]
	}
}

// lookupLocked resolves a token against live credentials, then the bounded
// tombstone set. Callers must hold s.mu.
func (s *TokenStore) lookupLocked(token string) (*Credential, bool) {
	if cred, ok := s.credentials[token]; ok &&
		subtle.ConstantTimeCompare([]byte(cred.Token), []byte(token)) == 1 {
		return cred, true
	}
	if cred, ok := s.revoked[token]; ok &&
		subtle.ConstantTimeCompare([]byte(cred.Token), []byte(token)) == 1 {
		return cred, true
	}
	return nil, false
}

// Authenticate verifies the token on connection handshake.
func (s *TokenStore) Authenticate(token string) (Principal, *Capabilities, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cred, ok := s.lookupLocked(token)
	if !ok {
		return Principal{}, nil, errors.New("unauthorized: bad token")
	}
	if cred.Revoked {
		return Principal{}, nil, errors.New("unauthorized: token revoked")
	}
	return cred.Principal, NewCapabilities(cred.Capabilities.List()...), nil
}

// Validate verifies whether an already-authenticated token remains active and unrevoked.
func (s *TokenStore) Validate(token string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cred, ok := s.lookupLocked(token)
	if !ok {
		return errors.New("unauthorized: bad token")
	}
	if cred.Revoked {
		return errors.New("unauthorized: credential revoked")
	}
	return nil
}

// Revoke invalidates a specific token immediately and reaps it from the live set.
func (s *TokenStore) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cred, ok := s.credentials[token]; ok {
		s.reapLocked(token, cred, time.Now())
	}
}

// RevokeApp invalidates all tokens issued for an application.
func (s *TokenStore) RevokeApp(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for tok := range s.byApp[appID] {
		if cred, ok := s.credentials[tok]; ok {
			s.reapLocked(tok, cred, now)
		}
	}
}

// RevokeProcess invalidates all tokens associated with a process ID.
func (s *TokenStore) RevokeProcess(procID int32) {
	if procID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for tok := range s.byProc[procID] {
		if cred, ok := s.credentials[tok]; ok {
			s.reapLocked(tok, cred, now)
		}
	}
}

// Lookup returns a copy of the credential metadata for diagnostics, including
// recently revoked credentials retained in the bounded tombstone set.
func (s *TokenStore) Lookup(token string) (Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cred, ok := s.lookupLocked(token)
	if !ok {
		return Credential{}, false
	}
	return Credential{
		Token:        cred.Token,
		Principal:    cred.Principal,
		Capabilities: NewCapabilities(cred.Capabilities.List()...),
		CreatedAt:    cred.CreatedAt,
		Revoked:      cred.Revoked,
		RevokedAt:    cred.RevokedAt,
	}, true
}

// Well-known capabilities. Applications declare the ones they need in
// their manifest; the runtime grants them to the application's call
// context, and system services check them before acting.
const (
	CapIPC            = sdk.CapIPC
	CapFileRead       = sdk.CapFileRead
	CapFileWrite      = sdk.CapFileWrite
	CapProcList       = sdk.CapProcList
	CapProcStop       = sdk.CapProcStop
	CapAppList        = sdk.CapAppList
	CapAppLaunch      = sdk.CapAppLaunch
	CapShutdown       = sdk.CapShutdown
	CapConfigRead     = sdk.CapConfigRead
	CapConfigWrite    = sdk.CapConfigWrite
	CapClipboardRead  = sdk.CapClipboardRead
	CapClipboardWrite = sdk.CapClipboardWrite
	CapHostFSRead     = sdk.CapHostFSRead
	CapHostFSWrite    = sdk.CapHostFSWrite
	CapNetEgress      = sdk.CapNetEgress
	CapSessionRead    = sdk.CapSessionRead
	CapSessionWrite   = sdk.CapSessionWrite
	CapProfileRead    = sdk.CapProfileRead
	CapProfileWrite   = sdk.CapProfileWrite
	CapPackageRead    = sdk.CapPackageRead
	CapPackageWrite   = sdk.CapPackageWrite
	CapBackupRead     = sdk.CapBackupRead
	CapBackupWrite    = sdk.CapBackupWrite
	CapNotify         = sdk.CapNotify
	CapSound          = sdk.CapSound
	CapAdmin          = "admin"
)

// Capabilities is a concurrency-safe permission set.
type Capabilities struct {
	mu   sync.RWMutex
	caps map[string]bool
}

func NewCapabilities(capabilities ...string) *Capabilities {
	c := &Capabilities{caps: make(map[string]bool, len(capabilities))}
	for _, name := range capabilities {
		c.caps[name] = true
	}
	return c
}

func (c *Capabilities) Has(capability string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps[capability]
}

func (c *Capabilities) Grant(capabilities ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range capabilities {
		c.caps[name] = true
	}
}

// List returns the granted capabilities, sorted.
func (c *Capabilities) List() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.caps))
	for name := range c.caps {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AdminCapabilities returns the full capability set granted to trusted
// local control clients (gctl) and to the runtime's own in-process
// calls. Applications never receive this set; they get what their
// manifest declares.
func AdminCapabilities() *Capabilities {
	return NewCapabilities(
		CapIPC, CapFileRead, CapFileWrite,
		CapProcList, CapProcStop,
		CapAppList, CapAppLaunch,
		CapShutdown,
		CapConfigRead, CapConfigWrite,
		CapClipboardRead, CapClipboardWrite,
		CapHostFSRead, CapHostFSWrite,
		CapNetEgress,
		CapSessionRead, CapSessionWrite,
		CapProfileRead, CapProfileWrite,
		CapPackageRead, CapPackageWrite,
		CapBackupRead, CapBackupWrite,
		CapSound,
		CapAdmin,
	)
}
