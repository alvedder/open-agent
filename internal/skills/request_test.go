package skills_test

import (
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/skills"
)

func TestRequestsAnywhereComposeDistinctReferenceBlocksInOrder(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for _, name := range []string{"review", "refactor", "help"} {
		bundle(t, project, name, "---\ndescription: "+name+" procedure\ndisable-model-invocation: true\n---\n"+name+" instructions\n")
	}
	c := skills.Discover(project, home)
	for _, text := range []string{"/review examine changes", "examine /review changes", "examine changes /review", "Please use review after editing", "Explain review; do not use it"} {
		p, err := c.Prepare(skills.Request{Instructions: text, Context: "generated /refactor"})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Names) != 1 || p.Names[0] != "review" || p.Instructions != text {
			t.Fatalf("request %q = %+v", text, p)
		}
		if !strings.Contains(p.Reference, "reference context") || !strings.Contains(p.Reference, "review instructions") {
			t.Fatalf("reference = %q", p.Reference)
		}
	}
	p, err := c.Prepare(skills.Request{Instructions: "/refactor then /skill:review and /refactor"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Names, ",") != "refactor,review" || strings.Index(p.Reference, "refactor instructions") > strings.Index(p.Reference, "review instructions") {
		t.Fatalf("ordered request = %+v", p)
	}
	for _, text := range []string{"https://site/review", "./review.md", "/tmp/review", "/review.md", "/help"} {
		p, err := c.Prepare(skills.Request{Instructions: text})
		if err != nil || len(p.Names) != 0 {
			t.Fatalf("path/builtin %q = %+v, %v", text, p, err)
		}
	}
	p, err = c.Prepare(skills.Request{Instructions: "/skill:help"})
	if err != nil || len(p.Names) != 1 || p.Names[0] != "help" {
		t.Fatalf("namespaced collision = %+v, %v", p, err)
	}
	if _, err := c.Prepare(skills.Request{Instructions: "After editing, /skill:missing"}); err == nil {
		t.Fatal("missing explicit skill accepted")
	}
	p, err = c.Prepare(skills.Request{Instructions: "ordinary task", Context: "Use /review"})
	if err != nil || len(p.Names) != 0 {
		t.Fatalf("generated context became user request = %+v, %v", p, err)
	}
}

func TestRequiredNamedContextFailsInsteadOfSilentlyTruncating(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	bundle(t, project, "large", "---\ndescription: Large\n---\n"+strings.Repeat("A procedure line.\n", 4000))
	_, err := skills.Discover(project, home).Prepare(skills.Request{Instructions: "Use /large"})
	if err == nil || !strings.Contains(err.Error(), "required named skill context exceeds") {
		t.Fatalf("context limit = %v", err)
	}
}

func TestAcceptedNonconventionalNamesKeepExplicitNamespacedInvocation(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	for _, name := range []string{"review.v2", "review:helper", "révision", "?"} {
		bundle(t, project, name, "---\ndescription: Fixture\n---\nFixture instructions.")
	}
	catalog := skills.Discover(project, home)
	for _, name := range []string{"review.v2", "review:helper", "révision", "?"} {
		for _, prompt := range []string{"After editing, use /skill:" + name, "After editing, use (`/skill:" + name + "`)."} {
			prepared, err := catalog.Prepare(skills.Request{Instructions: prompt})
			if err != nil || len(prepared.Names) != 1 || prepared.Names[0] != name {
				t.Fatalf("namespaced %s: %+v, %v", name, prepared, err)
			}
		}
	}
}
