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
		// layout/compositing dependency, including from tests.
		for _, list := range parts[1:] {
			for _, imp := range strings.Split(list, ",") {
				if isBloomImport(strings.TrimSpace(imp)) && !inExperience {
					t.Errorf("package %s imports bloom package %s (restricted to internal/experience/)", pkgPath, imp)
				}
			}
		}
	}
}

func isBloomImport(imp string) bool {
	return strings.HasPrefix(imp, "bloomfx.dev/")
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
		// bloom (layout, compositing and input for the experience layer only;
		// docs/architecture.md §4.1). Pre-1.0 with a single maintainer and
		// expected breaking changes, so it is pinned exactly and upgraded
		// deliberately. Its go.mod sets the Bubble Tea v2.1.1, x/ansi v0.11.9
		// and Go 1.27.3 floors above.
		"bloomfx.dev/bloom": "v0.0.0-alpha.6",
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
