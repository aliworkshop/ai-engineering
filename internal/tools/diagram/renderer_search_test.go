package diagram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRenderer plants a script and its built bundle at dir/excalidraw/, so the
// search can be tested without a real 13 MB Excalidraw build.
func fakeRenderer(t *testing.T, dir string) string {
	t.Helper()
	side := filepath.Join(dir, "excalidraw")
	if err := os.MkdirAll(side, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(side, "render.cjs")
	for _, f := range []string{script, filepath.Join(side, "excalidraw-bundle.cjs")} {
		if err := os.WriteFile(f, []byte("// test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return script
}

// The bug this replaced: the search only ever walked *up*, so running from the
// directory that contains the agent — the session folder — never found
// agent/excalidraw/render.cjs and fell back to the built-in renderer without
// the user asking for it.
func TestSearchProbesTheAgentSubdirectory(t *testing.T) {
	root := t.TempDir()
	want := fakeRenderer(t, filepath.Join(root, "agent"))

	got := ancestorCandidates(root)
	found := false
	for _, candidate := range got {
		if candidate == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("running from the parent of agent/ must probe %q; candidates were %v", want, got)
	}
}

// The ordinary case still works: at or under the agent directory itself.
func TestSearchProbesSelfAndAncestors(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "internal", "tools", "diagram")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "excalidraw", "render.cjs")

	candidates := ancestorCandidates(deep)
	for _, c := range candidates {
		if c == want {
			return
		}
	}
	t.Fatalf("a directory under the agent must probe %q; candidates were %v", want, candidates)
}

// Every candidate must be absolute and named render.cjs — a relative probe
// would resolve differently depending on where the process happened to start.
func TestSearchCandidatesAreWellFormed(t *testing.T) {
	for _, c := range ancestorCandidates(t.TempDir()) {
		if !filepath.IsAbs(c) {
			t.Errorf("candidate %q is not absolute", c)
		}
		if filepath.Base(c) != "render.cjs" {
			t.Errorf("candidate %q does not point at render.cjs", c)
		}
	}
	if len(ancestorCandidates("/")) == 0 {
		t.Error("the filesystem root should still yield candidates, not loop forever")
	}
}

// The headline fix: found from a working directory with no relationship to the
// agent at all, because the binary knows where its own source tree is.
func TestRendererFoundFromAnUnrelatedDirectory(t *testing.T) {
	t.Setenv("AGENT_EXCALIDRAW_RENDERER", "")
	if findRenderer() == "" {
		t.Skip("excalidraw renderer not built; run `npm install` in agent/excalidraw")
	}

	t.Chdir(t.TempDir())
	got := findRenderer()
	if got == "" {
		t.Fatal("the renderer must be found from any working directory")
	}
	if !strings.HasSuffix(got, rendererScript) {
		t.Errorf("found %q, which is not the sidecar", got)
	}
}

// A binary must prefer the sidecar shipped with its own source over one that
// merely happens to sit near the working directory — otherwise session-3's
// agent would render with session-2's build.
func TestRendererPrefersItsOwnSourceTree(t *testing.T) {
	t.Setenv("AGENT_EXCALIDRAW_RENDERER", "")
	own := findRenderer()
	if own == "" {
		t.Skip("excalidraw renderer not built; run `npm install` in agent/excalidraw")
	}

	// A decoy sidecar in the working directory.
	decoyRoot := t.TempDir()
	decoy := fakeRenderer(t, decoyRoot)
	t.Chdir(decoyRoot)

	if got := findRenderer(); got != own {
		t.Errorf("found %q, want the binary's own renderer %q (decoy was %q)", got, own, decoy)
	}
}

// The explicit override still wins over everything, and still turns the
// renderer off.
func TestExplicitOverrideWins(t *testing.T) {
	root := t.TempDir()
	explicit := fakeRenderer(t, root)

	t.Setenv("AGENT_EXCALIDRAW_RENDERER", explicit)
	if got := findRenderer(); got != explicit {
		t.Errorf("explicit path should win, got %q", got)
	}

	t.Setenv("AGENT_EXCALIDRAW_RENDERER", "off")
	if got := findRenderer(); got != "" {
		t.Errorf(`"off" should disable the renderer, got %q`, got)
	}

	t.Setenv("AGENT_EXCALIDRAW_RENDERER", filepath.Join(root, "nope.cjs"))
	if got := findRenderer(); got != "" {
		t.Errorf("a bad explicit path should not silently fall through to a search, got %q", got)
	}
}

// A sidecar whose bundle was never built is not usable — it would only fail at
// the point of drawing.
func TestUnbuiltSidecarIsNotUsable(t *testing.T) {
	root := t.TempDir()
	script := fakeRenderer(t, root)
	if err := os.Remove(filepath.Join(filepath.Dir(script), "excalidraw-bundle.cjs")); err != nil {
		t.Fatal(err)
	}
	if usableRenderer(script) {
		t.Error("a sidecar with no built bundle should not be reported as usable")
	}
}
