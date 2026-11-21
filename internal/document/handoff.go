package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// AppLauncher starts applications and reports whether an application is running.
type AppLauncher interface {
	IsRunning(id string) bool
	Launch(ctx context.Context, id string) (*process.Process, error)
}

// IPCDispatcher handles in-process IPC request routing.
type IPCDispatcher interface {
	Dispatch(ctx context.Context, req ipc.Request) ipc.Response
}

// HandoffManager implements versioned open-with document handoff.
type HandoffManager struct {
	vfs         vfs.DocumentFS
	grants      *vfs.GrantStore
	assocTable  *AssociationTable
	recents     *RecentsStore
	launcher    AppLauncher
	dispatcher  IPCDispatcher
	lifetimeCtx context.Context
}

// NewHandoffManager creates a new HandoffManager.
func NewHandoffManager(
	dfs vfs.DocumentFS,
	grants *vfs.GrantStore,
	assocTable *AssociationTable,
	recents *RecentsStore,
	launcher AppLauncher,
	dispatcher IPCDispatcher,
) *HandoffManager {
	return &HandoffManager{
		vfs:        dfs,
		grants:     grants,
		assocTable: assocTable,
		recents:    recents,
		launcher:   launcher,
		dispatcher: dispatcher,
	}
}

// SetLifetimeContext sets the environment lifetime context used for launching applications.
func (h *HandoffManager) SetLifetimeContext(ctx context.Context) {
	h.lifetimeCtx = ctx
}

// Handoff executes an open-with handoff request according to the versioned contract.
// It verifies caller permissions, resolves the target app, issues a scoped single-document
// grant (without conferring broader directory access), launches the target app if needed,
// dispatches document opening, and records the interaction in recents.
func (h *HandoffManager) Handoff(ctx context.Context, caller security.Principal, req sdk.HandoffRequest) (*sdk.HandoffResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	norm, err := vfs.Normalize(req.Path)
	if err != nil {
		return nil, &vfs.Error{Op: "handoff", Path: req.Path, Code: vfs.ErrInvalid, Message: fmt.Sprintf("invalid path: %v", err)}
	}
	cleanPath := "/" + norm

	// Check if document exists on VFS
	if h.vfs != nil {
		info, err := h.vfs.Stat(cleanPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, &vfs.Error{Op: "handoff", Path: cleanPath, Code: vfs.ErrNotFound, Message: "document not found"}
			}
			return nil, &vfs.Error{Op: "handoff", Path: cleanPath, Code: vfs.ErrIO, Message: err.Error()}
		}
		// Handoff issues a non-recursive grant covering exactly the handed
		// path; on a directory that shape confers only the entry itself and
		// cannot describe subtree access honestly, so directories are not
		// handoff targets. Directory association (browse a folder in Files)
		// runs on the app's own standing grants instead.
		if info.IsDir() {
			return nil, &vfs.Error{Op: "handoff", Path: cleanPath, Code: vfs.ErrIsDir, Message: "cannot hand off a directory"}
		}
	}

	// Determine requested access mode. The default is least privilege:
	// a bare "open" confers read access; write access must be requested
	// explicitly.
	modeStr := strings.TrimSpace(req.Mode)
	if modeStr == "" {
		modeStr = "read"
	}
	accessMode, err := vfs.NormalizeAccessMode(modeStr)
	if err != nil {
		return nil, &vfs.Error{Op: "handoff", Path: cleanPath, Code: vfs.ErrInvalid, Message: err.Error()}
	}

	// Caller permission check:
	// If caller is an application, it must have read access to the document.
	// Furthermore, if read-write is requested, caller must have read-write access.
	if caller.IsApp() && h.grants != nil {
		if err := h.grants.CheckAccess(caller.AppID, cleanPath, vfs.AccessRead); err != nil {
			return nil, &vfs.Error{
				Op:      "handoff",
				Path:    cleanPath,
				Code:    vfs.ErrPermission,
				Message: fmt.Sprintf("permission denied: application %q cannot hand off document without read access", caller.AppID),
			}
		}
		if accessMode == vfs.AccessReadWrite {
			if err := h.grants.CheckAccess(caller.AppID, cleanPath, vfs.AccessReadWrite); err != nil {
				return nil, &vfs.Error{
					Op:      "handoff",
					Path:    cleanPath,
					Code:    vfs.ErrReadOnly,
					Message: fmt.Sprintf("permission denied: application %q has read-only access and cannot grant read-write access", caller.AppID),
				}
			}
		}
	}

	// Resolve target application
	targetApp := strings.TrimSpace(req.AppID)
	if targetApp == "" {
		if h.assocTable == nil {
			return nil, fmt.Errorf("handoff: no association table configured")
		}
		defApp, _, found := h.assocTable.Resolve(cleanPath)
		if !found || defApp == "" {
			return nil, fmt.Errorf("handoff: no registered application for document %s", cleanPath)
		}
		targetApp = defApp
	}

	// Launch target application if not currently running. The grant is
	// issued only after a successful launch: granting first would leak a
	// live grant to any app id that fails to launch.
	launched := false
	if h.launcher != nil && !h.launcher.IsRunning(targetApp) {
		launchCtx := h.lifetimeCtx
		if launchCtx == nil {
			launchCtx = context.Background()
		}
		if _, err := h.launcher.Launch(launchCtx, targetApp); err != nil {
			return nil, fmt.Errorf("handoff: failed to launch %s: %w", targetApp, err)
		}
		launched = true
	}

	// Issue a scoped grant for the selected document ONLY to the target
	// application. recursive: false ensures the target receives access to
	// cleanPath and NOT to any parent directory, siblings, or descendants.
	// The grant is session-bound: it is reaped when the target app's run
	// ends. Re-opening the same document reuses the live grant rather than
	// minting a duplicate, and a failed open dispatch revokes the grant only
	// when this handoff issued it — revoking a reused grant would cut off a
	// document still open in the running app.
	var grantID string
	var grantIssued bool
	if h.grants != nil {
		grant, issued, err := h.grants.IssueSessionOnce(targetApp, cleanPath, accessMode, false)
		if err != nil {
			return nil, fmt.Errorf("handoff: failed to issue scoped grant: %w", err)
		}
		grantID = grant.ID
		grantIssued = issued
	}
	revokeGrant := func() {
		if grantIssued && grantID != "" && h.grants != nil {
			_ = h.grants.Revoke(grantID)
		}
	}

	// Dispatch document open to the target application
	if h.dispatcher != nil {
		openParams := map[string]any{
			"path":     cleanPath,
			"mode":     string(accessMode),
			"grant_id": grantID,
			"force":    false,
		}
		raw, err := json.Marshal(openParams)
		if err != nil {
			revokeGrant()
			return nil, err
		}

		// Dispatch with operator authority so the internal routing succeeds
		dispCtx := ipc.WithCapabilities(ctx, security.AdminCapabilities())
		dispCtx = ipc.WithPrincipal(dispCtx, security.OperatorPrincipal(security.User{Name: "guest"}))
		resp := h.dispatcher.Dispatch(dispCtx, ipc.Request{
			Method: "app/" + targetApp + "/open",
			Params: raw,
		})
		if !resp.OK {
			revokeGrant()
			return nil, fmt.Errorf("handoff: target %s failed to open document: %s", targetApp, resp.Error)
		}
	}

	// Record in recents
	if h.recents != nil {
		_, _ = h.recents.Add(cleanPath, targetApp)
	}

	return &sdk.HandoffResult{
		Version:  sdk.DocumentHandoffVersion,
		Path:     cleanPath,
		AppID:    targetApp,
		GrantID:  grantID,
		Mode:     string(accessMode),
		Launched: launched,
		Success:  true,
		Message:  fmt.Sprintf("Opened %s in %s", cleanPath, targetApp),
	}, nil
}
