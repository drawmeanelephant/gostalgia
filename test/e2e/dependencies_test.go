package e2e

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreDependencyBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}",
		"gostalgia/internal/runtime", "gostalgia/sdk", "gostalgia/apps/...", "gostalgia/cmd/gctl")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect dependency graph: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if !strings.HasPrefix(pkg, "gostalgia/") {
			t.Errorf("core/SDK dependency escaped stdlib boundary: %s", pkg)
		}
		if strings.Contains(pkg, "charmbracelet") || strings.Contains(pkg, "charm.land") {
			t.Errorf("core/SDK dependency includes Charm package: %s", pkg)
		}
	}
}

func TestCharmRestrictedToExperience(t *testing.T) {
	cmd := exec.Command("go", "list", "-f",
		"{{.ImportPath}}|{{join .Imports \",\"}}|{{join .TestImports \",\"}}|{{join .XTestImports \",\"}}", "gostalgia/...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list imports: %v\n%s", err, out)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		pkgPath := parts[0]
		inExperience := strings.HasPrefix(pkgPath, "gostalgia/internal/experience/")
		imports := strings.Split(parts[1], ",")

		for _, imp := range imports {
			imp = strings.TrimSpace(imp)
			if strings.Contains(imp, "charmbracelet") || strings.Contains(imp, "charm.land") {
				if !inExperience {
					t.Errorf("package %s imports Charm library %s (restricted to internal/experience/)", pkgPath, imp)
				}
			}
			if imp == "github.com/muesli/termenv" && !inExperience {
				t.Errorf("package %s imports terminal styling helper %s outside experience", pkgPath, imp)
			}
		}

		// bloom (any package of the bloomfx.dev module) is an experience-only
		// layout/compositing dependency, including from tests. Steep and reel
		// are narrower: even in experience or test, only test imports are allowed.
		// Gio (any package of the gioui.org module) is narrower still: only the
		// experimental window spike may import it, including from tests, because
		// gioui.org/app uses cgo on macOS and needs display headers on Linux.
		for listIndex, list := range parts[1:] {
			for _, imp := range strings.Split(list, ",") {
				imp = strings.TrimSpace(imp)
				if isBloomImport(imp) && !inExperience {
					t.Errorf("package %s imports bloom package %s (restricted to internal/experience/)", pkgPath, imp)
				}
				if isSteepImport(imp) && !steepImportAllowed(pkgPath, listIndex > 0) {
					t.Errorf("package %s imports steep package %s outside _test.go under internal/experience/ or test/", pkgPath, imp)
				}
				if isGioImport(imp) && !gioImportAllowed(pkgPath) {
					t.Errorf("package %s imports Gio package %s (restricted to internal/experience/experimental/windows/)", pkgPath, imp)
				}
			}
		}
	}
}

func isBloomImport(imp string) bool {
	return strings.HasPrefix(imp, "bloomfx.dev/")
}

func isSteepImport(imp string) bool {
	return imp == "getsteep.dev/steep" || strings.HasPrefix(imp, "getsteep.dev/steep/")
}

func isGioImport(imp string) bool {
	return strings.HasPrefix(imp, "gioui.org/")
}

func gioImportAllowed(pkgPath string) bool {
	return strings.HasPrefix(pkgPath, "gostalgia/internal/experience/experimental/windows/")
}

func steepImportAllowed(pkgPath string, testImport bool) bool {
	inAllowedTree := strings.HasPrefix(pkgPath, "gostalgia/internal/experience/") ||
		pkgPath == "gostalgia/test" || strings.HasPrefix(pkgPath, "gostalgia/test/")
	return testImport && inAllowedTree
}

func TestSteepImportFence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		imp        string
		pkgPath    string
		testImport bool
		match      bool
		allowed    bool
	}{
		{name: "root", imp: "getsteep.dev/steep", pkgPath: "gostalgia/internal/experience/shell", testImport: true, match: true, allowed: true},
		{name: "subpackage", imp: "getsteep.dev/steep/reel/vt", pkgPath: "gostalgia/test/e2e", testImport: true, match: true, allowed: true},
		{name: "lookalike", imp: "getsteep.dev/steepfake", pkgPath: "gostalgia/test/e2e", testImport: true},
		{name: "production experience", imp: "getsteep.dev/steep", pkgPath: "gostalgia/internal/experience/shell", match: true},
		{name: "test outside trees", imp: "getsteep.dev/steep", pkgPath: "gostalgia/internal/runtime", testImport: true, match: true},
		{name: "production outside trees", imp: "getsteep.dev/steep", pkgPath: "gostalgia/cmd/gostalgia", match: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSteepImport(tc.imp); got != tc.match {
				t.Fatalf("isSteepImport(%q) = %v, want %v", tc.imp, got, tc.match)
			}
			if tc.match {
				if got := steepImportAllowed(tc.pkgPath, tc.testImport); got != tc.allowed {
					t.Fatalf("steepImportAllowed(%q, %v) = %v, want %v", tc.pkgPath, tc.testImport, got, tc.allowed)
				}
			}
		})
	}
}

func TestBloomImportMatcher(t *testing.T) {
	for imp, want := range map[string]bool{
		"bloomfx.dev/bloom":        true,
		"bloomfx.dev/bloom/fx":     true,
		"bloomfx.dev/bloom/anim":   true,
		"bloomfx.dev/bloom/on":     true,
		"bloomfx.dev/other":        true,
		"example.com/bloomfx.dev":  false,
		"github.com/lrstanley/foo": false,
		"gostalgia/internal/ui":    false,
	} {
		if got := isBloomImport(imp); got != want {
			t.Errorf("isBloomImport(%q) = %v, want %v", imp, got, want)
		}
	}
}

func TestGioImportMatcher(t *testing.T) {
	for imp, want := range map[string]bool{
		"gioui.org/app":            true,
		"gioui.org/op":             true,
		"gioui.org/io/input":       true,
		"gioui.org/font/gofont":    true,
		"gioui.org/shader":         true,
		"gioui.orgx/counterfeit":   false,
		"example.com/gioui.org":    false,
		"github.com/eliasnaur/gio": false,
		"gostalgia/internal/ui":    false,
	} {
		if got := isGioImport(imp); got != want {
			t.Errorf("isGioImport(%q) = %v, want %v", imp, got, want)
		}
	}
}

func TestAppsAndSDKUseOnlyPublicBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \",\"}}", "gostalgia/sdk/...", "gostalgia/apps/...")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list app imports: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, imports, _ := strings.Cut(line, "|")
		for _, imp := range strings.Split(imports, ",") {
			if strings.HasPrefix(imp, "gostalgia/") && imp != "gostalgia/sdk" && !strings.HasPrefix(imp, "gostalgia/apps/") {
				t.Errorf("%s imports private runtime package %s", pkg, imp)
			}
			switch imp {
			case "os/exec", "syscall", "unsafe":
				t.Errorf("%s imports host/terminal API %s", pkg, imp)
			case "os":
				if strings.HasPrefix(pkg, "gostalgia/apps/") {
					t.Errorf("%s imports host/terminal API %s", pkg, imp)
				}
			}
		}
	}
}

func TestApprovedCharmBaseline(t *testing.T) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{if not .Indirect}}{{.Path}} {{.Version}}{{end}}", "all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list direct modules: %v\n%s", err, out)
	}

	expectedDirect := map[string]string{
		"gostalgia":                             "",
		"charm.land/bubbles/v2":                 "v2.2.1",
		"charm.land/bubbletea/v2":               "v2.1.1",
		"charm.land/lipgloss/v2":                "v2.0.6",
		"github.com/charmbracelet/x/ansi":       "v0.11.9",
		"github.com/charmbracelet/colorprofile": "v0.4.3",
		"github.com/muesli/termenv":             "v0.16.0",
		// SSH attach (internal/experience/sshx only): the inbound transport is
		// charm.land/ssh — the server library wish v2 is built on — plus
		// x/crypto for host-key marshaling. These are the deliberate first
		// exception to the "no inbound network" baseline; they are fenced to
		// the experience tree by TestCharmRestrictedToExperience and out of
		// the core runtime by TestCoreDependencyBoundary.
		"charm.land/ssh":      "v0.4.3",
		"golang.org/x/crypto": "v0.54.0",
		// x/sys/windows provides the Job Object API behind Windows
		// process-tree teardown (platform/proc_windows.go only).
		"golang.org/x/sys": "v0.49.0",
		// bloom (layout, compositing and input for the experience layer only;
		// docs/architecture.md §4.1). Pre-1.0 with a single maintainer and
		// expected breaking changes, so it is pinned exactly and upgraded
		// deliberately. Its go.mod sets the Bubble Tea v2.1.1, x/ansi v0.11.9
		// and Go 1.27.3 floors above.
		"bloomfx.dev/bloom": "v0.0.0-alpha.6",
		// Gio (experimental window-spike comparison only; docs/architecture.md
		// §4.6). Pinned exactly like bloom: one toolkit experiment at a time,
		// fenced to internal/experience/experimental/windows/ so nothing that
		// default builds can reach the cgo gioui.org/app or GPU packages.
		"gioui.org": "v0.10.3",
		// steep and its terminal/input capture module are exact-pinned,
		// test-only dependencies. TestSteepImportFence and the import scan
		// above confine both to _test.go in internal/experience/ or test/.
		"getsteep.dev/steep":      "v0.0.0-alpha.12",
		"getsteep.dev/steep/reel": "v0.0.0-alpha.12",
	}

	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		modPath := parts[0]
		version := ""
		if len(parts) > 1 {
			version = parts[1]
		}

		expectedVer, allowed := expectedDirect[modPath]
		seen[modPath] = true
		if !allowed {
			t.Errorf("unexpected direct dependency in go.mod: %s %s", modPath, version)
			continue
		}
		if expectedVer != "" && version != expectedVer {
			t.Errorf("module %s pinned to %s, want %s", modPath, version, expectedVer)
		}
	}
	for modPath, version := range expectedDirect {
		if !seen[modPath] {
			t.Errorf("approved module %s %s is not a direct dependency in go.mod", modPath, version)
		}
	}

	goVersion, err := exec.Command("go", "list", "-m", "-f", "{{.GoVersion}}").CombinedOutput()
	if err != nil {
		t.Fatalf("read go directive: %v\n%s", err, goVersion)
	}
	if got := strings.TrimSpace(string(goVersion)); got != "1.27.3" {
		t.Errorf("go directive = %s, want 1.27.3 (bloom's floor)", got)
	}
}

func TestNoStandaloneHostExecutables(t *testing.T) {
	// Standalone Charm executables (gum, glow, vhs) or host shells must not be runtime dependencies.
	root := filepath.Join("..", "..")
	forbiddenBinaries := []string{"gum", "glow", "vhs"}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(content)
		for _, bin := range forbiddenBinaries {
			if strings.Contains(text, `"`+bin+`"`) {
				t.Errorf("%s references forbidden standalone tool %q", path, bin)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan codebase: %v", err)
	}
}
