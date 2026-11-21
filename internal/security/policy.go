package security

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrPolicyDisabled         = errors.New("policy_denied: integration is disabled by operator policy")
	ErrPolicyHostFSNotAllowed = errors.New("policy_denied: host path is not within allowed paths")
	ErrPolicyNetworkDenied    = errors.New("policy_denied: network egress denied by operator policy")
)

// ClipboardPolicy controls access to the host system clipboard.
// When disabled, clipboard operations fall back to the internal environment clipboard.
type ClipboardPolicy struct {
	Enabled        bool `json:"enabled"`          // Master opt-in switch for host clipboard
	AllowHostRead  bool `json:"allow_host_read"`  // Permit reading from host clipboard
	AllowHostWrite bool `json:"allow_host_write"` // Permit writing to host clipboard
}

// HostFSPolicy controls mounting and accessing shared folders from the host filesystem.
type HostFSPolicy struct {
	Enabled      bool     `json:"enabled"`                 // Master opt-in switch for shared host folders
	AllowedPaths []string `json:"allowed_paths,omitempty"` // Permitted host path prefixes (empty = any valid host path if Enabled)
	AllowRead    bool     `json:"allow_read"`              // Permit reading from shared host mounts
	AllowWrite   bool     `json:"allow_write"`             // Permit writing to shared host mounts
}

// NetworkPolicy controls network egress from the environment.
type NetworkPolicy struct {
	Enabled       bool     `json:"enabled"`                  // Master opt-in switch for network egress
	AllowedHosts  []string `json:"allowed_hosts,omitempty"`  // Whitelist of destination hosts (supports "*", or specific hosts)
	BlockedHosts  []string `json:"blocked_hosts,omitempty"`  // Blacklist of destination hosts
	AllowedPorts  []int    `json:"allowed_ports,omitempty"`  // Permitted destination ports (empty = any standard port)
	AllowInsecure bool     `json:"allow_insecure,omitempty"` // Allow plaintext HTTP in addition to HTTPS
}

// OperatorPolicy defines the complete operator-controlled host integration policy.
type OperatorPolicy struct {
	Clipboard ClipboardPolicy `json:"clipboard"`
	HostFS    HostFSPolicy    `json:"hostfs"`
	Network   NetworkPolicy   `json:"network"`
}

// DefaultOperatorPolicy returns the default policy where all host integrations
// are explicitly disabled (opt-in requirement).
func DefaultOperatorPolicy() OperatorPolicy {
	return OperatorPolicy{
		Clipboard: ClipboardPolicy{
			Enabled:        false,
			AllowHostRead:  false,
			AllowHostWrite: false,
		},
		HostFS: HostFSPolicy{
			Enabled:      false,
			AllowedPaths: nil,
			AllowRead:    false,
			AllowWrite:   false,
		},
		Network: NetworkPolicy{
			Enabled:       false,
			AllowedHosts:  nil,
			BlockedHosts:  nil,
			AllowedPorts:  nil,
			AllowInsecure: false,
		},
	}
}

// CheckClipboard verifies if host clipboard access is permitted by operator policy.
func (p OperatorPolicy) CheckClipboard(isWrite bool) error {
	if !p.Clipboard.Enabled {
		return fmt.Errorf("%w: host clipboard integration is disabled", ErrPolicyDisabled)
	}
	if isWrite && !p.Clipboard.AllowHostWrite {
		return fmt.Errorf("%w: host clipboard writing is disabled", ErrPolicyDisabled)
	}
	if !isWrite && !p.Clipboard.AllowHostRead {
		return fmt.Errorf("%w: host clipboard reading is disabled", ErrPolicyDisabled)
	}
	return nil
}

// CheckHostFS verifies if accessing a shared host mount is permitted by operator policy.
func (p OperatorPolicy) CheckHostFS(hostPath string, isWrite bool) error {
	if !p.HostFS.Enabled {
		return fmt.Errorf("%w: shared host folder integration is disabled", ErrPolicyDisabled)
	}
	if isWrite && !p.HostFS.AllowWrite {
		return fmt.Errorf("%w: writing to shared host folders is disabled", ErrPolicyDisabled)
	}
	if !isWrite && !p.HostFS.AllowRead {
		return fmt.Errorf("%w: reading from shared host folders is disabled", ErrPolicyDisabled)
	}
	if len(p.HostFS.AllowedPaths) > 0 && hostPath != "" {
		cleanHost := filepath.Clean(hostPath)
		allowed := false
		for _, ap := range p.HostFS.AllowedPaths {
			cleanAllowed := filepath.Clean(ap)
			rel, err := filepath.Rel(cleanAllowed, cleanHost)
			if err == nil && !strings.HasPrefix(rel, "..") {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%w: path %q is not under any allowed path", ErrPolicyHostFSNotAllowed, hostPath)
		}
	}
	return nil
}

// CheckNetwork verifies whether an egress request to targetURL is permitted by operator policy.
func (p OperatorPolicy) CheckNetwork(targetURL string) error {
	if !p.Network.Enabled {
		return fmt.Errorf("%w: network egress is disabled", ErrPolicyDisabled)
	}

	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("%w: invalid url: %v", ErrPolicyNetworkDenied, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("%w: unsupported scheme %q (must be http or https)", ErrPolicyNetworkDenied, scheme)
	}
	if scheme == "http" && !p.Network.AllowInsecure {
		return fmt.Errorf("%w: plaintext HTTP is not allowed by policy (HTTPS required)", ErrPolicyNetworkDenied)
	}

	hostname := canonicalizeHost(u.Hostname())
	if hostname == "" {
		return fmt.Errorf("%w: empty host", ErrPolicyNetworkDenied)
	}

	// Check blocked hosts
	for _, blocked := range p.Network.BlockedHosts {
		if hostname == canonicalizeHost(blocked) {
			return fmt.Errorf("%w: host %q is blocked by policy", ErrPolicyNetworkDenied, hostname)
		}
	}

	// Check allowed hosts if whitelist specified
	if len(p.Network.AllowedHosts) > 0 {
		matched := false
		for _, allowed := range p.Network.AllowedHosts {
			allowed = canonicalizeHost(allowed)
			if allowed == "*" || hostname == allowed {
				matched = true
				break
			}
			if strings.HasPrefix(allowed, "*.") {
				suffix := strings.TrimPrefix(allowed, "*")
				if strings.HasSuffix(hostname, suffix) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return fmt.Errorf("%w: host %q is not in allowed hosts whitelist", ErrPolicyNetworkDenied, hostname)
		}
	}

	// Check allowed ports if specified
	if len(p.Network.AllowedPorts) > 0 {
		portStr := u.Port()
		var port int
		if portStr != "" {
			_, err := fmt.Sscanf(portStr, "%d", &port)
			if err != nil {
				return fmt.Errorf("%w: invalid port %q", ErrPolicyNetworkDenied, portStr)
			}
		} else {
			if scheme == "https" {
				port = 443
			} else {
				port = 80
			}
		}
		portAllowed := false
		for _, ap := range p.Network.AllowedPorts {
			if ap == port {
				portAllowed = true
				break
			}
		}
		if !portAllowed {
			return fmt.Errorf("%w: port %d is not allowed by policy", ErrPolicyNetworkDenied, port)
		}
	}

	return nil
}

var errNotIPv4Literal = errors.New("not an IPv4 literal")

// canonicalizeHost normalizes a request host or host-list entry so alternate
// spellings of the same destination compare equal: it lowercases, strips a
// single trailing dot (the fully-qualified form of a DNS name), drops brackets
// around IPv6 literals, and normalizes IP literals to canonical netip text.
// Besides standard forms it recognizes the IPv4 spellings inet_aton-style
// system resolvers accept — bare decimal ("2130706433"), hexadecimal
// ("0x7f000001"), octal ("0177.0.0.1"), mixed-radix, and abbreviated dotted
// components ("127.1") — since the dialer resolves those to the same address.
func canonicalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	if addr, err := parseIPv4Literal(host); err == nil {
		return addr.String()
	}
	return host
}

// parseIPv4Literal parses the non-standard IPv4 literal forms that
// inet_aton(3)-style resolvers accept but netip.ParseAddr rejects: up to four
// decimal/octal/hexadecimal components where the last component expands to
// fill the remaining address bytes ("127.0.0.1", "127.1", "10.0.513",
// "0x7f.0.0.1", "2130706433").
func parseIPv4Literal(host string) (netip.Addr, error) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, errNotIPv4Literal
	}
	vals := make([]uint64, len(parts))
	for i, part := range parts {
		v, err := parseIPv4Component(part)
		if err != nil {
			return netip.Addr{}, errNotIPv4Literal
		}
		vals[i] = v
	}
	for _, v := range vals[:len(vals)-1] {
		if v > 0xff {
			return netip.Addr{}, errNotIPv4Literal
		}
	}
	lastBits := uint(8 * (5 - len(vals)))
	if last := vals[len(vals)-1]; last >= 1<<lastBits {
		return netip.Addr{}, errNotIPv4Literal
	}
	ip := uint64(0)
	for _, v := range vals[:len(vals)-1] {
		ip = ip<<8 | v
	}
	ip = ip<<lastBits | vals[len(vals)-1]
	return netip.AddrFrom4([4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)}), nil
}

// parseIPv4Component parses one inet_aton-style component: hexadecimal with a
// "0x" prefix, octal with a leading "0", otherwise decimal.
func parseIPv4Component(part string) (uint64, error) {
	if part == "" || part[0] == '+' || part[0] == '-' {
		return 0, errNotIPv4Literal
	}
	base := 10
	switch {
	case strings.HasPrefix(part, "0x"):
		base, part = 16, part[2:]
	case len(part) > 1 && part[0] == '0':
		base, part = 8, part[1:]
	}
	if part == "" {
		return 0, errNotIPv4Literal
	}
	return strconv.ParseUint(part, base, 32)
}

// PolicyStore provides thread-safe access to the runtime OperatorPolicy.
type PolicyStore struct {
	mu     sync.RWMutex
	policy OperatorPolicy
}

// NewPolicyStore returns a PolicyStore initialized with policy.
func NewPolicyStore(policy OperatorPolicy) *PolicyStore {
	return &PolicyStore{policy: policy}
}

// Get returns a copy of the current policy.
func (s *PolicyStore) Get() OperatorPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.policy
}

// Set replaces the policy.
func (s *PolicyStore) Set(p OperatorPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
}

// Update modifies the policy inside a write lock.
func (s *PolicyStore) Update(fn func(*OperatorPolicy)) OperatorPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.policy)
	return s.policy
}
