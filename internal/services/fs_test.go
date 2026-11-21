package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

func TestFSDocumentOperationsOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	// 1. fs/save
	payload := base64.StdEncoding.EncodeToString([]byte("hello document"))
	resp := env.call(context.Background(), admin, "fs/save", map[string]any{
		"path":        "/users/guest/documents/doc.txt",
		"data_base64": payload,
	})
	if !resp.OK {
		t.Fatalf("fs/save failed: %s", resp.Error)
	}

	// 2. fs/stat
	resp = env.call(context.Background(), admin, "fs/stat", map[string]string{
		"path": "/users/guest/documents/doc.txt",
	})
	if !resp.OK {
		t.Fatalf("fs/stat failed: %s", resp.Error)
	}
	var statOut struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	must(t, json.Unmarshal(resp.Data, &statOut))
	if statOut.Name != "doc.txt" || statOut.IsDir || statOut.Size != int64(len("hello document")) {
		t.Fatalf("unexpected stat output: %+v", statOut)
	}

	// 3. fs/save overwrite=false collision
	overwriteFalse := false
	resp = env.call(context.Background(), admin, "fs/save", map[string]any{
		"path":        "/users/guest/documents/doc.txt",
		"data_base64": payload,
		"overwrite":   &overwriteFalse,
	})
	if resp.OK {
		t.Fatal("fs/save with overwrite=false should fail on existing file")
	}

	// 4. fs/read bounded read
	resp = env.call(context.Background(), admin, "fs/read", map[string]any{
		"path":   "/users/guest/documents/doc.txt",
		"offset": 6,
		"limit":  4,
	})
	if !resp.OK {
		t.Fatalf("fs/read bounded failed: %s", resp.Error)
	}
	var readOut struct {
		DataBase64 string `json:"data_base64"`
		Size       int    `json:"size"`
		TotalSize  int64  `json:"total_size"`
	}
	must(t, json.Unmarshal(resp.Data, &readOut))
	readDecoded, err := base64.StdEncoding.DecodeString(readOut.DataBase64)
	if err != nil || string(readDecoded) != "docu" {
		t.Fatalf("bounded read output = %q, want 'docu'", string(readDecoded))
	}

	// 5. fs/copy
	resp = env.call(context.Background(), admin, "fs/copy", map[string]any{
		"src":       "/users/guest/documents/doc.txt",
		"dst":       "/users/guest/documents/doc_copy.txt",
		"overwrite": false,
	})
	if !resp.OK {
		t.Fatalf("fs/copy failed: %s", resp.Error)
	}

	// 6. fs/copy cross-mount (/users/... to /tmp/...)
	resp = env.call(context.Background(), admin, "fs/copy", map[string]any{
		"src":       "/users/guest/documents/doc.txt",
		"dst":       "/tmp/doc_tmp.txt",
		"overwrite": false,
	})
	if !resp.OK {
		t.Fatalf("fs/copy cross-mount failed: %s", resp.Error)
	}

	// 7. fs/rename same-mount
	resp = env.call(context.Background(), admin, "fs/rename", map[string]any{
		"src":       "/users/guest/documents/doc_copy.txt",
		"dst":       "/users/guest/documents/doc_renamed.txt",
		"overwrite": false,
	})
	if !resp.OK {
		t.Fatalf("fs/rename failed: %s", resp.Error)
	}

	// 8. fs/rename cross-mount must fail explicitly
	resp = env.call(context.Background(), admin, "fs/rename", map[string]any{
		"src":       "/users/guest/documents/doc_renamed.txt",
		"dst":       "/tmp/doc_renamed.txt",
		"overwrite": false,
	})
	if resp.OK {
		t.Fatal("fs/rename cross-mount should have failed")
	}

	// 9. fs/move cross-mount succeeds
	resp = env.call(context.Background(), admin, "fs/move", map[string]any{
		"src":       "/tmp/doc_tmp.txt",
		"dst":       "/users/guest/documents/from_tmp.txt",
		"overwrite": false,
	})
	if !resp.OK {
		t.Fatalf("fs/move cross-mount failed: %s", resp.Error)
	}

	// 10. fs/trash, fs/trash/list, fs/restore, fs/trash/empty
	resp = env.call(context.Background(), admin, "fs/trash", map[string]string{
		"path": "/users/guest/documents/from_tmp.txt",
	})
	if !resp.OK {
		t.Fatalf("fs/trash failed: %s", resp.Error)
	}
	var trashEntry vfs.TrashEntry
	must(t, json.Unmarshal(resp.Data, &trashEntry))
	if trashEntry.Name != "from_tmp.txt" {
		t.Fatalf("unexpected trash entry: %+v", trashEntry)
	}

	// Trash list
	resp = env.call(context.Background(), admin, "fs/trash/list", nil)
	if !resp.OK {
		t.Fatalf("fs/trash/list failed: %s", resp.Error)
	}
	var listOut struct {
		Entries []vfs.TrashEntry `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &listOut))
	if len(listOut.Entries) == 0 {
		t.Fatal("fs/trash/list returned empty entries")
	}

	// Restore
	resp = env.call(context.Background(), admin, "fs/restore", map[string]any{
		"id":        trashEntry.ID,
		"dst":       "/users/guest/documents/restored.txt",
		"overwrite": false,
	})
	if !resp.OK {
		t.Fatalf("fs/restore failed: %s", resp.Error)
	}

	// Verify restored file exists
	if _, err := env.ctx.VFS.Stat("/users/guest/documents/restored.txt"); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}

	// Trash again and empty
	resp = env.call(context.Background(), admin, "fs/trash", map[string]string{
		"path": "/users/guest/documents/restored.txt",
	})
	if !resp.OK {
		t.Fatalf("fs/trash second time failed: %s", resp.Error)
	}

	resp = env.call(context.Background(), admin, "fs/trash/empty", nil)
	if !resp.OK {
		t.Fatalf("fs/trash/empty failed: %s", resp.Error)
	}
	var emptyOut struct {
		Count int `json:"count"`
	}
	must(t, json.Unmarshal(resp.Data, &emptyOut))
	if emptyOut.Count != 1 {
		t.Fatalf("expected 1 item emptied, got %d", emptyOut.Count)
	}
}

func TestFSCapabilityDenials(t *testing.T) {
	env := newTestEnv(t)
	readOnly := security.NewCapabilities(security.CapFileRead)
	writeOnly := security.NewCapabilities(security.CapFileWrite)

	// fs/save requires write
	resp := env.call(context.Background(), readOnly, "fs/save", map[string]string{
		"path": "/users/guest/documents/test.txt",
	})
	if resp.OK {
		t.Fatal("fs/save should require CapFileWrite")
	}

	// fs/rename requires write
	resp = env.call(context.Background(), readOnly, "fs/rename", map[string]string{
		"src": "/users/guest/documents/a",
		"dst": "/users/guest/documents/b",
	})
	if resp.OK {
		t.Fatal("fs/rename should require CapFileWrite")
	}

	// fs/copy requires both read and write
	resp = env.call(context.Background(), readOnly, "fs/copy", map[string]string{
		"src": "/users/guest/documents/a",
		"dst": "/users/guest/documents/b",
	})
	if resp.OK {
		t.Fatal("fs/copy should require CapFileWrite")
	}
	resp = env.call(context.Background(), writeOnly, "fs/copy", map[string]string{
		"src": "/users/guest/documents/a",
		"dst": "/users/guest/documents/b",
	})
	if resp.OK {
		t.Fatal("fs/copy should require CapFileRead")
	}

	// fs/trash/list requires read
	resp = env.call(context.Background(), writeOnly, "fs/trash/list", nil)
	if resp.OK {
		t.Fatal("fs/trash/list should require CapFileRead")
	}

	// fs/trash requires write
	resp = env.call(context.Background(), readOnly, "fs/trash", map[string]string{
		"path": "/users/guest/documents/a",
	})
	if resp.OK {
		t.Fatal("fs/trash should require CapFileWrite")
	}

	// fs/restore requires write
	resp = env.call(context.Background(), readOnly, "fs/restore", map[string]string{
		"id": "item-1",
	})
	if resp.OK {
		t.Fatal("fs/restore should require CapFileWrite")
	}

	// fs/trash/empty requires write
	resp = env.call(context.Background(), readOnly, "fs/trash/empty", nil)
	if resp.OK {
		t.Fatal("fs/trash/empty should require CapFileWrite")
	}
}

func TestFSConfinementOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	escapePaths := []string{
		"../../outside",
		"/../outside",
		"/users/guest/../../escaped",
	}

	for _, p := range escapePaths {
		resp := env.call(context.Background(), admin, "fs/stat", map[string]string{"path": p})
		if resp.OK {
			t.Errorf("fs/stat(%q) should have failed confinement", p)
		}
		resp = env.call(context.Background(), admin, "fs/save", map[string]string{"path": p})
		if resp.OK {
			t.Errorf("fs/save(%q) should have failed confinement", p)
		}
		resp = env.call(context.Background(), admin, "fs/rename", map[string]string{"src": p, "dst": "/valid"})
		if resp.OK {
			t.Errorf("fs/rename(%q) should have failed confinement", p)
		}
		resp = env.call(context.Background(), admin, "fs/copy", map[string]string{"src": p, "dst": "/valid"})
		if resp.OK {
			t.Errorf("fs/copy(%q) should have failed confinement", p)
		}
		resp = env.call(context.Background(), admin, "fs/trash", map[string]string{"path": p})
		if resp.OK {
			t.Errorf("fs/trash(%q) should have failed confinement", p)
		}
	}
}

func TestFSSizeLimitsOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	// Try to write payload exceeding MaxIPCWriteLimit (4MB)
	hugeData := make([]byte, vfs.MaxIPCWriteLimit+1024)
	hugeBase64 := base64.StdEncoding.EncodeToString(hugeData)

	resp := env.call(context.Background(), admin, "fs/write", map[string]string{
		"path":        "/users/guest/documents/huge.txt",
		"data_base64": hugeBase64,
	})
	if resp.OK {
		t.Fatal("fs/write exceeding MaxIPCWriteLimit should fail")
	}
	if !strings.Contains(resp.Error, "exceeds write limit") {
		t.Fatalf("unexpected error message: %s", resp.Error)
	}

	resp = env.call(context.Background(), admin, "fs/save", map[string]string{
		"path":        "/users/guest/documents/huge.txt",
		"data_base64": hugeBase64,
	})
	if resp.OK {
		t.Fatal("fs/save exceeding MaxIPCWriteLimit should fail")
	}
}

// Issue #264: a write at the documented MaxIPCWriteLimit must survive the
// wire end-to-end — the base64 envelope of a 4 MiB payload overflows the
// old 4 MiB frame cap, and a frame rejection must fail only that call, not
// the whole connection.
func TestFSWriteDocumentedLimitOverSocket(t *testing.T) {
	env := newTestEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	srv := ipc.NewServer(ln, env.router, "test-token", nil)
	go srv.Serve()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	must(t, err)
	client, err := ipc.NewClient(conn, "test-token")
	must(t, err)
	t.Cleanup(func() { client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	data := make([]byte, vfs.MaxIPCWriteLimit)
	for i := range data {
		data[i] = byte(i)
	}
	err = client.Call(ctx, "fs/write", map[string]string{
		"path":        "/users/guest/documents/big.bin",
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err != nil {
		t.Fatalf("fs/write at the documented limit failed end-to-end: %v", err)
	}

	var stat struct {
		Size int64 `json:"size"`
	}
	must(t, client.Call(ctx, "fs/stat", map[string]string{
		"path": "/users/guest/documents/big.bin",
	}, &stat))
	if stat.Size != int64(len(data)) {
		t.Fatalf("stat size = %d, want %d", stat.Size, len(data))
	}

	// A request whose frame cannot fit at all fails only that call; the
	// connection and its other calls survive.
	err = client.Call(ctx, "fs/write", map[string]string{
		"path":        "/users/guest/documents/too-big.bin",
		"data_base64": base64.StdEncoding.EncodeToString(make([]byte, 2*vfs.MaxIPCWriteLimit)),
	}, nil)
	if err == nil {
		t.Fatal("frame-overflowing write unexpectedly succeeded")
	}
	must(t, client.Call(ctx, "fs/stat", map[string]string{
		"path": "/users/guest/documents/big.bin",
	}, &stat))
}

func TestAppPrivateStorageIsolationOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	appAPrincipal := security.Principal{Kind: security.PrincipalKindApp, AppID: "com.gostalgia.appA"}
	appBPrincipal := security.Principal{Kind: security.PrincipalKindApp, AppID: "com.gostalgia.appB"}
	appCaps := security.NewCapabilities(security.CapFileRead, security.CapFileWrite)

	// App A writes a private file
	payloadA := base64.StdEncoding.EncodeToString([]byte("appA secret data"))
	resp := env.callAs(context.Background(), appAPrincipal, appCaps, "fs/write", map[string]any{
		"path":        "/apps/data/com.gostalgia.appA/private.txt",
		"data_base64": payloadA,
	})
	if !resp.OK {
		t.Fatalf("appA write to private storage failed: %s", resp.Error)
	}

	// App A reads its own private file
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/apps/data/com.gostalgia.appA/private.txt",
	})
	if !resp.OK {
		t.Fatalf("appA read own private file failed: %s", resp.Error)
	}

	// App B attempts to read App A's private file -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/apps/data/com.gostalgia.appA/private.txt",
	})
	if resp.OK {
		t.Fatal("appB read of appA private file should have failed")
	}

	// App B attempts to write to App A's private file -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/write", map[string]any{
		"path":        "/apps/data/com.gostalgia.appA/malicious.txt",
		"data_base64": payloadA,
	})
	if resp.OK {
		t.Fatal("appB write to appA private directory should have failed")
	}

	// App B attempts to remove App A's private file -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/remove", map[string]any{
		"path": "/apps/data/com.gostalgia.appA/private.txt",
	})
	if resp.OK {
		t.Fatal("appB remove of appA private file should have failed")
	}

	// App B attempts to rename App A's file -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/rename", map[string]any{
		"src": "/apps/data/com.gostalgia.appA/private.txt",
		"dst": "/apps/data/com.gostalgia.appB/stolen.txt",
	})
	if resp.OK {
		t.Fatal("appB rename of appA private file should have failed")
	}

	// App B attempts to copy App A's file -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/copy", map[string]any{
		"src": "/apps/data/com.gostalgia.appA/private.txt",
		"dst": "/apps/data/com.gostalgia.appB/copied.txt",
	})
	if resp.OK {
		t.Fatal("appB copy of appA private file should have failed")
	}

	// App B attempts to list App A's private directory -> must fail
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/list", map[string]any{
		"path": "/apps/data/com.gostalgia.appA",
	})
	if resp.OK {
		t.Fatal("appB list of appA private directory should have failed")
	}

	// App B lists /apps/data -> must only see com.gostalgia.appB
	must(t, env.ctx.VFS.MkdirAll("/apps/data/com.gostalgia.appB"))
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/list", map[string]any{
		"path": "/apps/data",
	})
	if !resp.OK {
		t.Fatalf("appB list /apps/data failed: %s", resp.Error)
	}
	var listOut struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &listOut))
	for _, entry := range listOut.Entries {
		if entry.Name == "com.gostalgia.appA" {
			t.Fatal("appB should not see com.gostalgia.appA in /apps/data")
		}
	}

	// Operator (admin) has full access
	resp = env.call(context.Background(), admin, "fs/read", map[string]any{
		"path": "/apps/data/com.gostalgia.appA/private.txt",
	})
	if !resp.OK {
		t.Fatalf("admin read of appA private file failed: %s", resp.Error)
	}
}

func TestScopedGrantsOverIPC(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	appAPrincipal := security.Principal{Kind: security.PrincipalKindApp, AppID: "com.gostalgia.appA"}
	appBPrincipal := security.Principal{Kind: security.PrincipalKindApp, AppID: "com.gostalgia.appB"}
	appCaps := security.NewCapabilities(security.CapFileRead, security.CapFileWrite)

	// Create a shared document
	docData := base64.StdEncoding.EncodeToString([]byte("public report"))
	resp := env.call(context.Background(), admin, "fs/save", map[string]any{
		"path":        "/users/guest/documents/report.txt",
		"data_base64": docData,
	})
	if !resp.OK {
		t.Fatalf("seed document failed: %s", resp.Error)
	}

	// App A initially cannot read it (no grant)
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/users/guest/documents/report.txt",
	})
	if resp.OK {
		t.Fatal("appA read before grant should have failed")
	}

	// App A cannot issue a grant (requires CapAdmin)
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/grant", map[string]any{
		"app_id": "com.gostalgia.appA",
		"path":   "/users/guest/documents/report.txt",
		"access": "read",
	})
	if resp.OK {
		t.Fatal("appA issuing grant should have failed")
	}

	// Operator grants read-only access on the report to App A
	resp = env.call(context.Background(), admin, "fs/grant", map[string]any{
		"app_id": "com.gostalgia.appA",
		"path":   "/users/guest/documents/report.txt",
		"access": "read",
	})
	if !resp.OK {
		t.Fatalf("admin issue grant failed: %s", resp.Error)
	}
	var grantOut struct {
		Grant struct {
			ID string `json:"id"`
		} `json:"grant"`
	}
	must(t, json.Unmarshal(resp.Data, &grantOut))
	grantID := grantOut.Grant.ID
	if grantID == "" {
		t.Fatal("expected non-empty grant id")
	}

	// App A can now read the document
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/users/guest/documents/report.txt",
	})
	if !resp.OK {
		t.Fatalf("appA read after grant failed: %s", resp.Error)
	}

	// App A CANNOT write to the document (grant is read-only)
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/write", map[string]any{
		"path":        "/users/guest/documents/report.txt",
		"data_base64": base64.StdEncoding.EncodeToString([]byte("overwritten")),
	})
	if resp.OK {
		t.Fatal("appA write to read-only grant should have failed")
	}

	// App B still cannot read the document
	resp = env.callAs(context.Background(), appBPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/users/guest/documents/report.txt",
	})
	if resp.OK {
		t.Fatal("appB read without grant should have failed")
	}

	// App A lists its grants
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/grant/list", nil)
	if !resp.OK {
		t.Fatalf("appA list grants failed: %s", resp.Error)
	}
	var listGrantsOut struct {
		Grants []struct {
			ID string `json:"id"`
		} `json:"grants"`
	}
	must(t, json.Unmarshal(resp.Data, &listGrantsOut))
	if len(listGrantsOut.Grants) != 1 || listGrantsOut.Grants[0].ID != grantID {
		t.Fatalf("unexpected grant list for appA: %+v", listGrantsOut)
	}

	// App A cannot list App B's grants
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/grant/list", map[string]string{
		"app_id": "com.gostalgia.appB",
	})
	if resp.OK {
		t.Fatal("appA should not be allowed to list appB grants")
	}

	// Operator revokes the grant
	resp = env.call(context.Background(), admin, "fs/grant/revoke", map[string]string{
		"id": grantID,
	})
	if !resp.OK {
		t.Fatalf("revoke grant failed: %s", resp.Error)
	}

	// App A subsequent read fails immediately with revoked error
	resp = env.callAs(context.Background(), appAPrincipal, appCaps, "fs/read", map[string]any{
		"path": "/users/guest/documents/report.txt",
	})
	if resp.OK {
		t.Fatal("appA read after grant revocation should have failed")
	}
	if !strings.Contains(resp.Error, "revoked") {
		t.Fatalf("expected error mentioning revoked, got: %s", resp.Error)
	}
}
