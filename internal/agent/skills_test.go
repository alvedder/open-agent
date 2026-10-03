package agent_test

import (
	"context"
	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletedReplacementBodyIsNotReloaded(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write := func(root, body string) {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", "helper")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Helper fixture\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(home, "Original personal helper.")
	original, _, err := agent.PrepareSkillRequest(skills.Discover(project, home), skills.Request{Instructions: "Use /helper"})
	if err != nil {
		t.Fatal(err)
	}
	original.ReferenceContext = "" // reloadable bodies removed by transcript compaction
	write(project, strings.Repeat("Project helper procedure.\n", 1100))
	current := skills.Discover(project, home)
	once, _, err := agent.CompleteSkillRequest(current, original)
	if err != nil {
		t.Fatal(err)
	}
	twice, _, err := agent.CompleteSkillRequest(current, once)
	if err != nil {
		t.Errorf("repeated completion reloaded already-complete replacement: %v", err)
	}
	if err == nil && once.ReferenceContext != twice.ReferenceContext {
		t.Errorf("completion duplicated complete body: %d -> %d", len(once.ReferenceContext), len(twice.ReferenceContext))
	}
}

func TestCompleteHistoricalReplacementDoesNotResolveOrGrantNewEligibility(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	write := func(root, metadata, body string) string {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", "helper")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte("---\ndescription: Fixture\n"+metadata+"---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(home, "", "Personal reference.")
	worker := &agent.Agent{Registry: agent.NewRegistry()}
	worker.ConfigureSkills(skills.Discover(project, home), nil, false)
	if _, err := worker.PrepareInput("Investigate evidence"); err != nil {
		t.Fatal(err)
	}
	read, ok := worker.Registry.Get("skill_view")
	if !ok {
		t.Fatal("skill_view unavailable")
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": "helper"}); err != nil {
		t.Fatal(err)
	}
	original := skills.Request{Instructions: "Continue", WorkflowContext: agent.WorkflowContext(worker.SkillHistory())}
	path := write(project, "", "Replacement historical bytes.")
	loaded, _, err := agent.CompleteSkillRequest(skills.Discover(project, home), original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.WorkflowContext, path) {
		t.Fatal("actual loaded source not recorded")
	}
	// Merge both old and current reminders, as owned worker reads do. The old
	// body is missing; the replacement body is complete historical context.
	loaded.WorkflowContext = agent.MergeSkillSources(original.WorkflowContext, loaded.WorkflowContext)
	write(project, "disable-model-invocation: true\n", "Changed bytes must not refresh history.")
	current := skills.Discover(project, home)
	retained, _, err := agent.CompleteSkillRequest(current, loaded)
	if err != nil || retained.ReferenceContext != loaded.ReferenceContext {
		t.Fatalf("complete replacement history changed: %v", err)
	}
	if strings.Count(retained.WorkflowContext, `"name":"helper"`) != 1 {
		t.Fatal("reconciled sources not deduplicated")
	}
	if _, _, err := agent.CompleteSkillRequest(current, original); err == nil {
		t.Fatal("missing newly user-only body acquired eligibility")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, ".agents", "skills", "helper", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	retained, _, err = agent.CompleteSkillRequest(skills.Discover(project, home), retained)
	if err != nil || retained.ReferenceContext != loaded.ReferenceContext {
		t.Fatalf("complete historical body required current source: %v", err)
	}
}
