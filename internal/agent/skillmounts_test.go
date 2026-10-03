package agent_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/skills"
	"github.com/imhassla/open-agent/internal/tools"
)

type skillMountFixture struct {
	root, home, capture string
}

func newSkillMountFixture(t *testing.T) skillMountFixture {
	t.Helper()
	f := skillMountFixture{root: t.TempDir(), home: t.TempDir()}
	t.Chdir(f.root)
	cli := t.TempDir()
	f.capture = filepath.Join(cli, "args")
	if err := os.WriteFile(filepath.Join(cli, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cli+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_ARGS_FILE", f.capture)
	tools.SetSandbox(tools.DockerSandbox{Image: "fixture:local"})
	t.Cleanup(func() { tools.SetSandbox(tools.HostSandbox{}) })
	return f
}

func (f skillMountFixture) add(t *testing.T, folder, name string) {
	t.Helper()
	dir := filepath.Join(f.root, ".agents", "skills", folder)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Fixture\n---\nSmall procedure."), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f skillMountFixture) overlays(t *testing.T) map[string]bool {
	t.Helper()
	if _, err := tools.BashExec(context.Background(), "true", 5); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	overlays := make(map[string]bool)
	for i, arg := range args {
		if arg != "--mount" || i+1 == len(args) {
			continue
		}
		fields, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range fields {
			if strings.HasPrefix(field, "target=") {
				overlays[strings.TrimPrefix(field, "target=")] = true
			}
		}
	}
	return overlays
}

func TestRejectedSkillViewLeavesOnlyPreviouslyAcceptedDockerMounts(t *testing.T) {
	f := newSkillMountFixture(t)
	f.add(t, "accepted", "accepted")
	f.add(t, "rejected", "rejected")
	reg := agent.NewRegistry()
	agent.RegisterSkills(reg, skills.Discover(f.root, f.home), nil, func(v skills.View) error {
		if v.Name == "rejected" {
			return errors.New("fixture context rejection")
		}
		return nil
	})
	read, _ := reg.Get("skill_view")
	data, err := read.Handler(context.Background(), map[string]any{"name": "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	var accepted skills.View
	if err := json.Unmarshal([]byte(data), &accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": "rejected"}); err == nil {
		t.Fatal("rejected read succeeded")
	}
	mounts := f.overlays(t)
	if mounts["/work/.agents/skills/rejected"] || len(mounts) != 3 {
		t.Errorf("rejected read changed later Docker mounts: %v", mounts)
	}
	if !mounts[accepted.ExecutionDir] || !mounts["/work/.agents/skills/accepted"] || !mounts["/work"] {
		t.Errorf("previously accepted mounts were removed: %v", mounts)
	}
}

func TestAutomaticSkillContextRejectionDoesNotMountNewBundle(t *testing.T) {
	f := newSkillMountFixture(t)
	first, second := "first-"+strings.Repeat("a", 5000), "second-"+strings.Repeat("b", 5000)
	f.add(t, "first", first)
	f.add(t, "second", second)
	worker := &agent.Agent{Registry: agent.NewRegistry()}
	worker.ConfigureSkills(skills.Discover(f.root, f.home), nil, false)
	if _, err := worker.PrepareInput(strings.Repeat("ordinary ", 3888)); err != nil {
		t.Fatal(err)
	}
	read, _ := worker.Registry.Get("skill_view")
	if _, err := read.Handler(context.Background(), map[string]any{"name": first}); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Handler(context.Background(), map[string]any{"name": second}); err == nil || !strings.Contains(err.Error(), "context exceeds") {
		t.Fatalf("expected context rejection, got %v", err)
	}
	if strings.Contains(agent.WorkflowContext(worker.SkillHistory()), second) {
		t.Fatal("rejected context persisted its new source")
	}
	mounts := f.overlays(t)
	if mounts["/work/.agents/skills/second"] || len(mounts) != 3 {
		t.Errorf("context rejection changed later Docker mounts: %v", mounts)
	}
	if !mounts["/work/.agents/skills/first"] {
		t.Error("accepted source mount was removed")
	}
}

func TestFailedSkillPreparationDoesNotMountBundles(t *testing.T) {
	for _, mode := range []string{"worker", "planner"} {
		t.Run(mode, func(t *testing.T) {
			f := newSkillMountFixture(t)
			f.add(t, "helper", "helper")
			catalog := skills.Discover(f.root, f.home)
			request := skills.Request{Instructions: "Use /helper", Context: strings.Repeat("g", skills.RequiredContextBytes)}
			var err error
			var worker *agent.Agent
			if mode == "worker" {
				worker = &agent.Agent{Registry: agent.NewRegistry()}
				worker.ConfigureSkills(catalog, &request, false)
				_, err = worker.PrepareInput(request.Instructions)
			} else {
				_, _, err = agent.PrepareSkillRequest(catalog, request)
			}
			if err == nil || !strings.Contains(err.Error(), "context exceeds") {
				t.Fatalf("expected context rejection, got %v", err)
			}
			mounts := f.overlays(t)
			if len(mounts) != 1 || !mounts["/work"] {
				t.Errorf("failed preparation changed later Docker mounts: %v", mounts)
			}
			// A fitting retry must still register the accepted bundle, including
			// its read-only overlay within the writable project mount.
			request.Context = ""
			var accepted string
			if mode == "worker" {
				accepted, err = worker.PrepareInput(request.Instructions)
			} else {
				_, accepted, err = agent.PrepareSkillRequest(catalog, request)
			}
			if err != nil {
				t.Fatal(err)
			}
			mounts = f.overlays(t)
			if len(mounts) != 3 || !mounts["/work/.agents/skills/helper"] {
				t.Errorf("successful retry did not register its bundle: %v", mounts)
			}
			for target := range mounts {
				if strings.HasPrefix(target, "/skills/") && !strings.Contains(accepted, target) {
					t.Errorf("accepted context omitted execution directory %s", target)
				}
			}
		})
	}
}
