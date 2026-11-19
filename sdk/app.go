// Package sdk is the standard-library-only contract for Gostalgia apps.
// See docs/applications.md for the normative authoring specification.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strings"
)

// Capability names understood by the runtime. There is no implicit grant.
const (
	CapIPC         = "ipc"
	CapFileRead    = "fs.read"
	CapFileWrite   = "fs.write"
	CapProcList    = "proc.list"
	CapProcStop    = "proc.stop"
	CapAppList     = "app.list"
	CapAppLaunch   = "app.launch"
	CapShutdown    = "shutdown"
	CapConfigRead  = "config.read"
	CapConfigWrite = "config.write"

	CapClipboardRead  = "clipboard.read"
	CapClipboardWrite = "clipboard.write"
	CapHostFSRead     = "hostfs.read"
	CapHostFSWrite    = "hostfs.write"
	CapNetEgress      = "net.egress"
	CapSessionRead    = "session.read"
	CapSessionWrite   = "session.write"
	CapProfileRead    = "profile.read"
	CapProfileWrite   = "profile.write"
	CapPackageRead    = "package.read"
	CapPackageWrite   = "package.write"
	CapBackupRead     = "backup.read"
	CapBackupWrite    = "backup.write"
	CapNotify         = "notify"
	CapSound          = "sound"

	ModeInProc   = "inproc"
	ModeExternal = "external"

	IsolationInProc  = "inproc"
	IsolationTrusted = "trusted"
	IsolationSandbox = "sandbox"
	IsolationStrict  = "strict"

	ProtocolVersion = 1
)

// Manifest describes an application. Permissions are the complete
// grant, not hints. App manifests cannot request the operator-only admin cap.
type Manifest struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Version         string      `json:"version"`
	Entrypoint      string      `json:"entrypoint,omitempty"`
	Mode            string      `json:"mode,omitempty"`
	Executable      string      `json:"executable,omitempty"`
	Args            []string    `json:"args,omitempty"`
	ProtocolVersion int         `json:"protocol_version,omitempty"`
	Permissions     []string    `json:"permissions,omitempty"`
	PathGrants      []PathGrant `json:"path_grants,omitempty"`
	Description     string      `json:"description,omitempty"`
	Icon            string      `json:"icon,omitempty"`
	Isolation       string      `json:"isolation,omitempty"`
	DocumentTypes   []string    `json:"document_types,omitempty"`
}

// MaxIconBytes bounds the optional icon art a manifest may carry. It is a
// storage limit for registration and transport only: the experience layer
// sanitizes the art and normalizes it to two display lines of at most twelve
// cells each. Manifests without an icon remain valid and usable.
const MaxIconBytes = 512

// PathGrant requests environment filesystem access, independently of capabilities.
type PathGrant struct {
	Path      string `json:"path"`
	Access    string `json:"access"`
	Recursive bool   `json:"recursive,omitempty"`
}

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z0-9-]+)+$`)
	entryPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// ValidAppID reports whether id is a canonical reverse-DNS application identity.
func ValidAppID(id string) bool { return idPattern.MatchString(id) }

// Validate rejects invalid identity, version, entrypoint, or permissions.
func (m Manifest) Validate() error {
	if !ValidAppID(m.ID) {
		return fmt.Errorf("app: manifest id %q is not a reverse-DNS name", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("app: manifest %s: name is required", m.ID)
	}
	if !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("app: manifest %s: version must be MAJOR.MINOR.PATCH", m.ID)
	}
	if m.ProtocolVersion < 0 || (m.ProtocolVersion > 0 && m.ProtocolVersion != ProtocolVersion) {
		return fmt.Errorf("app: manifest %s: unsupported protocol version %d", m.ID, m.ProtocolVersion)
	}
	switch m.Mode {
	case "", ModeInProc:
		if !entryPattern.MatchString(m.Entrypoint) {
			return fmt.Errorf("app: manifest %s: entrypoint must match %s", m.ID, entryPattern)
		}
		if m.Executable != "" {
			return fmt.Errorf("app: manifest %s: executable is not permitted for inproc mode", m.ID)
		}
	case ModeExternal:
		if strings.TrimSpace(m.Executable) == "" {
			return fmt.Errorf("app: manifest %s: executable is required for external mode", m.ID)
		}
		if m.Entrypoint != "" && !entryPattern.MatchString(m.Entrypoint) {
			return fmt.Errorf("app: manifest %s: entrypoint must match %s", m.ID, entryPattern)
		}
	default:
		return fmt.Errorf("app: manifest %s: unknown mode %q", m.ID, m.Mode)
	}
	if m.Isolation != "" {
		switch m.Isolation {
		case IsolationInProc, IsolationTrusted, IsolationSandbox, IsolationStrict:
		default:
			return fmt.Errorf("app: manifest %s: unknown isolation %q", m.ID, m.Isolation)
		}
	}
	if (m.Mode == "" || m.Mode == ModeInProc) && (m.Isolation == IsolationSandbox || m.Isolation == IsolationStrict) {
		return fmt.Errorf("app: manifest %s: inproc mode does not support %q isolation", m.ID, m.Isolation)
	}
	// Icon art is optional display metadata. Only its storage size is asserted
	// here; terminal sanitization and the two-line/twelve-cell display bounds
	// belong to the experience layer that renders it.
	if len(m.Icon) > MaxIconBytes {
		return fmt.Errorf("app: manifest %s: icon art must be at most %d bytes", m.ID, MaxIconBytes)
	}

	seen := make(map[string]bool)
	for _, cap := range m.Permissions {
		switch cap {
		case CapIPC, CapFileRead, CapFileWrite, CapProcList, CapProcStop, CapAppList, CapAppLaunch, CapShutdown,
			CapConfigRead, CapConfigWrite,
			CapClipboardRead, CapClipboardWrite, CapHostFSRead, CapHostFSWrite, CapNetEgress,
			CapSessionRead, CapSessionWrite,
			CapProfileRead, CapProfileWrite,
			CapPackageRead, CapPackageWrite,
			CapBackupRead, CapBackupWrite,
			CapNotify, CapSound:
		default:
			return fmt.Errorf("app: manifest %s: unknown or reserved permission %q", m.ID, cap)
		}
		if seen[cap] {
			return fmt.Errorf("app: manifest %s: duplicate permission %q", m.ID, cap)
		}
		seen[cap] = true
	}
	paths := make(map[string]bool)
	for _, g := range m.PathGrants {
		if !strings.HasPrefix(g.Path, "/") || g.Path == "/" || path.Clean(g.Path) != g.Path ||
			strings.ContainsAny(g.Path, "\\\x00:") {
			return fmt.Errorf("app: manifest %s: invalid grant path %q", m.ID, g.Path)
		}
		for _, part := range strings.Split(strings.TrimPrefix(g.Path, "/"), "/") {
			if strings.TrimRight(part, ". ") != part || strings.ContainsAny(part, "<>\"?*|") {
				return fmt.Errorf("app: manifest %s: nonportable grant path %q", m.ID, g.Path)
			}
		}
		// Installed code and other applications' private data are not grantable.
		folded := strings.ToLower(g.Path)
		if folded == "/apps" || strings.HasPrefix(folded, "/apps/") {
			own := "/apps/data/" + m.ID
			if g.Path != own && !strings.HasPrefix(g.Path, own+"/") {
				return fmt.Errorf("app: manifest %s: forbidden grant path %q", m.ID, g.Path)
			}
		}
		if g.Access != "read" && g.Access != "read-write" {
			return fmt.Errorf("app: manifest %s: invalid grant access %q", m.ID, g.Access)
		}
		if paths[g.Path] {
			return fmt.Errorf("app: manifest %s: duplicate grant path %q", m.ID, g.Path)
		}
		paths[g.Path] = true
	}
	return nil
}

// EffectiveIsolation returns the configured or default isolation level for this manifest.
func (m Manifest) EffectiveIsolation() string {
	if m.Isolation != "" {
		return m.Isolation
	}
	if m.Mode == ModeExternal {
		return IsolationTrusted
	}
	return IsolationInProc
}

// ParseManifest decodes exactly one JSON object; unknown fields are errors.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("app: parse manifest: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return m, fmt.Errorf("app: manifest must contain exactly one JSON object")
	}
	return m, m.Validate()
}

// Instance is one launch, never reused. Init installs routes and acquires
// resources. Run blocks until work finishes or ctx is canceled. Stop releases
// resources, including after a failed Init, and honors its fresh deadline.
type Instance interface {
	Init(*Context) error
	Run(context.Context) error
	Stop(context.Context) error
}

// Factory allocates an instance; defer resource acquisition to Init.
type Factory func() (Instance, error)

// Handler receives JSON params and an app-scoped context, NOT the caller's
// permissions. Handlers can run concurrently with Run and each other.
type Handler func(context.Context, json.RawMessage) (any, error)

// Context exposes only app-scoped services. Runtime supplies the two private
// adapters; applications never receive a router, VFS, or process manager.
type Context struct {
	Manifest Manifest
	Log      *slog.Logger
	call     func(context.Context, string, any, any) error
	handle   func(string, Handler) error
}

// NewContext is runtime wiring, not an application authoring API.
func NewContext(m Manifest, log *slog.Logger, call func(context.Context, string, any, any) error, handle func(string, Handler) error) *Context {
	return &Context{Manifest: m, Log: log, call: call, handle: handle}
}

// Call invokes an environment method with this app's manifest grant even if
// ctx came from an administrator. params/out follow encoding/json; out may be
// nil. The runtime rejects calls after this instance's lifetime ends.
func (c *Context) Call(ctx context.Context, method string, params, out any) error {
	return c.call(ctx, method, params, out)
}

// Handle declares a local route during Init only. A name is one lowercase
// segment matching [a-z][a-z0-9_-]*, served as app/<manifest.id>/<name>.
// Declaring routes and calling them both require ipc.
func (c *Context) Handle(name string, h Handler) error {
	if !entryPattern.MatchString(name) || h == nil {
		return fmt.Errorf("app: invalid local route %q", name)
	}
	return c.handle(name, h)
}

// DecodeParams decodes params into a Go value; absent params leave it alone.
func DecodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("app: bad params: %w", err)
	}
	return nil
}
