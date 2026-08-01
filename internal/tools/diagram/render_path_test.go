package diagram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// There are two ways a diagram can be drawn, and they produce entirely
// different SVG:
//
//   - Excalidraw's own renderer, when the sidecar is installed. This is the
//     real library — its stroke, its fonts, its arrow routing.
//   - The built-in Go renderers, otherwise.
//
// TestMain pins every other test in this package to the built-in path, because
// those tests assert on this package's own markup (class="tcell", the arrowhead
// marker, pie wedges) which Excalidraw does not and cannot emit. It also keeps
// them fast: the sidecar loads a 13 MB bundle per call.
//
// The Excalidraw path is covered here instead, and skips when the sidecar
// hasn't been built.
func TestMain(m *testing.M) {
	os.Setenv("AGENT_EXCALIDRAW_RENDERER", "off")
	os.Exit(m.Run())
}

// installedRenderer returns the sidecar path, or skips the test.
func installedRenderer(t *testing.T) string {
	t.Helper()
	// Undo TestMain's default so the real search runs.
	t.Setenv("AGENT_EXCALIDRAW_RENDERER", "")
	script := findRenderer()
	if script == "" {
		t.Skip("excalidraw renderer not built; run `npm install` in agent/excalidraw")
	}
	return script
}

// The headline claim: when the renderer is installed, the drawing is produced
// by Excalidraw itself.
func TestExcalidrawRendererDrawsTheCanvas(t *testing.T) {
	installedRenderer(t)

	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("draw: %v", err)
	}

	svg, err := os.ReadFile(filepath.Join(dir, canvasFile))
	if err != nil {
		t.Fatalf("canvas: %v", err)
	}
	// Excalidraw stamps its own output; nothing this package writes carries it.
	if !strings.Contains(string(svg), "svg-source:excalidraw") {
		t.Fatalf("canvas.svg was not rendered by excalidraw:\n%.200s", svg)
	}
	for _, label := range []string{"Start", "Create account", "Signed up"} {
		if !strings.Contains(string(svg), label) {
			t.Errorf("label %q missing from the excalidraw rendering", label)
		}
	}
}

// The scene must come back from convertToExcalidrawElements with the bindings
// and bound labels the skeletons asked for — that's the whole reason to run the
// real library rather than approximate it.
func TestExcalidrawRendererProducesBoundScene(t *testing.T) {
	installedRenderer(t)

	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("draw: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, excalidrawFile))
	if err != nil {
		t.Fatalf("scene: %v", err)
	}
	var scene excalidrawScene
	if err := json.Unmarshal(raw, &scene); err != nil {
		t.Fatalf("scene is not valid JSON: %v", err)
	}
	if scene.Type != "excalidraw" {
		t.Errorf("scene type = %q", scene.Type)
	}

	ids := map[string]bool{}
	for _, el := range scene.Elements {
		ids[el["id"].(string)] = true
	}

	var arrows, boundLabels int
	for _, el := range scene.Elements {
		if el["type"] == "arrow" {
			arrows++
			for _, side := range []string{"startBinding", "endBinding"} {
				if b, ok := el[side].(map[string]any); ok {
					if target, _ := b["elementId"].(string); target != "" && !ids[target] {
						t.Errorf("arrow %v has a dangling %s to %q", el["id"], side, target)
					}
				}
			}
		}
		if c, ok := el["containerId"].(string); ok && c != "" {
			boundLabels++
			if !ids[c] {
				t.Errorf("text %v is bound to a container that isn't in the scene", el["id"])
			}
		}
	}
	if arrows == 0 {
		t.Error("no arrows in the converted scene")
	}
	if boundLabels == 0 {
		t.Error("no bound labels in the converted scene — the label skeletons were dropped")
	}
}

// Skeletons are the contract between this package and the library, so they must
// survive the round trip unchanged.
func TestRendererConsumesTheSkeletonFile(t *testing.T) {
	script := installedRenderer(t)

	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("draw: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, skeletonFile))
	if err != nil {
		t.Fatalf("skeleton file: %v", err)
	}
	var skels []skeletonElement
	if err := json.Unmarshal(raw, &skels); err != nil {
		t.Fatalf("skeleton file is not valid skeletons: %v", err)
	}

	// Feeding the written file straight back to the renderer must work — that's
	// what makes canvas.skeleton.json a usable artifact rather than a log.
	out, err := renderWithExcalidraw(script, "User signup", skels)
	if err != nil {
		t.Fatalf("rendering the written skeletons failed: %v", err)
	}
	if !strings.Contains(out.SVG, "svg-source:excalidraw") {
		t.Error("re-rendering the skeleton file did not produce an excalidraw drawing")
	}
}

// A missing or broken sidecar must degrade to the built-in renderer, not break
// drawing. An agent that stops being able to draw because an optional npm
// package is absent is worse than one that draws a plainer picture.
func TestFallsBackWhenRendererMissing(t *testing.T) {
	t.Setenv("AGENT_EXCALIDRAW_RENDERER", filepath.Join(t.TempDir(), "does-not-exist.cjs"))

	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), signupElements); err != nil {
		t.Fatalf("drawing must still work without the renderer: %v", err)
	}
	svg, _ := os.ReadFile(filepath.Join(dir, canvasFile))
	if strings.Contains(string(svg), "svg-source:excalidraw") {
		t.Error("expected the built-in renderer, got an excalidraw drawing")
	}
	if !strings.Contains(string(svg), "Create account") {
		t.Error("the fallback drawing lost its labels")
	}
}

func TestRendererCanBeTurnedOff(t *testing.T) {
	for _, off := range []string{"off", "none", "0", "false", "OFF"} {
		t.Setenv("AGENT_EXCALIDRAW_RENDERER", off)
		if got := findRenderer(); got != "" {
			t.Errorf("AGENT_EXCALIDRAW_RENDERER=%q should disable the renderer, got %q", off, got)
		}
	}
}
