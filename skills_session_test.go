package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/orchestrator"
	"github.com/imhassla/open-agent/internal/telemetry"
)

type sessionSkillModel struct{ seen [][]llm.Message }

func (d *sessionSkillModel) Chat(_ context.Context, msgs []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
	d.seen = append(d.seen, append([]llm.Message(nil), msgs...))
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Turn completed."}}, nil
}
func (d *sessionSkillModel) ChatStream(ctx context.Context, msgs []llm.Message, opts llm.ChatOptions, _ llm.StreamHandler) (*llm.Response, error) {
	return d.Chat(ctx, msgs, opts)
}
func (d *sessionSkillModel) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, fmt.Errorf("no embedding")
}

func TestSkillConversationPersistsHistoryWithoutAutomaticRefreshAndResetSaves(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	text := "---\ndescription: Workflow fixture\ndisable-model-invocation: true\n---\nHistorical fixture instructions.\n"
	if err := os.WriteFile(file, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	model := &sessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "telemetry.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}}
	s.converse(context.Background(), orchestrator.RoleAsk, "Explain /workflow without applying it")
	state, ok := loadSession()
	if !ok {
		t.Fatal("conversation not saved")
	}
	joined := ""
	for _, m := range state.History {
		joined += m.Content
	}
	if !strings.Contains(joined, "Historical fixture instructions.") {
		t.Fatal("named skill context discarded by session fold")
	}
	if err := os.WriteFile(file, []byte(strings.ReplaceAll(text, "Historical", "Changed")), 0644); err != nil {
		t.Fatal(err)
	}
	resumed := &session{deps: deps, opts: options{noStream: true}, history: state.History}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Continue explaining it; do not apply the procedure")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Historical fixture instructions.") || strings.Contains(joined, "Changed fixture instructions.") {
		t.Fatal("history was lost or refreshed without an actual load")
	}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Stop using that procedure") {
		t.Fatal("stop instruction lost")
	}
	resumed.converse(context.Background(), orchestrator.RoleAsk, "Explain /workflow again")
	joined = ""
	for _, m := range model.seen[len(model.seen)-1] {
		joined += m.Content
	}
	if !strings.Contains(joined, "Changed fixture instructions.") {
		t.Fatal("another actual load did not read current bytes")
	}
	resumed.slash("/reset")
	if state, ok := loadSession(); ok || len(state.History) > 0 {
		t.Fatal("reset resurrects history after immediate restart")
	}
}

type overflowingSkillModel struct {
	sessionSkillModel
	overflowed bool
}

func (d *overflowingSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	if len(opts.Tools) == 0 {
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Lossy summary omitting skill sources and stop instructions."}}, nil
	}
	if !d.overflowed {
		d.overflowed = true
		return nil, &llm.APIError{Kind: llm.ErrContextLength, Status: 400, Body: "maximum context length"}
	}
	reminder := agent.WorkflowContext(msgs)
	if !strings.Contains(reminder, "workflow") || !strings.Contains(reminder, "Stop using that procedure") {
		return nil, fmt.Errorf("worker compaction lost context: %s", reminder)
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "Compacted workflow context retained."}}, nil
}

func TestWorkerCompactionAndRewindRestoreCorrespondingSkillContext(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.test"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\n---\nFixture instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "fixture"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	deps := &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true}, cpStore: newCheckpointStore()}
	if !s.cpStore.enabled {
		t.Fatalf("checkpoints disabled: %s", s.cpStore.why)
	}
	s.snapshotBaseline()
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
	s.checkpointTurn() // before stopping
	s.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
	history := append(append([]llm.Message(nil), s.history...), bigHistory(12)...)
	deps.Client = &overflowingSkillModel{}
	w, err := orchestrator.BuildWorker(orchestrator.RoleAsk, deps, orchestrator.Options{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	w.LoadHistory(history)
	r, err := w.Send(context.Background(), "Explain the remaining context")
	if err != nil || r.Answer != "Compacted workflow context retained." {
		t.Fatalf("compaction result = %+v, %v", r, err)
	}
	s.rewind("1 chat")
	reminder := agent.WorkflowContext(s.history)
	if !strings.Contains(reminder, "Use /workflow") || strings.Contains(reminder, "Stop using that procedure") {
		t.Fatalf("rewind restored wrong workflow context: %s", reminder)
	}
	st, ok := loadSession()
	if !ok || agent.WorkflowContext(st.History) != reminder {
		t.Fatal("rewind did not persist corresponding context")
	}
}

type planningSessionSkillModel struct {
	sessionSkillModel
	plans int
}

func (d *planningSessionSkillModel) Chat(_ context.Context, msgs []llm.Message, opts llm.ChatOptions) (*llm.Response, error) {
	joined := ""
	for _, m := range msgs {
		joined += m.Content + "\n"
	}
	if opts.JSONObject && strings.Contains(msgs[0].Content, "strict, independent reviewer") {
		if len(opts.Tools) > 0 || strings.Contains(joined, "Local skills") || strings.Contains(joined, "Historical planner fixture") {
			return nil, fmt.Errorf("judge acquired skill context")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"pass":true,"feedback":"Adequate fixture answer."}`}}, nil
	}
	if opts.JSONObject {
		d.plans++
		if len(opts.Tools) > 0 || !strings.Contains(joined, "Historical planner fixture") || !strings.Contains(joined, "Use /workflow") || !strings.Contains(joined, "Continue that procedure") {
			return nil, fmt.Errorf("interactive planner lost continuing context")
		}
		return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"Planner rewritten goal","tasks":[{"id":"answer","goal":"Explain fixture","role":"ask","deps":[]}]}`}}, nil
	}
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "A complete fixture answer."}}, nil
}

func TestInteractiveDoAndSavedRunKeepOriginalSkillContextAndIndependentJudge(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(file, []byte("---\ndescription: Workflow\ndisable-model-invocation: true\n---\nHistorical planner fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	model := &planningSessionSkillModel{}
	deps := &orchestrator.Deps{Client: model, Tlog: telemetry.Open(filepath.Join(home, "t.jsonl"))}
	s := &session{deps: deps, opts: options{noStream: true, maxSteps: 20}, render: newSessionRenderer(io.Discard)}
	s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
	s.orchestrate(context.Background(), "Continue that procedure")
	if model.plans != 1 || s.lastRun == "" {
		t.Fatalf("orchestration did not execute: plans=%d run=%s", model.plans, s.lastRun)
	}
	path := filepath.Join(home, ".open-agent", "runs", s.lastRun, "plan.json")
	p, err := loadPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Goal != "Planner rewritten goal" || p.Request == nil || p.Request.Instructions != "Continue that procedure" || !strings.Contains(p.Request.ReferenceContext, "Historical planner fixture") {
		t.Fatalf("saved request = %+v", p)
	}
	if err := os.WriteFile(file, []byte("---\ndescription: Workflow\n---\nChanged planner fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	resumed, bb, _, _, _, err := buildOrResumePlan(context.Background(), deps, "Ignore this new argument", options{resume: s.lastRun})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Request.Instructions != "Continue that procedure" || !strings.Contains(resumed.Request.ReferenceContext, "Historical planner fixture") || strings.Contains(resumed.Request.ReferenceContext, "Changed planner fixture") {
		t.Fatal("resume lost original request or refreshed historical bytes")
	}
	if _, ok := bb.GetArtifact("answer"); !ok {
		t.Fatal("resume lost completed artifact")
	}
	if model.plans != 1 {
		t.Fatal("resume invoked planner again")
	}
}

func TestSessionCompactionRetainsSkillSourcesAndLaterStopEvenIfSummaryOmitsThem(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", home)
	dir := filepath.Join(root, ".agents", "skills", "workflow")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Workflow\n---\nFixture instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		s := &session{deps: &orchestrator.Deps{Client: &sessionSkillModel{}, Tlog: telemetry.Open(filepath.Join(t.TempDir(), "t.jsonl"))}, opts: options{noStream: true}}
		s.converse(context.Background(), orchestrator.RoleAsk, "Use /workflow")
		s.converse(context.Background(), orchestrator.RoleAsk, "Stop using that procedure")
		s.history = append(s.history, bigHistory(40)...)
		d := &summarizeDoer{summary: "A summary omitting skill names and stop instructions."}
		if failure {
			d.err = fmt.Errorf("summary unavailable")
		}
		s.deps.Client = d
		s.compactHistory()
		context := agent.WorkflowContext(s.history)
		if !strings.Contains(context, "workflow") || !strings.Contains(context, "Stop using that procedure") || !strings.Contains(context, dir) {
			t.Fatalf("compaction lost deterministic context (failure=%v): %s", failure, context)
		}
		if historyChars(s.history) > historyBudget {
			t.Fatalf("history exceeds budget: %d", historyChars(s.history))
		}
		for _, m := range s.history {
			if m.Role == "tool" || len(m.ToolCalls) > 0 {
				t.Fatal("folded history contains orphanable tool protocol")
			}
		}
	}
}
