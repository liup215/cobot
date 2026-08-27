// Skill metadata types, extracted from gline pkg/types/skill.go.
// Skill file format follows the cline agent skill specification:
// Each skill is a directory containing a SKILL.md file.
//
// Example directory structure:
//
//	my-skill/
//	├── SKILL.md          # Required: frontmatter + markdown instructions
//	├── docs/             # Optional: additional documentation
//	├── templates/        # Optional: templates
//	└── scripts/          # Optional: utility scripts
//
// SKILL.md format:
//
//	---
//	name: my-skill
//	description: Brief description of what this skill does.
//	---
//
//	# My Skill
//
//	Detailed instructions...
package types

// Skill describes an externally-defined capability loaded from a
// SKILL.md file. Skills follow the cline agent skill specification:
// each skill is a directory containing a SKILL.md with YAML frontmatter
// (name, description) and markdown instructions.
type Skill struct {
	// Name is the canonical skill identifier (kebab-case).
	// Must match the directory name exactly.
	Name string `yaml:"name" json:"name"`

	// Description tells the LLM when to use this skill (max 1024 chars).
	Description string `yaml:"description" json:"description"`

	// Contents are the full skill instructions (Markdown body after frontmatter).
	// This is loaded on-demand when the use_skill tool is called.
	Contents string `yaml:"contents,omitempty" json:"contents,omitempty"`

	// Source is the absolute path to the SKILL.md file this skill was loaded from.
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
}

// SkillMeta is a lightweight summary of a skill for listing in the
// system prompt and slash-command menus. It contains only name and
// description so that the prompt stays small (~100 tokens per skill).
type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
