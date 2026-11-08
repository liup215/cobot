package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeSkill creates <root>/<name>/SKILL.md with the given body and returns
// the SKILL.md path.
func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// ─── frontmatter parsing ─────────────────────────────────────────────────────

func TestParseSKILLBasics(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "pdf-tools", `---
name: pdf-tools
description: Extract text from PDF files. Use when reading or converting PDFs.
---

# PDF tools
Body instructions here.
`)

	skill, err := ParseSKILL(path)
	if err != nil {
		t.Fatalf("ParseSKILL: %v", err)
	}
	if skill.Name != "pdf-tools" {
		t.Errorf("Name = %q, want pdf-tools", skill.Name)
	}
	if !strings.Contains(skill.Description, "Extract text from PDF") {
		t.Errorf("Description = %q", skill.Description)
	}
	if skill.Path != path {
		t.Errorf("Path = %q, want %q", skill.Path, path)
	}
	if skill.Dir != filepath.Dir(path) {
		t.Errorf("Dir = %q, want %q", skill.Dir, filepath.Dir(path))
	}
	if skill.DisableModelInvocation {
		t.Error("DisableModelInvocation should be false by default")
	}
}

// A SKILL.md without a name falls back to its directory name; the spec lets
// the directory name stand in when frontmatter omits it.
func TestParseSKILLNameFallsBackToDir(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "my-skill", `---
description: A skill without a declared name.
---
body
`)
	skill, err := ParseSKILL(path)
	if err != nil {
		t.Fatalf("ParseSKILL: %v", err)
	}
	if skill.Name != "my-skill" {
		t.Errorf("Name = %q, want my-skill (directory fallback)", skill.Name)
	}
}

func TestParseSKILLRejectsMissingDescription(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "no-desc", `---
name: no-desc
---
body
`)
	if _, err := ParseSKILL(path); err == nil {
		t.Fatal("expected error for missing description")
	}
}

func TestParseSKILLRejectsMissingFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "no-fm", "# just markdown\nno frontmatter\n")
	if _, err := ParseSKILL(path); err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

func TestParseSKILLRejectsUnterminatedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "open-fm", "---\nname: x\ndescription: y\nno closing fence\n")
	if _, err := ParseSKILL(path); err == nil {
		t.Fatal("expected error for unterminated frontmatter")
	}
}

func TestParseSKILLQuotedAndFoldedDescription(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "quoted", `---
name: quoted
description: "A quoted description: with colon."
disable-model-invocation: true
---
body
`)
	skill, err := ParseSKILL(path)
	if err != nil {
		t.Fatalf("ParseSKILL: %v", err)
	}
	if skill.Description != "A quoted description: with colon." {
		t.Errorf("Description = %q", skill.Description)
	}
	if !skill.DisableModelInvocation {
		t.Error("DisableModelInvocation should be true")
	}
}

func TestParseSKILLFoldedBlockDescription(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "folded", `---
name: folded
description: >-
  First line of the description
  continues onto a second line.
---
body
`)
	skill, err := ParseSKILL(path)
	if err != nil {
		t.Fatalf("ParseSKILL: %v", err)
	}
	want := "First line of the description continues onto a second line."
	if skill.Description != want {
		t.Errorf("Description = %q, want %q", skill.Description, want)
	}
}

// ─── discovery ───────────────────────────────────────────────────────────────

func TestDiscoverWalksAndSorts(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "zeta", "---\ndescription: Z skill.\n---\n")
	writeSkill(t, root, "alpha", "---\ndescription: A skill.\n---\n")
	writeSkill(t, root, "mid", "---\ndescription: M skill.\n---\n")
	// nested skill inside another skill's dir is still found (no reason to hide)
	writeSkill(t, filepath.Join(root, "alpha"), "nested", "---\ndescription: Nested.\n---\n")

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := "alpha,mid,nested,zeta"
	if strings.Join(names, ",") != want {
		t.Errorf("names = %v, want %s", names, want)
	}
}

func TestDiscoverSkipsInvalidAndHidden(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "---\ndescription: Good.\n---\n")
	writeSkill(t, root, "bad-no-desc", "---\nname: bad\n---\n")
	writeSkill(t, root, "bad-no-fm", "no frontmatter at all\n")
	// hidden directory is skipped entirely
	hidden := filepath.Join(root, ".hidden")
	writeSkill(t, hidden, "sneaky", "---\ndescription: Hidden.\n---\n")
	// non-SKILL.md markdown is ignored
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("---\ndescription: X\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		t.Errorf("got %v, want [good]", names)
	}
}

// First root wins a name collision: ordering user root before bundled root
// lets a user skill override a bundled one.
func TestDiscoverFirstRootWinsCollision(t *testing.T) {
	userRoot := t.TempDir()
	bundleRoot := t.TempDir()
	writeSkill(t, userRoot, "shared", "---\ndescription: User version.\n---\n")
	writeSkill(t, bundleRoot, "shared", "---\ndescription: Bundled version.\n---\n")

	got, err := Discover(userRoot, bundleRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d skills, want 1 (deduped)", len(got))
	}
	if got[0].Description != "User version." {
		t.Errorf("Description = %q, want user version to win", got[0].Description)
	}
}

// A non-existent root is skipped, not an error — hosts pass fixed paths.
func TestDiscoverMissingRootIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "only", "---\ndescription: Only.\n---\n")

	got, err := Discover(filepath.Join(root, "does-not-exist"), root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d skills, want 1", len(got))
	}
}

// ─── prompt section ──────────────────────────────────────────────────────────

func TestSectionRendersAdvertisedSkills(t *testing.T) {
	list := []Skill{
		{Name: "pdf-tools", Description: "Extract text from PDFs. Use when reading PDFs.", Path: `C:\skills\pdf-tools\SKILL.md`},
		{Name: "secret", Description: "Not advertised.", Path: `C:\skills\secret\SKILL.md`, DisableModelInvocation: true},
	}
	got := Section(list)

	if !strings.Contains(got, "SKILLS") {
		t.Error("section should contain the SKILLS header")
	}
	if !strings.Contains(got, "- pdf-tools: Extract text from PDFs.") {
		t.Errorf("section should advertise pdf-tools, got:\n%s", got)
	}
	if !strings.Contains(got, `SKILL.md: C:\skills\pdf-tools\SKILL.md`) {
		t.Errorf("section should include the SKILL.md path, got:\n%s", got)
	}
	if strings.Contains(got, "secret") {
		t.Error("disable-model-invocation skill must not be advertised")
	}
	if !strings.Contains(got, "call read on that SKILL.md path") {
		t.Error("section should tell the model to read SKILL.md on demand")
	}
}

func TestSectionEmptyWhenNothingAdvertised(t *testing.T) {
	if s := Section(nil); s != "" {
		t.Errorf("Section(nil) = %q, want empty", s)
	}
	if s := Section([]Skill{{Name: "x", Description: "y", Path: "p", DisableModelInvocation: true}}); s != "" {
		t.Errorf("Section(all disabled) = %q, want empty", s)
	}
}

// Descriptions may span lines in frontmatter; the bullet must stay one line.
func TestSectionFoldsMultilineDescription(t *testing.T) {
	got := Section([]Skill{{Name: "s", Description: "line one\nline two", Path: "/p/SKILL.md"}})
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "- s:") && strings.Contains(line, "\n") {
			t.Error("bullet line must not contain embedded newline")
		}
	}
	if !strings.Contains(got, "- s: line one line two") {
		t.Errorf("description should be folded, got:\n%s", got)
	}
}

// ─── symlink / junction roots (shared skill store) ──────────────────────────

// linkDir creates link → target: a real symlink where the platform allows it
// (Unix always, Windows with Developer Mode), falling back to a directory
// junction on Windows (no privilege required). Skips the test if neither
// works.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		out, jerr := exec.Command("cmd", "/C", "mklink", "/J", filepath.FromSlash(link), filepath.FromSlash(target)).CombinedOutput()
		if jerr == nil {
			return
		}
		t.Skipf("cannot create symlink or junction: symlink=%v junction=%v (%s)", err, jerr, out)
	}
	t.Skipf("cannot create symlink: %v", err)
}

// The whole root being a link to a shared store (the pi-agent convention:
// ~/.ltedu-agent/skills → ~/.agents/skills) must discover the linked skills.
// filepath.WalkDir Lstats its root and stops at a link — this pins the
// os.Stat-based walker against that regression.
func TestDiscoverThroughLinkedRoot(t *testing.T) {
	real := t.TempDir()
	writeSkill(t, real, "shared-skill", "---\ndescription: Lives in the shared store.\n---\nbody\n")
	writeSkill(t, real, "outer", "---\ndescription: Outer.\n---\nbody\n")
	writeSkill(t, filepath.Join(real, "outer"), "inner", "---\ndescription: Nested inside outer.\n---\nbody\n")

	linkRoot := filepath.Join(t.TempDir(), "skills")
	linkDir(t, real, linkRoot)

	got, err := Discover(linkRoot)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := "inner,outer,shared-skill"
	if strings.Join(names, ",") != want {
		t.Errorf("linked-root discovery = %v, want [%s]", names, want)
	}
	// Paths stay under the link root (what the model will read), not the
	// resolved target.
	for _, s := range got {
		if !strings.HasPrefix(s.Path, linkRoot) {
			t.Errorf("skill %s path = %q, want prefix %q", s.Name, s.Path, linkRoot)
		}
	}
}

// A symlinked subdirectory INSIDE a real root (individual skills linked into
// the store) is entered too — same Stat-based classification.
func TestDiscoverThroughLinkedSubdir(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	writeSkill(t, external, "linked-in", "---\ndescription: Linked into the root.\n---\nbody\n")
	if err := os.Symlink(external, filepath.Join(root, "linked-in")); err != nil {
		if runtime.GOOS == "windows" {
			out, jerr := exec.Command("cmd", "/C", "mklink", "/J",
				filepath.Join(root, "linked-in"), external).CombinedOutput()
			if jerr != nil {
				t.Skipf("cannot create symlink or junction: %v / %v (%s)", err, jerr, out)
			}
		} else {
			t.Skipf("cannot create symlink: %v", err)
		}
	}

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Name != "linked-in" {
		t.Fatalf("got %d skills, want the linked-in one", len(got))
	}
}

// A link cycle must terminate instead of walking forever (depth cap +
// visited-set; EvalSymlinks cannot see Windows junction loops).
func TestDiscoverLinkCycleTerminates(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "a", "---\ndescription: A.\n---\nbody\n")
	// a/loop → root: descending a/loop reaches root again.
	loop := filepath.Join(root, "a", "loop")
	if err := os.Symlink(root, loop); err != nil {
		if runtime.GOOS == "windows" {
			if out, jerr := exec.Command("cmd", "/C", "mklink", "/J", loop, root).CombinedOutput(); jerr != nil {
				t.Skipf("cannot create cycle link: %v / %v (%s)", err, jerr, out)
			}
		} else {
			t.Skipf("cannot create symlink: %v", err)
		}
	}

	done := make(chan struct{})
	var got []Skill
	var derr error
	go func() {
		got, derr = Discover(root)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Discover did not terminate on a link cycle")
	}
	if derr != nil {
		t.Fatalf("Discover: %v", derr)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("cycle walk found %d skills, want just [a] (no duplicates)", len(got))
	}
}
