package skills_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func bundle(t *testing.T, root, dir, text string) string {
	t.Helper()
	p := filepath.Join(root, ".agents", "skills", dir)
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoveryRootsAndWorktreeBoundary(t *testing.T) {
	outer, home := t.TempDir(), t.TempDir()
	bundle(t, outer, "outer", "---\ndescription: Outer\n---\n")
	if err := os.Mkdir(filepath.Join(outer, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(outer, "worktree")
	bundle(t, worktree, "inner", "---\ndescription: Inner\n---\n")
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: elsewhere"), 0644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(worktree, "sub", "dir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	c := skills.Discover(sub, home)
	if _, err := c.View("inner", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.View("outer", "", 0, 0); err == nil {
		t.Fatal("crossed nearest worktree boundary")
	}
	page, err := skills.Discover(t.TempDir(), home).List("")
	if err != nil || len(page.Skills) != 0 {
		t.Fatalf("missing roots = %+v, %v", page, err)
	}
}

func TestAmbiguityDoesNotFallBackToPersonalSkill(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for _, dir := range []string{"one", "two"} {
		bundle(t, project, dir, "---\nname: review\ndescription: Review\n---\n")
	}
	bundle(t, home, "review", "---\ndescription: Personal\n---\n")
	c := skills.Discover(project, home)
	page, err := c.List("")
	if err != nil || len(page.Skills) != 0 {
		t.Fatalf("ambiguous name listed: %+v, %v", page, err)
	}
	if _, err := c.View("review", "", 0, 0); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("view ambiguity = %v", err)
	}
	if _, err := c.View("missing", "", 0, 0); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing view = %v", err)
	}
}

func TestSymlinkDedupCyclesAndResourceContainment(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	external := t.TempDir()
	p := bundle(t, external, "portable", "---\ndescription: Portable\n---\nInstructions\n")
	root := filepath.Join(project, ".agents", "skills")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"one", "two"} {
		if err := os.Symlink(p, filepath.Join(root, alias)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(root, filepath.Join(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(external, "secret.md")
	if err := os.WriteFile(secret, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(p, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "safe.md"), []byte("safe"), 0644); err != nil {
		t.Fatal(err)
	}
	c := skills.Discover(project, home)
	page, err := c.List("")
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("aliases = %+v, %v", page, err)
	}
	if v, err := c.View("portable", "safe.md", 0, 0); err != nil || v.Content != "safe" {
		t.Fatalf("safe read = %+v, %v", v, err)
	}
	for _, path := range []string{"../secret.md", "escape.md", secret} {
		if _, err := c.View("portable", path, 0, 0); err == nil {
			t.Errorf("read escaping resource %s", path)
		}
	}
}

func TestMetadataDiagnosticsAndUserOnlyVisibility(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, project, "hidden", "---\ndescription: Private\ndisable-model-invocation: true\nallowed-tools: [bash]\nmetadata:\n  author: Example\n---\nPrivate instructions\n")
	for name, text := range map[string]string{"missing": "---\nname: missing\n---\n", "bool": "---\ndescription: Invalid boolean\ndisable-model-invocation: 'true'\n---\n", "yaml": "---\ndescription: [broken\n---\n"} {
		bundle(t, project, name, text)
	}
	c := skills.Discover(project, home)
	page, err := c.List("")
	if err != nil || len(page.Skills) != 0 {
		t.Fatalf("automatic catalog = %+v, %v", page, err)
	}
	if v, err := c.View("hidden", "", 0, 0); err != nil || !v.UserOnly {
		t.Fatalf("named view = %+v, %v", v, err)
	}
	if len(c.Diagnostics) != 4 {
		t.Fatalf("diagnostics = %v", c.Diagnostics)
	}
	for _, name := range []string{"missing", "bool", "yaml"} {
		if _, err := c.View(name, "", 0, 0); err == nil {
			t.Errorf("invalid %s resolved", name)
		}
	}
}

func TestCatalogPaginationIsStableCompleteAndBounded(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		bundle(t, project, name, "---\ndescription: "+strings.Repeat("x", 1000)+"\n---\n")
	}
	c := skills.Discover(project, home)
	var names []string
	for cursor := ""; ; {
		page, err := c.List(cursor)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(page)
		if len(wire) > skills.CatalogBytes {
			t.Fatalf("oversized page %d", len(wire))
		}
		for _, m := range page.Skills {
			names = append(names, m.Name)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(names) != 40 || names[0] != "skill-00" || names[39] != "skill-39" {
		t.Fatalf("names = %v", names)
	}
	for i, name := range names {
		if name != fmt.Sprintf("skill-%02d", i) {
			t.Fatalf("unstable/duplicate names = %v", names)
		}
	}
	if _, err := c.List("bad"); err == nil {
		t.Fatal("accepted invalid cursor")
	}
}

func TestViewPagesBoundedWireOutputWithoutLosingLines(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	p := bundle(t, project, "paged", "---\ndescription: Paging\n---\nProcedure\n")
	text := strings.Repeat("\t\t\tquoted \"instruction\"\n", 2500)
	if err := os.WriteFile(filepath.Join(p, "reference.md"), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	c := skills.Discover(project, home)
	var read strings.Builder
	for start := 1; ; {
		v, err := c.View("paged", "reference.md", start, 0)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if len(wire) > skills.ViewBytes {
			t.Fatalf("wire page uses %d bytes, limit %d", len(wire), skills.ViewBytes)
		}
		read.WriteString(v.Content)
		if v.NextStart == 0 {
			break
		}
		if v.NextStart <= start {
			t.Fatalf("pagination made no progress: %+v", v)
		}
		start = v.NextStart
	}
	if read.String() != text {
		t.Fatal("paged read lost or repeated content")
	}
}

func TestDiscoverAndReadProjectOverride(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, home, "review", "---\ndescription: Personal review\n---\nPersonal instructions")
	bundle(t, project, "nested/review", "---\ndescription: |\n  Review carefully:\n  include evidence.\n---\nProject instructions")
	c := skills.Discover(project, home)
	page, err := c.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Skills) != 1 || page.Skills[0].Name != "review" || !strings.Contains(page.Skills[0].Description, "include evidence") {
		t.Fatalf("catalog = %+v", page)
	}
	v, err := c.View("review", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Content, "Project instructions") || strings.Contains(v.Content, "Personal instructions") {
		t.Fatalf("view = %+v", v)
	}
	if len(c.Diagnostics) != 1 || !strings.Contains(c.Diagnostics[0], "overrides") {
		t.Fatalf("diagnostics = %v", c.Diagnostics)
	}
}
