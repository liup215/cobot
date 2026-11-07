// Package skills discovers Agent Skills — directories containing a SKILL.md
// with YAML-ish frontmatter — and renders the advertisement section that lets
// a model load them on demand with the read tool (progressive disclosure).
//
// The model never receives full skill instructions up front: the system
// prompt only lists each skill's name, description and SKILL.md path. When a
// task matches a description, the model calls read on that path and follows
// the instructions it finds there. Supporting files referenced inside a skill
// (scripts/, references/) are relative to the skill's directory.
//
// Discovery is deliberately forgiving, matching the Agent Skills
// specification: files without a description, malformed frontmatter or
// unreadable paths are skipped rather than failing the whole scan, and the
// first skill wins a name collision (roots are checked in the order given,
// so a caller can put a user root before a bundled root to let user skills
// override bundled ones).
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skill is one discovered SKILL.md.
type Skill struct {
	// Name is the frontmatter name, falling back to the directory name.
	// It doubles as the dedupe key across discovery roots.
	Name string
	// Description is the routing description shown to the model. Required —
	// a SKILL.md without one is not loaded.
	Description string
	// Path is the path to the SKILL.md file itself (what the model reads).
	Path string
	// Dir is the directory containing SKILL.md; paths inside the skill are
	// relative to it.
	Dir string
	// DisableModelInvocation hides the skill from the advertised list
	// (frontmatter disable-model-invocation: true). Such a skill is still
	// returned by Discover — hosts may expose it through an explicit
	// command — but Section never advertises it.
	DisableModelInvocation bool
}

// ParseSKILL reads and parses a single SKILL.md file.
//
// It returns an error when the file has no frontmatter block, no usable
// description, or cannot be read — callers doing discovery should treat any
// error as "skip this file".
func ParseSKILL(path string) (Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, fmt.Errorf("skills: read %s: %w", path, err)
	}
	meta, err := parseFrontmatter(string(raw))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: %s: %w", path, err)
	}

	desc := strings.TrimSpace(meta["description"])
	if desc == "" {
		return Skill{}, fmt.Errorf("skills: %s: missing description", path)
	}
	if len(desc) > 1024 {
		desc = desc[:1024]
	}

	name := strings.TrimSpace(meta["name"])
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}

	dir := filepath.Dir(path)
	return Skill{
		Name:                   name,
		Description:            desc,
		Path:                   path,
		Dir:                    dir,
		DisableModelInvocation: strings.EqualFold(strings.TrimSpace(meta["disable-model-invocation"]), "true"),
	}, nil
}

// Discover walks each root (non-existent roots are skipped) for SKILL.md
// files and returns the parsed skills, sorted by name for a deterministic
// prompt. Invalid files are skipped; duplicate names keep the first
// occurrence across roots, so ordering the roots user-first gives user
// skills precedence over bundled ones.
func Discover(roots ...string) ([]Skill, error) {
	seen := make(map[string]bool)
	var out []Skill
	var firstErr error

	for _, root := range roots {
		if root == "" {
			continue
		}
		if _, err := os.Stat(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("skills: stat %s: %w", root, err)
			}
			continue
		}
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry: skip, keep walking
			}
			if d.IsDir() {
				// Skip hidden directories (.git, .builtin, ...) — a skill
				// never lives inside one and the walk would be pointless.
				if strings.HasPrefix(d.Name(), ".") && path != root {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "SKILL.md" {
				return nil
			}
			skill, perr := ParseSKILL(path)
			if perr != nil {
				return nil // malformed skill: skip silently (spec behavior)
			}
			if seen[skill.Name] {
				return nil // first root wins
			}
			seen[skill.Name] = true
			out = append(out, skill)
			return nil
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("skills: walk %s: %w", root, err)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, firstErr
}

// Section renders the SKILLS system-prompt section for the advertised skills
// (those without disable-model-invocation). It returns "" when nothing is
// advertised, so callers can append unconditionally.
func Section(list []Skill) string {
	advertised := make([]Skill, 0, len(list))
	for _, s := range list {
		if !s.DisableModelInvocation {
			advertised = append(advertised, s)
		}
	}
	if len(advertised) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\nSKILLS\n\n")
	b.WriteString("Skills are instruction files (SKILL.md) for specific kinds of work. Each skill below lists its name, what it is for, and where its SKILL.md lives.\n\n")
	b.WriteString("When the user's request matches a skill's description, call read on that SKILL.md path and follow the instructions it contains. Load skills on demand — do not read all of them upfront. Files referenced inside a skill (scripts, references, assets) are relative to the skill's directory.\n")
	for _, s := range advertised {
		fmt.Fprintf(&b, "\n- %s: %s\n  SKILL.md: %s\n", s.Name, oneLine(s.Description), s.Path)
	}
	return b.String()
}

// oneLine folds a description onto a single line so the bullet list stays
// readable regardless of how the frontmatter was wrapped.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
