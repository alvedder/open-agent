package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	CatalogBytes = 8000
	ViewBytes    = 30000
)

type Page struct {
	Skills     []Metadata `json:"skills"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type View struct {
	Metadata
	Path         string `json:"path"`
	Content      string `json:"content"`
	Start        int    `json:"start"`
	End          int    `json:"end"`
	TotalLines   int    `json:"total_lines"`
	NextStart    int    `json:"next_start,omitempty"`
	ExecutionDir string `json:"execution_dir,omitempty"` // set by the execution adapter, not discovery
	Instructions bool   `json:"instructions,omitempty"`  // this file is the bundle's instruction source
}

func (c *Catalog) names() []string {
	names := make([]string, 0, len(c.entries))
	for name := range c.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve applies the same precedence and ambiguity rules used by List and View.
// Visibility in automatic catalogs is independent of name resolution.
func (c *Catalog) Resolve(name string) (Metadata, error) {
	entries := c.entries[name]
	if len(entries) == 0 {
		return Metadata{}, fmt.Errorf("skill %q is unavailable (not found or invalid)", name)
	}
	if len(entries) > 1 {
		return Metadata{}, fmt.Errorf("skill %q is ambiguous in %s scope", name, entries[0].Scope)
	}
	return entries[0], nil
}

// List returns a stable bounded page of automatically selectable metadata.
func (c *Catalog) List(cursor string) (Page, error) {
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil || start < 0 {
			return Page{}, fmt.Errorf("invalid skills cursor %q", cursor)
		}
	}
	var visible []Metadata
	for _, name := range c.names() {
		m, err := c.Resolve(name)
		if err == nil && !m.UserOnly {
			if m, fits := catalogMetadata(m); fits {
				visible = append(visible, m)
			}
		}
	}
	if start > len(visible) {
		return Page{}, fmt.Errorf("skills cursor %q is past catalog end", cursor)
	}
	page := Page{Skills: []Metadata{}}
	for i := start; i < len(visible); i++ {
		m := visible[i]
		candidate := Page{Skills: append(append([]Metadata{}, page.Skills...), m)}
		if i+1 < len(visible) {
			candidate.NextCursor = strconv.Itoa(i + 1)
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return Page{}, err
		}
		if len(encoded) > CatalogBytes {
			if len(page.Skills) == 0 {
				return Page{}, fmt.Errorf("metadata for skill %q exceeds catalog page limit", m.Name)
			}
			page.NextCursor = strconv.Itoa(i)
			break
		}
		page = candidate
	}
	return page, nil
}

// Account for the page wrapper and a maximum-width cursor before listing an
// entry. One unlistable identity must not prevent access to healthy entries.
func catalogMetadata(m Metadata) (Metadata, bool) {
	m.Description = abbreviate(m.Description, 1024)
	data, err := json.Marshal(Page{Skills: []Metadata{m}, NextCursor: "99999999999999999999"})
	return m, err == nil && len(data) <= CatalogBytes
}

func abbreviate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	n := limit - 3
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n] + "..."
}

// View reads current bytes. Relative resources may not escape the canonical
// bundle, even via symlinks. Line ranges are one-based and inclusive.
func (c *Catalog) View(name, file string, start, end int) (View, error) {
	m, err := c.Resolve(name)
	if err != nil {
		return View{}, err
	}
	if file == "" {
		file = "SKILL.md"
	}
	if filepath.IsAbs(file) {
		return View{}, fmt.Errorf("skill resource must be bundle-relative")
	}
	path := filepath.Join(m.BaseDir, file)
	if !inside(m.BaseDir, path) {
		return View{}, fmt.Errorf("skill resource escapes bundle")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return View{}, err
	}
	if !inside(m.BaseDir, path) {
		return View{}, fmt.Errorf("skill resource symlink escapes bundle")
	}
	info, err := os.Stat(path)
	if err != nil {
		return View{}, err
	}
	if !info.Mode().IsRegular() {
		return View{}, fmt.Errorf("skill resource must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return View{}, err
	}
	if !utf8.Valid(data) {
		return View{}, fmt.Errorf("skill resource must be UTF-8 text")
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if end < start && start <= len(lines) {
		return View{}, fmt.Errorf("end must not precede start")
	}
	v := View{Metadata: m, Path: path, Start: start, End: start - 1, TotalLines: len(lines)}
	if source, err := filepath.EvalSymlinks(m.Source); err == nil {
		v.Instructions = path == source
	}
	v.Description = abbreviate(v.Description, 1024)
	header, err := json.Marshal(v)
	if err != nil {
		return View{}, err
	}
	used := len(header) + 256 // space for changing range numbers and continuation
	if used >= ViewBytes {
		return View{}, fmt.Errorf("skill metadata exceeds view page limit")
	}
	var out strings.Builder
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if i < len(lines) || strings.HasSuffix(string(data), "\n") {
			line += "\n"
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			return View{}, err
		}
		size := len(encoded) - 2 // exclude the surrounding JSON quotes
		if size+len(header)+256 > ViewBytes {
			return View{}, fmt.Errorf("skill resource line %d exceeds %d-byte page limit; split the line to read it", i, ViewBytes)
		}
		if used+size > ViewBytes {
			v.NextStart = i
			break
		}
		used += size
		out.WriteString(line)
		v.End = i
	}
	if v.End < len(lines) && v.NextStart == 0 && v.End >= start {
		v.NextStart = v.End + 1
	}
	v.Content = out.String()
	return v, nil
}
