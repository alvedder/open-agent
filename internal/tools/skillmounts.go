package tools

import (
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type skillMounts struct {
	mu      sync.RWMutex
	bundles map[string]string // canonical source → stable execution directory
}

// MountSkillBundle registers one loaded bundle for subsequent shell invocations.
// This does not run Docker, execute the skill or provision dependencies. Ordinary
// host execution uses the real bundle path; whole-agent sandboxes remain guest-local.
func MountSkillBundle(base string) (string, error) {
	if docker, ok := active.(interface{ mountSkill(string) (string, error) }); ok {
		return docker.mountSkill(base)
	}
	return base, nil
}

func (d DockerSandbox) mountSkill(base string) (string, error) {
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skill bundle must be a directory")
	}
	if d.mounts == nil {
		return "", fmt.Errorf("Docker sandbox skill mounts were not initialized")
	}
	d.mounts.mu.Lock()
	defer d.mounts.mu.Unlock()
	if alias, ok := d.mounts.bundles[canonical]; ok {
		return alias, nil
	}
	id := sha256.Sum256([]byte(canonical))
	alias := fmt.Sprintf("/skills/%x", id)
	d.mounts.bundles[canonical] = alias
	return alias, nil
}

func bindMount(source, target string, readonly bool) string {
	fields := []string{"type=bind", "source=" + source, "target=" + target}
	if readonly {
		fields = append(fields, "readonly")
	}
	var text strings.Builder
	writer := csv.NewWriter(&text)
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(text.String(), "\n")
}

func (d DockerSandbox) skillMountArgs(work string) []string {
	if d.mounts == nil {
		return []string{"--mount", bindMount(work, "/work", false)}
	}
	d.mounts.mu.RLock()
	defer d.mounts.mu.RUnlock()
	var sources []string
	for source := range d.mounts.bundles {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	workReadonly := false
	for _, source := range sources {
		if relative, err := filepath.Rel(source, work); err == nil && insideBundle(relative) {
			workReadonly = true
		}
	}
	args := []string{"--mount", bindMount(work, "/work", workReadonly)}
	for _, source := range sources {
		args = append(args, "--mount", bindMount(source, d.mounts.bundles[source], true))
		rel, err := filepath.Rel(work, source)
		if err == nil && rel != "." && insideBundle(rel) {
			target := filepath.ToSlash(filepath.Join("/work", rel))
			args = append(args, "--mount", bindMount(source, target, true))
		}
	}
	return args
}

func insideBundle(relative string) bool {
	return !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
