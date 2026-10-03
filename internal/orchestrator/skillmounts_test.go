package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/llm"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

func TestRejectedGeneratedPlanningContextDoesNotMountSkill(t *testing.T) {
	for _, consensus := range []bool{false, true} {
		name := "single"
		if consensus {
			name = "consensus"
		}
		t.Run(name, func(t *testing.T) {
			root, home, cli := t.TempDir(), t.TempDir(), t.TempDir()
			t.Chdir(root)
			t.Setenv("HOME", home)
			dir := filepath.Join(root, ".agents", "skills", "helper")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\ndescription: Fixture\n---\nSmall procedure."), 0644); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(cli, "args")
			if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DOCKER_ARGS_FILE", capture)
			tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
			t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
			modelCalls := 0
			model := &inspectSkillModel{inspect: func(_ []llm.Message, _ llm.ChatOptions) (*llm.Response, error) {
				modelCalls++
				return &llm.Response{Message: llm.Message{Role: "assistant", Content: `{"goal":"answer","tasks":[{"id":"answer","goal":"answer","role":"ask","deps":[]}]}`}}, nil
			}}
			d := testDeps(t, model)
			d.PlanModel = "fixture-model"
			request := skills.Request{Instructions: "Use /helper"}
			plan := func(goal string) error {
				if consensus {
					_, err := MakePlanConsensusWithRequest(context.Background(), d, goal, request, 2, nil)
					return err
				}
				_, err := MakePlanWithRequest(context.Background(), d, goal, request)
				return err
			}
			if err := plan(strings.Repeat("g", skills.RequiredContextBytes)); err == nil || !strings.Contains(err.Error(), "context exceeds") {
				t.Fatalf("expected generated context rejection, got %v", err)
			}
			if modelCalls != 0 {
				t.Fatalf("rejected planning context reached model: %d calls", modelCalls)
			}
			shellArgs := func() string {
				t.Helper()
				if _, err := tools.BashExec(context.Background(), "true", 5); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			if args := shellArgs(); strings.Contains(args, "target=/skills/") || strings.Contains(args, "target=/work/.agents/skills/helper") {
				t.Errorf("rejected planner registered bundle: %s", args)
			}
			if err := plan("Answer with the helper"); err != nil {
				t.Fatal(err)
			}
			if args := shellArgs(); !strings.Contains(args, "target=/skills/") || !strings.Contains(args, "target=/work/.agents/skills/helper") {
				t.Errorf("accepted planning context did not register bundle: %s", args)
			}
		})
	}
}
