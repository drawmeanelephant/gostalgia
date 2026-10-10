package security

import (
	"strings"
	"sync"
	"testing"
)

// #145: revoked credentials must be reaped so the store stays bounded across
// arbitrary launch/stop cycles; previously every revoked credential stayed in
// the live map and indexes forever (5001 entries after 5000 revocations).
func TestAuditTokenStoreReapsRevokedCredentials(t *testing.T) {
	ts := NewTokenStore()
	user := User{ID: "u-churn", Name: "churn"}

	const cycles = 5000
	for i := 0; i < cycles; i++ {
		tok, err := ts.IssueAppToken("com.test.churn", int32(1000+i), "sess", user, CapIPC)
		if err != nil {
			t.Fatalf("IssueAppToken %d: %v", i, err)
		}
		ts.Revoke(tok)
	}

	live, err := ts.IssueAppToken("com.test.churn", 99999, "sess-live", user, CapIPC)
	if err != nil {
		t.Fatalf("IssueAppToken live: %v", err)
	}

	ts.mu.RLock()
	liveCreds := len(ts.credentials)
	perApp := len(ts.byApp["com.test.churn"])
	perProc := len(ts.byProc)
	tombstones := len(ts.revoked)
	ts.mu.RUnlock()

	if liveCreds != 1 {
		t.Fatalf("credentials map holds %d entries after %d revoked tokens; want 1 live entry (unbounded growth)", liveCreds, cycles)
	}
	if perApp != 1 {
		t.Fatalf("byApp index holds %d tokens for churned app; want 1 live token", perApp)
	}
	if perProc != 1 {
		t.Fatalf("byProc index holds %d entries; want 1 live process", perProc)
	}
	if tombstones > revokedCredentialLimit {
		t.Fatalf("revoked tombstones = %d, want <= %d", tombstones, revokedCredentialLimit)
	}

	if err := ts.Validate(live); err != nil {
		t.Fatalf("live token fails Validate: %v", err)
	}
	if _, _, err := ts.Authenticate(live); err != nil {
		t.Fatalf("live token fails Authenticate: %v", err)
	}
}

// Bulk revocation paths (app stop, process exit) must reap too, and recently
// revoked tokens must still report as revoked rather than unknown.
func TestAuditTokenStoreRevokeAppAndProcessReap(t *testing.T) {
	ts := NewTokenStore()
	user := User{ID: "u", Name: "u"}

	toks := make([]string, 4)
	for i := range toks {
		tok, err := ts.IssueAppToken("com.test.bulk", int32(500+i), "sess", user, CapIPC)
		if err != nil {
			t.Fatalf("IssueAppToken: %v", err)
		}
		toks[i] = tok
	}
	ts.BindProcess(toks[3], 900)

	ts.RevokeApp("com.test.bulk")
	ts.mu.RLock()
	if n := len(ts.credentials); n != 0 {
		ts.mu.RUnlock()
		t.Fatalf("RevokeApp left %d live credentials, want 0", n)
	}
	if _, ok := ts.byApp["com.test.bulk"]; ok {
		ts.mu.RUnlock()
		t.Fatal("RevokeApp left a byApp index entry")
	}
	ts.mu.RUnlock()

	err := ts.Validate(toks[0])
	if err == nil || !strings.Contains(err.Error(), "credential revoked") {
		t.Fatalf("Validate on revoked token = %v, want revoked error", err)
	}
	if _, _, err := ts.Authenticate(toks[1]); err == nil || !strings.Contains(err.Error(), "token revoked") {
		t.Fatalf("Authenticate on revoked token = %v, want revoked error", err)
	}
	cred, ok := ts.Lookup(toks[2])
	if !ok || !cred.Revoked {
		t.Fatalf("Lookup on recently revoked token = %+v, %v; want revoked tombstone", cred, ok)
	}

	tok, err := ts.IssueAppToken("com.test.proc", 4242, "sess", user, CapIPC)
	if err != nil {
		t.Fatalf("IssueAppToken: %v", err)
	}
	ts.RevokeProcess(4242)
	ts.mu.RLock()
	_, live := ts.credentials[tok]
	_, indexed := ts.byProc[4242]
	ts.mu.RUnlock()
	if live || indexed {
		t.Fatal("RevokeProcess left credential or index entry behind")
	}
}

// Tombstones are a bounded diagnostic trail: once revoked credentials exceed
// the limit, the oldest are forgotten entirely.
func TestAuditTokenStoreTombstonesBounded(t *testing.T) {
	ts := NewTokenStore()
	user := User{ID: "u", Name: "u"}

	first, err := ts.IssueAppToken("com.test.old", 1, "sess", user)
	if err != nil {
		t.Fatalf("IssueAppToken: %v", err)
	}
	ts.Revoke(first)
	for i := 0; i < revokedCredentialLimit+10; i++ {
		tok, err := ts.IssueAppToken("com.test.new", int32(10+i), "sess", user)
		if err != nil {
			t.Fatalf("IssueAppToken: %v", err)
		}
		ts.Revoke(tok)
	}

	ts.mu.RLock()
	n := len(ts.revoked)
	order := len(ts.revokedOrder)
	ts.mu.RUnlock()
	if n != revokedCredentialLimit || order != revokedCredentialLimit {
		t.Fatalf("tombstones = %d (order %d), want %d", n, order, revokedCredentialLimit)
	}
	if _, ok := ts.Lookup(first); ok {
		t.Fatal("oldest tombstone survived eviction, want forgotten")
	}
}

// The store is concurrent: churn issuers, validators and revokers together
// under -race and confirm state stays bounded.
func TestAuditTokenStoreConcurrentChurn(t *testing.T) {
	ts := NewTokenStore()
	user := User{ID: "u", Name: "u"}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				tok, err := ts.IssueAppToken("com.test.churn", int32(w*10000+i), "sess", user, CapIPC)
				if err != nil {
					t.Errorf("IssueAppToken: %v", err)
					return
				}
				_ = ts.Validate(tok)
				if _, _, err := ts.Authenticate(tok); err != nil {
					t.Errorf("Authenticate live token: %v", err)
					return
				}
				ts.Revoke(tok)
				if i%97 == 0 {
					ts.RevokeApp("com.test.churn")
				}
				if i%89 == 0 {
					ts.RevokeProcess(int32(w*10000 + i))
				}
			}
		}(w)
	}
	wg.Wait()

	ts.mu.RLock()
	liveCreds := len(ts.credentials)
	tombstones := len(ts.revoked)
	ts.mu.RUnlock()
	if tombstones > revokedCredentialLimit {
		t.Fatalf("tombstones = %d, want <= %d", tombstones, revokedCredentialLimit)
	}
	t.Logf("after concurrent churn: %d live credentials, %d tombstones", liveCreds, tombstones)
}
