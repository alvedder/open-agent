package orchestrator

import (
	"fmt"
	"os"

	"github.com/imhassla/open-agent/internal/agent"
	"github.com/imhassla/open-agent/internal/skills"
)

func preparePlanningRequest(request skills.Request) (skills.Request, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return skills.Request{}, "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return skills.Request{}, "", err
	}
	catalog := skills.Discover(cwd, home)
	for _, diagnostic := range catalog.Diagnostics {
		fmt.Fprintf(os.Stderr, "skills: %s\n", diagnostic)
	}
	return agent.PrepareSkillRequest(catalog, request)
}
