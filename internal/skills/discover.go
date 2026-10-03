// Package skills discovers portable local instruction bundles and reads them
// without executing code or installing dependencies.
package skills

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Metadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	UserOnly    bool   `json:"user_only,omitempty"`
	Scope       string `json:"scope"`
	Source      string `json:"source"`
	BaseDir     string `json:"base_dir"`
}

type Catalog struct {
	Diagnostics []string
	entries     map[string][]Metadata
}

// Discover uses the nearest .git directory/file above cwd, or cwd outside Git,
// and the user's home. Catalogs belong to a workspace, not a shared model client.
func Discover(cwd, home string) *Catalog {
	c := &Catalog{entries: make(map[string][]Metadata)}
	root, err := filepath.Abs(cwd)
	if err != nil {
		c.note("working directory: %v", err)
		return c
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	seen := make(map[string]bool)
	project := c.walk(filepath.Join(root, ".agents", "skills"), "project", seen)
	personal := make(map[string][]Metadata)
	if home != "" {
		personal = c.walk(filepath.Join(home, ".agents", "skills"), "user", seen)
	}
	for name, entries := range personal {
		c.entries[name] = entries
	}
	for name, entries := range project {
		if len(personal[name]) > 0 {
			c.note("project skill %q overrides user skill", name)
		}
		c.entries[name] = entries
	}
	for _, name := range c.names() {
		if len(c.entries[name]) > 1 {
			c.note("skill %q is ambiguous in %s scope (%d bundles)", name, c.entries[name][0].Scope, len(c.entries[name]))
		}
	}
	sort.Strings(c.Diagnostics)
	return c
}

func (c *Catalog) note(format string, args ...any) {
	c.Diagnostics = append(c.Diagnostics, fmt.Sprintf(format, args...))
}

func (c *Catalog) walk(root, scope string, seen map[string]bool) map[string][]Metadata {
	entries := make(map[string][]Metadata)
	var visit func(string)
	visit = func(path string) {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			if path != root || !os.IsNotExist(err) {
				c.note("%s: %v", path, err)
			}
			return
		}
		if seen[canonical] {
			return
		}
		info, err := os.Stat(canonical)
		if err != nil {
			c.note("%s: %v", path, err)
			return
		}
		if !info.IsDir() {
			return
		}
		seen[canonical] = true
		file := filepath.Join(canonical, "SKILL.md")
		if _, err := os.Lstat(file); err == nil {
			m, err := c.metadata(file, canonical, scope)
			if err != nil {
				c.note("%s: %v", file, err)
			} else {
				entries[m.Name] = append(entries[m.Name], m)
				if _, fits := catalogMetadata(m); !m.UserOnly && !fits {
					c.note("%s: metadata exceeds catalog page limit; omitted from automatic lists, bounded named reads remain available", file)
				}
			}
		}
		children, err := os.ReadDir(canonical)
		if err != nil {
			c.note("%s: %v", path, err)
			return
		}
		for _, child := range children {
			if child.IsDir() || child.Type()&os.ModeSymlink != 0 {
				visit(filepath.Join(canonical, child.Name()))
			}
		}
	}
	visit(root)
	return entries
}

var conventionalName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (c *Catalog) metadata(file, base, scope string) (Metadata, error) {
	m := Metadata{Scope: scope, Source: file, BaseDir: base}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return m, err
	}
	if !inside(base, resolved) {
		return m, fmt.Errorf("SKILL.md escapes bundle")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() {
		return m, fmt.Errorf("SKILL.md must be a regular file")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return m, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return m, err
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return m, fmt.Errorf("missing YAML frontmatter and description")
	}
	end := 1
	for end < len(lines) && lines[end] != "---" && lines[end] != "..." {
		end++
	}
	if end == len(lines) {
		return m, fmt.Errorf("unterminated or oversized YAML frontmatter")
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fields); err != nil {
		return m, fmt.Errorf("invalid YAML: %w", err)
	}
	if raw, ok := fields["name"]; ok {
		var valid bool
		m.Name, valid = raw.(string)
		if !valid {
			return m, fmt.Errorf("name must be a string")
		}
	} else {
		m.Name = filepath.Base(base)
	}
	if m.Name == "" || strings.ContainsAny(m.Name, "/\\") || strings.IndexFunc(m.Name, unicode.IsSpace) >= 0 || m.Name == "." || m.Name == ".." {
		return m, fmt.Errorf("unusable skill name %q", m.Name)
	}
	if !conventionalName.MatchString(m.Name) || len(m.Name) > 64 {
		c.note("%s: name %q does not follow lowercase hyphenated naming (max 64 bytes)", file, m.Name)
	}
	m.Description, _ = fields["description"].(string)
	if strings.TrimSpace(m.Description) == "" {
		return m, fmt.Errorf("description must be a nonempty string")
	}
	if len(m.Description) > 1024 {
		c.note("%s: description exceeds 1024 bytes; catalog display is abbreviated", file)
	}
	if value, ok := fields["disable-model-invocation"]; ok {
		var valid bool
		m.UserOnly, valid = value.(bool)
		if !valid {
			return m, fmt.Errorf("disable-model-invocation must be a boolean")
		}
	}
	for key := range fields {
		switch key {
		case "name", "description", "disable-model-invocation", "license", "compatibility", "metadata", "version", "author", "tags":
		default:
			c.note("%s: unsupported metadata %q is not enforced", file, key)
		}
	}
	return m, nil
}

func inside(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
