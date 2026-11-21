package security_test

import (
	"testing"

	"gostalgia/internal/security"
)

func TestDefaultOperatorPolicy(t *testing.T) {
	p := security.DefaultOperatorPolicy()

	if p.Clipboard.Enabled {
		t.Errorf("expected clipboard disabled by default")
	}
	if p.HostFS.Enabled {
		t.Errorf("expected hostfs disabled by default")
	}
	if p.Network.Enabled {
		t.Errorf("expected network disabled by default")
	}

	if err := p.CheckClipboard(false); err == nil {
		t.Errorf("expected clipboard read error when disabled")
	}
	if err := p.CheckClipboard(true); err == nil {
		t.Errorf("expected clipboard write error when disabled")
	}

	if err := p.CheckHostFS("/tmp/foo", false); err == nil {
		t.Errorf("expected hostfs read error when disabled")
	}
	if err := p.CheckHostFS("/tmp/foo", true); err == nil {
		t.Errorf("expected hostfs write error when disabled")
	}

	if err := p.CheckNetwork("https://example.com"); err == nil {
		t.Errorf("expected network error when disabled")
	}
}

func TestClipboardPolicy(t *testing.T) {
	p := security.DefaultOperatorPolicy()
	p.Clipboard.Enabled = true
	p.Clipboard.AllowHostRead = true
	p.Clipboard.AllowHostWrite = false

	if err := p.CheckClipboard(false); err != nil {
		t.Fatalf("expected clipboard read allowed: %v", err)
	}
	if err := p.CheckClipboard(true); err == nil {
		t.Fatalf("expected clipboard write denied")
	}

	p.Clipboard.AllowHostWrite = true
	if err := p.CheckClipboard(true); err != nil {
		t.Fatalf("expected clipboard write allowed: %v", err)
	}
}

func TestHostFSPolicy(t *testing.T) {
	p := security.DefaultOperatorPolicy()
	p.HostFS.Enabled = true
	p.HostFS.AllowRead = true
	p.HostFS.AllowWrite = false
	p.HostFS.AllowedPaths = []string{"/tmp/allowed"}

	if err := p.CheckHostFS("/tmp/allowed/sub", false); err != nil {
		t.Fatalf("expected read allowed: %v", err)
	}
	if err := p.CheckHostFS("/tmp/allowed/sub", true); err == nil {
		t.Fatalf("expected write denied")
	}
	if err := p.CheckHostFS("/etc/passwd", false); err == nil {
		t.Fatalf("expected path outside allowed paths denied")
	}
}

func TestNetworkPolicy(t *testing.T) {
	p := security.DefaultOperatorPolicy()
	p.Network.Enabled = true
	p.Network.AllowedHosts = []string{"example.com", "*.google.com"}
	p.Network.BlockedHosts = []string{"evil.google.com"}
	p.Network.AllowedPorts = []int{443, 8443}

	// Plain HTTP blocked because allow_insecure is false
	if err := p.CheckNetwork("http://example.com"); err == nil {
		t.Fatalf("expected http blocked when allow_insecure is false")
	}

	// HTTPS to example.com:443 allowed
	if err := p.CheckNetwork("https://example.com"); err != nil {
		t.Fatalf("expected allowed: %v", err)
	}

	// Wildcard subdomain allowed
	if err := p.CheckNetwork("https://maps.google.com"); err != nil {
		t.Fatalf("expected allowed: %v", err)
	}

	// Blocked host denied
	if err := p.CheckNetwork("https://evil.google.com"); err == nil {
		t.Fatalf("expected blocked host denied")
	}

	// Unlisted host denied
	if err := p.CheckNetwork("https://other.com"); err == nil {
		t.Fatalf("expected unlisted host denied")
	}

	// Port not in allowed list denied
	if err := p.CheckNetwork("https://example.com:8080"); err == nil {
		t.Fatalf("expected unlisted port denied")
	}
}

// Alternate spellings of a blocked destination must be denied: the dialer
// resolves a trailing-dot DNS name to the same host, and the system resolver
// maps decimal, hexadecimal, octal, and abbreviated dotted IPv4 literals (as
// well as IPv4-mapped IPv6 spellings) to the same address. Verbatim string
// comparison lets an app reach a blocked host whenever the allowlist is empty
// or admits the spelling.
func TestNetworkPolicyBlockedHostSpellings(t *testing.T) {
	p := security.DefaultOperatorPolicy()
	p.Network.Enabled = true
	p.Network.AllowedHosts = []string{"*"}
	p.Network.BlockedHosts = []string{"evil.example.com", "127.0.0.1", "::1"}

	denied := []string{
		"https://evil.example.com.",       // trailing-dot FQDN form
		"https://EVIL.example.COM.",       // case + trailing dot
		"https://127.0.0.1.",              // trailing dot on an IP literal
		"https://2130706433",              // bare decimal IPv4
		"https://0x7f000001",              // hexadecimal IPv4
		"https://127.1",                   // abbreviated dotted quad
		"https://127.0.1",                 // abbreviated dotted quad
		"https://0177.0.0.1",              // octal components
		"https://0x7f.0.0.1",              // mixed radix components
		"https://[::ffff:127.0.0.1]",      // IPv4-mapped IPv6
		"https://[0:0:0:0:0:ffff:7f00:1]", // expanded IPv4-mapped IPv6
		"https://[0:0:0:0:0:0:0:1]",       // expanded IPv6 spelling of ::1
	}
	for _, target := range denied {
		if err := p.CheckNetwork(target); err == nil {
			t.Errorf("expected %q denied: alternate spelling bypasses blocked_hosts", target)
		}
	}

	// Sanity: an unlisted-but-allowed spelling still passes so the exercise
	// above proves blocking, not a blanket denial.
	if err := p.CheckNetwork("https://10.0.0.1"); err != nil {
		t.Fatalf("expected unlisted host allowed under wildcard: %v", err)
	}
}

// Allowed hosts must match under the same canonicalization, so an operator
// entry written in one spelling still admits the equivalent request spelling.
func TestNetworkPolicyAllowedHostSpellings(t *testing.T) {
	p := security.DefaultOperatorPolicy()
	p.Network.Enabled = true
	p.Network.AllowedHosts = []string{"Example.COM.", "*.google.com", "127.0.0.1"}
	p.Network.BlockedHosts = []string{"evil.google.com"}

	allowed := []string{
		"https://example.com",
		"https://EXAMPLE.com.",
		"https://maps.google.com",
		"https://MAPS.GOOGLE.com.",
		"https://127.1",
		"https://2130706433",
		"https://[::ffff:127.0.0.1]",
	}
	for _, target := range allowed {
		if err := p.CheckNetwork(target); err != nil {
			t.Errorf("expected %q allowed: %v", target, err)
		}
	}

	// A blocked entry still wins over a wildcard allowlist match under any
	// spelling, and unlisted hosts are still denied.
	for _, target := range []string{
		"https://evil.google.com.",
		"https://EVIL.google.com",
		"https://other.example.com.",
	} {
		if err := p.CheckNetwork(target); err == nil {
			t.Errorf("expected %q denied", target)
		}
	}
}

func TestPolicyStore(t *testing.T) {
	store := security.NewPolicyStore(security.DefaultOperatorPolicy())
	if store.Get().Clipboard.Enabled {
		t.Fatalf("expected disabled")
	}

	store.Update(func(p *security.OperatorPolicy) {
		p.Clipboard.Enabled = true
	})
	if !store.Get().Clipboard.Enabled {
		t.Fatalf("expected updated")
	}
}
