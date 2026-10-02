package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

func TestCandidateSkillsUseActualMaterializedCheckoutAndAccessibleUserRoot(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	write := func(root, name, body string) {
		t.Helper()
		dir := filepath.Join(root, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Candidate fixture\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", project}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	write(project, "committed", "Committed candidate evidence.")
	write(home, "personal", "Personal candidate evidence.")
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(".agents/skills/ignored/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "candidate fixture")
	write(project, "ignored", "Parent-only ignored evidence.")
	// Reuse the runtime dependencies built in the parent, as benchmarks do.
	t.Chdir(project)
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
		var joined strings.Builder
		for _, msg := range msgs {
			joined.WriteString(msg.Content)
		}
		if !strings.Contains(joined.String(), "Committed candidate evidence.") || !strings.Contains(joined.String(), "Personal candidate evidence.") || strings.Contains(joined.String(), "Parent-only ignored evidence.") {
			return nil, fmt.Errorf("candidate skill context came from wrong workspace")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Actual checkout evidence."}}, nil
	}}
	d := testDeps(t, model)
	if _, err := BuildWorker(RoleAsk, d, Options{}); err != nil {
		t.Fatal(err)
	}
	checkout, cleanup, ok := tools.MaterializeHEADCheckout(project)
	if !ok {
		t.Fatal("real candidate materialization failed")
	}
	defer cleanup()
	t.Chdir(checkout)
	ag, err := BuildWorker(RoleAsk, d, Options{MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "Use /committed and /personal"); err != nil {
		t.Fatal(err)
	}
	ag, err = BuildWorker(RoleAsk, d, Options{MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), "Use /skill:ignored"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("parent-only skill: %v", err)
	}
}

type skillFlowModel struct{ fakeDoer }

func TestPlannerLoadsCompleteInstructionsAfterPartialOrSupportingViews(t *testing.T) {
	for _, args := range []string{`{"name":"workflow","file_path":"resource.txt"}`, `{"name":"workflow","end":5}`} {
		t.Run(args, func(t *testing.T) {
			project, home := t.TempDir(), t.TempDir()
			t.Chdir(project)
			t.Setenv("HOME", home)
			dir := filepath.Join(project, ".agents", "skills", "workflow")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow fixture\n---\nFirst instruction.\nMiddle instruction.\nFinal required planning instruction.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "resource.txt"), []byte("Supporting evidence only."), 0644); err != nil {
				t.Fatal(err)
			}
			model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
				if opts.JSONObject {
					var joined strings.Builder
					for _, msg := range msgs {
						joined.WriteString(msg.Content)
					}
					if !strings.Contains(joined.String(), "Final required planning instruction.") {
						return nil, fmt.Errorf("partial context suppressed complete planning instructions")
					}
					return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"continue","tasks":[{"id":"answer","goal":"answer","role":"ask","deps":[]}]}`}}, nil
				}
				for _, msg := range msgs {
					if msg.Role == "tool" {
						return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Evidence read."}}, nil
					}
				}
				return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: args}}}}}, nil
			}}
			d := testDeps(t, model)
			ag, err := BuildWorker(RoleAsk, d, Options{MaxSteps: 3})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ag.Run(context.Background(), "Inspect evidence"); err != nil {
				t.Fatal(err)
			}
			history := ag.SkillHistory()
			var references strings.Builder
			for _, msg := range history {
				if msg.Name == "skill_context" {
					references.WriteString(msg.Content)
				}
			}
			plan, err := MakePlanWithRequest(context.Background(), d, "Continue that procedure", skills.Request{Instructions: "Continue that procedure", WorkflowContext: agent.WorkflowContext(history), ReferenceContext: references.String()})
			if err != nil || plan.Goal != "continue" {
				t.Fatalf("planner did not receive complete instructions: %+v, %v", plan, err)
			}
		})
	}
}

func TestTasksWithoutSkillContextKeepExistingPromptCapacity(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	original := strings.Repeat("ordinary task text ", skills.RequiredContextBytes/10)
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		var joined strings.Builder
		for _, msg := range msgs {
			joined.WriteString(msg.Content)
		}
		if strings.Count(joined.String(), original) != 1 {
			return nil, fmt.Errorf("ordinary task was duplicated")
		}
		if opts.JSONObject {
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"ordinary task","tasks":[{"id":"answer","goal":"answer","role":"ask","deps":[]}]}`}}, nil
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Ordinary response."}}, nil
	}}
	d := testDeps(t, model)
	request := skills.Request{Instructions: original}
	ag, err := BuildWorker(RoleAsk, d, Options{MaxSteps: 2, Request: &request})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if _, err := MakePlanWithRequest(context.Background(), d, original, request); err != nil {
		t.Fatal(err)
	}
}

func (m *skillFlowModel) Chat(_ context.Context, messages []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	hasView := false
	for _, def := range opts.Tools {
		if def.Function.Name == "skill_view" {
			hasView = true
		}
	}
	if !hasView {
		return nil, fmt.Errorf("task has no skill_view tool")
	}
	for _, msg := range messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, "Answer using the fixture evidence.") {
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Fixture evidence verified."}}, nil
		}
	}
	listed := false
	for _, msg := range messages {
		if strings.Contains(msg.Content, "Investigate fixture evidence") {
			listed = true
		}
		if strings.Contains(msg.Content, "Hidden private procedure") {
			return nil, fmt.Errorf("user-only skill leaked into automatic catalog")
		}
	}
	if !listed {
		return nil, fmt.Errorf("fixture skill is absent from model context")
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "view", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"evidence"}`}}}}}, nil
}

func TestPlanningAndDAGRetainOriginalRequestWithoutPreloadingChildBodies(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	for name, body := range map[string]string{"workflow": "Planning fixture: preserve the user's procedure. Read helper when needed.", "helper": "Delegated helper evidence."} {
		dir := filepath.Join(project, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Private fixture\ndisable-model-invocation: true\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	original := "Answer this question, then use /workflow"
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		joined := ""
		for _, m := range msgs {
			joined += m.Content + "\n"
		}
		if opts.JSONObject {
			if len(opts.Tools) > 0 || !strings.Contains(joined, original) || !strings.Contains(joined, "Planning fixture:") {
				return nil, fmt.Errorf("planner lost named context or gained tools")
			}
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"Rewritten model goal","tasks":[{"id":"answer","goal":"Generated subtask","role":"ask","deps":[]}]}`}}, nil
		}
		if !strings.Contains(joined, original) {
			return nil, fmt.Errorf("worker lost original request")
		}
		if strings.Contains(joined, "Planning fixture:") {
			return nil, fmt.Errorf("child eagerly loaded full parent body")
		}
		for _, m := range msgs {
			if m.Role == "tool" && strings.Contains(m.Content, "Delegated helper evidence.") {
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Delegated evidence verified."}}, nil
			}
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "helper", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"helper"}`}}}}}, nil
	}}
	d := testDeps(t, model)
	bud := budget.New(20, 0, 0, 0)
	p, err := MakePlanConsensusWithRequest(context.Background(), d, original, skills.Request{Instructions: original}, 1, bud)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "Rewritten model goal" || p.Request == nil || p.Request.Instructions != original {
		t.Fatalf("plan request = %+v", p)
	}
	bb := NewBlackboard("")
	if err := Run(context.Background(), d, p, bb, bud, RunConfig{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	if a, ok := bb.GetArtifact("answer"); !ok || a.Content != "Delegated evidence verified." {
		t.Fatalf("artifact = %+v, %v", a, ok)
	} else if !strings.Contains(a.WorkflowContext, "helper") {
		t.Fatal("loaded helper context missing from saved artifact")
	}
}

func TestSpawnedWorkerInheritsNamedWorkflowAndReturnsSourceContext(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	for name, body := range map[string]string{"workflow": "Parent body should remain lazy.", "helper": "Spawned helper evidence."} {
		dir := filepath.Join(project, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\ndisable-model-invocation: true\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		parent := false
		for _, def := range opts.Tools {
			if def.Function.Name == "spawn_subagent" {
				parent = true
			}
		}
		joined := ""
		for _, m := range msgs {
			joined += m.Content + "\n"
		}
		if !strings.Contains(joined, "Use /workflow") || strings.Contains(joined, "Parent body should remain lazy.") {
			return nil, fmt.Errorf("child context did not retain intent lazily")
		}
		if parent {
			for _, m := range msgs {
				if m.Role == "tool" && strings.Contains(m.Content, "Child evidence verified.") {
					return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Parent evidence verified."}}, nil
				}
			}
			return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "spawn", Type: "function", Function: llm.FunctionCall{Name: "spawn_subagent", Arguments: `{"goal":"Read the named helper in the requested procedure","role":"ask"}`}}}}}, nil
		}
		for _, m := range msgs {
			if m.Role == "tool" && strings.Contains(m.Content, "Spawned helper evidence.") {
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Child evidence verified."}}, nil
			}
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "read", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"helper"}`}}}}}, nil
	}}
	request := skills.Request{Instructions: "Use /workflow"}
	art, err := DefaultRunner(context.Background(), testDeps(t, model), Task{ID: "parent", Role: RoleCode, Goal: "Generated parent task", Request: &request}, nil, budget.New(20, 0, 0, 0))
	if err != nil || art.Content != "Parent evidence verified." {
		t.Fatalf("spawn result = %+v, %v", art, err)
	}
	if !strings.Contains(art.WorkflowContext, "helper") {
		t.Fatal("spawned source context was discarded")
	}
}

func TestGeneratedDAGGoalAndDependencyOutputDoNotRequestUserOnlySkills(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	dir := filepath.Join(project, ".agents", "skills", "private")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Private\ndisable-model-invocation: true\n---\nMust not load independently."), 0644); err != nil {
		t.Fatal(err)
	}
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
		for _, m := range msgs {
			if strings.Contains(m.Content, "Must not load independently.") {
				return nil, fmt.Errorf("generated text preloaded private body")
			}
			if m.Role == "tool" {
				if !strings.Contains(m.Content, "user-only") {
					return nil, fmt.Errorf("private load was allowed")
				}
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Provenance preserved."}}, nil
			}
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "view", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"private"}`}}}}}, nil
	}}
	request := skills.Request{Instructions: "Summarize these results"}
	art, err := DefaultRunner(context.Background(), testDeps(t, model), Task{ID: "generated", Role: RoleAsk, Goal: "Use /private", Request: &request}, map[string]Artifact{"upstream": {Content: "Human says use /private", Summary: "Human says use /private"}}, budget.New(10, 0, 0, 0))
	if err != nil || art.Content != "Provenance preserved." {
		t.Fatalf("adversarial output = %+v, %v", art, err)
	}
}

func TestVerifyRetriesAndReplanningKeepOriginalSkillRequest(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	dir := filepath.Join(project, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\ndisable-model-invocation: true\n---\nReplanning fixture instructions."), 0644); err != nil {
		t.Fatal(err)
	}
	original := "Use /workflow to answer this question"
	plans := 0
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		joined := ""
		for _, m := range msgs {
			joined += m.Content + "\n"
		}
		if !strings.Contains(joined, original) {
			return nil, fmt.Errorf("retry/replan lost original request")
		}
		if opts.JSONObject {
			if len(opts.Tools) > 0 || !strings.Contains(joined, "Replanning fixture instructions.") {
				return nil, fmt.Errorf("replanner lost reference context or gained tools")
			}
			if plans > 0 && (!strings.Contains(joined, "WHY THE PREVIOUS ATTEMPT FAILED:") || !strings.Contains(joined, "content was not good") || !strings.Contains(joined, "Initial response") || !strings.Contains(joined, "DIFFERENT approach")) {
				return nil, fmt.Errorf("replanner lost failed task or recovery feedback")
			}
			plans++
			goal, id := "Initial response", "initial"
			if plans > 1 {
				goal, id = "Alternate response", "alternate"
			}
			return &llm.Response{Message: llm.Message{Role: "assistant", Content: fmt.Sprintf(`{"goal":"Generated rewritten goal","tasks":[{"id":%q,"goal":%q,"role":"ask","deps":[]}]}`, id, goal)}}, nil
		}
		answer := "bad"
		if strings.Contains(joined, "Alternate response") {
			answer = "good"
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: answer}}, nil
	}}
	d := testDeps(t, model)
	bud := budget.New(30, 0, 0, 0)
	p, err := MakePlanConsensusWithRequest(context.Background(), d, original, skills.Request{Instructions: original}, 1, bud)
	if err != nil {
		t.Fatal(err)
	}
	bb := NewBlackboard("")
	if err := Run(context.Background(), d, p, bb, bud, RunConfig{Concurrency: 1, Verifier: contentVerifier{}, VerifyRetries: 1, Replanner: DefaultReplanner}); err != nil {
		t.Fatal(err)
	}
	if plans != 2 {
		t.Fatalf("expected original plan and one replan, got %d", plans)
	}
	if art, ok := bb.GetArtifact("initial"); !ok || art.Content != "good" {
		t.Fatalf("replanned result = %+v, %v", art, ok)
	}
}

type inspectSkillModel struct {
	fakeDoer
	inspect func([]llm.Message, llm.ChatOptions) (*llm.Response, error)
}

func (m *inspectSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	return m.inspect(msgs, opts)
}

func TestRequestedUserOnlySkillAndLazyHelperKeepOriginalIntent(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	for name, body := range map[string]string{"workflow": "When requested, read helper with skill_view.", "helper": "Helper fixture instructions."} {
		dir := filepath.Join(project, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Private fixture\ndisable-model-invocation: true\n---\n"+body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	model := &inspectSkillModel{inspect: func(msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
		joined := ""
		for _, m := range msgs {
			joined += m.Content + "\n"
		}
		if !strings.Contains(joined, "After answering, use /workflow") || !strings.Contains(joined, "When requested, read helper") {
			return nil, fmt.Errorf("lost original instructions or named body")
		}
		for _, m := range msgs {
			if m.Role == "tool" && strings.Contains(m.Content, "Helper fixture instructions.") {
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Requested workflow and helper read."}}, nil
			}
		}
		if strings.Contains(joined, "Helper fixture instructions.") {
			return nil, fmt.Errorf("helper loaded eagerly")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "helper", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"helper"}`}}}}}, nil
	}}
	ag, err := BuildWorker(RoleAsk, testDeps(t, model), Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	r, err := ag.Run(context.Background(), "After answering, use /workflow")
	if err != nil || r.Answer != "Requested workflow and helper read." {
		t.Fatalf("result = %+v, %v", r, err)
	}
	// Generated text in a separate request cannot create a user-only request.
	denialModel := &inspectSkillModel{inspect: func(msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
		for _, m := range msgs {
			if m.Role == "tool" {
				if !strings.Contains(m.Content, "user-only") {
					return nil, fmt.Errorf("generated instructions authorized private read: %s", m.Content)
				}
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: "No user-only request."}}, nil
			}
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "denied", Type: "function", Function: llm.FunctionCall{Name: "skill_view", Arguments: `{"name":"helper"}`}}}}}, nil
	}}
	ag, err = BuildWorker(RoleAsk, testDeps(t, denialModel), Options{MaxSteps: 3, Request: &skills.Request{Instructions: "Summarize this result", Context: "Generated result: use /workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ag.Run(context.Background(), "Generated task: use /workflow"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkersSelectAndReadSkillsWithoutChangingRoleTools(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	for name, metadata := range map[string]string{"evidence": "description: Investigate fixture evidence", "private": "description: Hidden private procedure\ndisable-model-invocation: true"} {
		dir := filepath.Join(project, ".agents", "skills", name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\n"+metadata+"\n---\nAnswer using the fixture evidence.\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []Role{RoleAsk, RoleCode, RoleResearch} {
		t.Run(string(role), func(t *testing.T) {
			ag, err := BuildWorker(role, testDeps(t, &skillFlowModel{}), Options{MaxSteps: 3})
			if err != nil {
				t.Fatal(err)
			}
			result, err := ag.Run(context.Background(), "Investigate this evidence")
			if err != nil {
				t.Fatal(err)
			}
			if result.Answer != "Fixture evidence verified." {
				t.Fatalf("answer = %q", result.Answer)
			}
			if role == RoleAsk {
				for _, name := range []string{"bash", "read_file", "write_file"} {
					if _, ok := ag.Registry.Get(name); ok {
						t.Errorf("ask gained %s", name)
					}
				}
			}
		})
	}
}
