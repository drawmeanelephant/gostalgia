package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"

	"gostalgia/internal/profile"
)

// userScopeTag is the authorized_keys comment tag that binds a client key to
// one Gostalgia profile, e.g.:
//
//	ssh-ed25519 AAAA... laptop gostalgia:user=alice
//
// A tagged key may only attach to sessions owned by that profile's user.
const userScopeTag = "gostalgia:user="

// Fingerprint returns the OpenSSH-format SHA256 fingerprint of a public key.
func Fingerprint(key ssh.PublicKey) string {
	return gossh.FingerprintSHA256(key)
}

// LoadOrCreateHostKey reads the PEM-encoded host key at path, generating a
// fresh ed25519 key pair persisted at mode 0600 when the file does not exist.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	if err == nil {
		signer, perr := gossh.ParsePrivateKey(pemBytes)
		if perr != nil {
			return nil, fmt.Errorf("sshx: parse host key %s: %w", path, perr)
		}
		return signer, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("sshx: read host key %s: %w", path, err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshx: generate host key: %w", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, fmt.Errorf("sshx: encode host key: %w", err)
	}
	pemBytes = pem.EncodeToMemory(block)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sshx: create host key directory: %w", err)
	}
	// O_EXCL: two boots racing first key generation must not silently end up
	// serving different host keys.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LoadOrCreateHostKey(path)
		}
		return nil, fmt.Errorf("sshx: write host key %s: %w", path, err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("sshx: write host key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sshx: write host key %s: %w", path, err)
	}
	signer, err := gossh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("sshx: parse generated host key: %w", err)
	}
	return signer, nil
}

// AuthorizedKeys is the parsed client key set. It is immutable after parse.
type AuthorizedKeys struct {
	entries []authorizedEntry
}

type authorizedEntry struct {
	key  ssh.PublicKey
	user string // profile id the key is scoped to; "" = operator-wide
}

// ParseAuthorizedKeys parses OpenSSH authorized_keys content. Blank lines and
// '#' comments are skipped; every other line must parse or the whole file is
// rejected — a malformed entry is a configuration error, never a silent
// skip. Key options (leading fields before the key type) are not supported:
// scoping goes in the comment via gostalgia:user=<profile>, keeping the file
// format a strict subset of what sshd accepts.
func ParseAuthorizedKeys(data []byte) (*AuthorizedKeys, error) {
	keys := &AuthorizedKeys{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("sshx: authorized_keys line %d: %w", i+1, err)
		}
		if len(rest) > 0 {
			return nil, fmt.Errorf("sshx: authorized_keys line %d: trailing data after key", i+1)
		}
		if len(options) > 0 {
			return nil, fmt.Errorf("sshx: authorized_keys line %d: key options are not supported (use the %s<profile> comment tag instead)", i+1, userScopeTag)
		}
		user, err := parseUserTag(comment)
		if err != nil {
			return nil, fmt.Errorf("sshx: authorized_keys line %d: %w", i+1, err)
		}
		keys.entries = append(keys.entries, authorizedEntry{key: key, user: user})
	}
	if len(keys.entries) == 0 {
		return nil, errors.New("sshx: authorized_keys contains no usable keys")
	}
	return keys, nil
}

// LoadAuthorizedKeys reads and parses an authorized_keys file. A missing or
// unreadable file is an error: the SSH listener refuses to start without an
// explicit, operator-provisioned key set.
func LoadAuthorizedKeys(path string) (*AuthorizedKeys, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sshx: read authorized_keys %s: %w", path, err)
	}
	return ParseAuthorizedKeys(data)
}

// parseUserTag extracts the gostalgia:user=<profile> scope from a key comment.
func parseUserTag(comment string) (string, error) {
	user := ""
	for _, field := range strings.Fields(comment) {
		if !strings.HasPrefix(field, userScopeTag) {
			continue
		}
		if user != "" {
			return "", fmt.Errorf("duplicate %s tags", userScopeTag)
		}
		id := strings.TrimPrefix(field, userScopeTag)
		if err := profile.ValidateID(id); err != nil {
			return "", fmt.Errorf("invalid %s value %q: %w", userScopeTag, id, err)
		}
		user = id
	}
	return user, nil
}

// Lookup reports whether key is authorized, returning its profile scope
// ("" when the key is unscoped).
func (a *AuthorizedKeys) Lookup(key ssh.PublicKey) (user string, ok bool) {
	for _, e := range a.entries {
		if ssh.KeysEqual(e.key, key) {
			return e.user, true
		}
	}
	return "", false
}
