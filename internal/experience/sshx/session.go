package sshx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"github.com/charmbracelet/colorprofile"

	"gostalgia/internal/experience/shell"
)

// attachClientType is recorded on session attachments made over SSH so
// session/list distinguishes remote shells from local ones.
const attachClientType = "ssh"

type scopeContextKey struct{}

// publicKeyAuth admits only keys in the authorized set. Password and
// keyboard-interactive handlers are never configured, so those methods are
// not offered at all; "none" auth is likewise never accepted.
func (s *Server) publicKeyAuth(ctx ssh.Context, key ssh.PublicKey) bool {
	user, ok := s.opts.Keys.Lookup(key)
	if !ok {
		s.log.Warn("sshx: rejected public key",
			"remote", ctx.RemoteAddr(), "user", ctx.User(), "fingerprint", Fingerprint(key))
		return false
	}
	ctx.SetValue(scopeContextKey{}, user)
	s.log.Info("sshx: authorized key",
		"remote", ctx.RemoteAddr(), "user", ctx.User(), "fingerprint", Fingerprint(key), "scope", user)
	return true
}

// handleSession runs one attached shell on a "shell" session request. It is
// invoked only after SessionRequestCallback has limited requests to "shell".
func (s *Server) handleSession(sess ssh.Session) {
	ctx := sess.Context()
	ptyReq, winch, ok := sess.Pty()
	if !ok {
		fmt.Fprintln(sess.Stderr(), "gostalgia: an interactive terminal is required (allocate a pty; do not use -T)")
		_ = sess.Exit(1)
		return
	}
	scope, _ := ctx.Value(scopeContextKey{}).(string)

	conn, err := s.opts.Connect(ctx)
	if err != nil {
		s.log.Warn("sshx: IPC connect failed", "remote", ctx.RemoteAddr(), "err", err)
		fmt.Fprintln(sess.Stderr(), "gostalgia: environment is unavailable")
		_ = sess.Exit(1)
		return
	}
	defer conn.Close()

	sessionID, err := resolveSession(ctx, conn, scope, sess.User())
	if err != nil {
		s.log.Warn("sshx: session resolution failed", "remote", ctx.RemoteAddr(), "scope", scope, "err", err)
		fmt.Fprintf(sess.Stderr(), "gostalgia: %s\n", err)
		_ = sess.Exit(1)
		return
	}

	// Emulated-PTY option set, matching wish's bubbletea middleware: the
	// session channel is the byte pipe, TERM comes from the pty request, the
	// color profile is derived from the client environment rather than
	// terminal queries, and window-change requests are forwarded as
	// tea.WindowSizeMsg.
	envs := append(sess.Environ(), "TERM="+ptyReq.Term)
	width, height := ptyReq.Window.Width, ptyReq.Window.Height
	if width <= 0 || height <= 0 {
		// A client without a real tty (forced pty, stdin pipe) reports 0x0;
		// fall back to a conventional size so the shell still renders.
		width, height = 80, 24
	}
	prog, model, teardown := shell.AttachedProgram(ctx, conn, conn.Done(),
		tea.WithInput(sess),
		tea.WithOutput(sess),
		tea.WithEnvironment(envs),
		tea.WithColorProfile(colorprofile.Env(envs)),
		tea.WithWindowSize(width, height),
	)
	model.SetAttachIdentity(attachClientType, "ssh-"+shortID(ctx.SessionID()), sessionID)
	defer teardown()
	defer func() {
		// An SSH disconnect kills the program without the model's clean-quit
		// detach path, so release the attachment explicitly here.
		if sid, aid := model.SessionIdentity(); sid != "" && aid != "" {
			dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = conn.Call(dctx, "session/detach", map[string]any{
				"session_id":    sid,
				"attachment_id": aid,
			}, nil)
			cancel()
		}
	}()
	defer shell.Restore(sess)

	go func() {
		for {
			select {
			case <-ctx.Done():
				prog.Kill()
				return
			case w, ok := <-winch:
				if !ok {
					return
				}
				prog.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
			}
		}
	}()

	if _, err := prog.Run(); err != nil {
		// A dead SSH connection cancels the session context and kills the
		// program — that is a normal disconnect, not a fault.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			s.log.Debug("sshx: session ended", "remote", ctx.RemoteAddr())
		} else {
			s.log.Warn("sshx: shell exited with error", "remote", ctx.RemoteAddr(), "err", err)
		}
	}
}

func shortID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "unknown"
	}
	return id
}

// sessionSummary is the subset of session/list a scope decision needs.
type sessionSummary struct {
	ID   string `json:"id"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
}

// resolveSession picks the session an SSH connection may attach to:
//  1. a key scoped with gostalgia:user=<profile> may only attach to a session
//     owned by that profile — no match is a hard failure;
//  2. otherwise, if the SSH login name matches a live session's user, that
//     session is selected (ssh <profile>@… as a selector);
//  3. otherwise "" leaves the runtime's default session resolution.
func resolveSession(ctx context.Context, caller shell.Caller, scope, sshUser string) (string, error) {
	var sessions []sessionSummary
	if err := caller.Call(ctx, "session/list", nil, &sessions); err != nil {
		return "", fmt.Errorf("resolve session: %w", err)
	}
	match := func(name string) string {
		found := ""
		for _, sess := range sessions {
			if sessionUserName(sess) == name {
				found = sess.ID // sessions are id-sorted; last wins = newest
			}
		}
		return found
	}
	if scope != "" {
		id := match(scope)
		if id == "" {
			return "", fmt.Errorf("no active session for profile %q", scope)
		}
		return id, nil
	}
	if sshUser != "" {
		if id := match(sshUser); id != "" {
			return id, nil
		}
	}
	return "", nil
}

func sessionUserName(s sessionSummary) string {
	name := s.User.Name
	if name == "" {
		name = strings.TrimPrefix(s.User.ID, "u-")
	}
	return name
}
