package diagram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// excalidrawScene is the subset of the scene format the tests assert on.
type excalidrawScene struct {
	Type     string           `json:"type"`
	Version  int              `json:"version"`
	Source   string           `json:"source"`
	AppState map[string]any   `json:"appState"`
	Files    map[string]any   `json:"files"`
	Elements []map[string]any `json:"elements"`
}

func drawScene(t *testing.T, args string) (excalidrawScene, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), args); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, excalidrawFile))
	if err != nil {
		t.Fatalf("scene not written: %v", err)
	}
	var scene excalidrawScene
	if err := json.Unmarshal(raw, &scene); err != nil {
		t.Fatalf("scene is not valid JSON: %v", err)
	}
	return scene, dir
}

// One call must leave both files behind: the SVG to refresh in a browser and
// the scene to open at excalidraw.com.
func TestGenerateDiagramWritesBothFormats(t *testing.T) {
	_, dir := drawScene(t, signupElements)

	for _, name := range []string{canvasFile, excalidrawFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}
}

// excalidraw.com rejects a scene whose envelope is wrong, so the wrapper fields
// matter as much as the elements.
func TestExcalidrawSceneEnvelope(t *testing.T) {
	scene, _ := drawScene(t, signupElements)

	if scene.Type != "excalidraw" {
		t.Errorf(`type = %q, want "excalidraw"`, scene.Type)
	}
	if scene.Version != 2 {
		t.Errorf("version = %d, want 2", scene.Version)
	}
	if scene.Source == "" {
		t.Error("source must be set")
	}
	if scene.AppState == nil {
		t.Error("appState must be present")
	}
	if scene.Files == nil {
		t.Error("files must be present (empty object is fine)")
	}
}

// Every box becomes a shape plus its bound label, every arrow becomes an arrow,
// and the shapes map onto Excalidraw's primitives by role.
func TestExcalidrawElementsMatchDiagram(t *testing.T) {
	scene, _ := drawScene(t, signupElements)

	count := map[string]int{}
	for _, el := range scene.Elements {
		count[el["type"].(string)]++
	}
	if count["rectangle"] != 4 {
		t.Errorf("rectangles = %d, want 4", count["rectangle"])
	}
	if count["ellipse"] != 2 {
		t.Errorf("ellipses = %d, want 2", count["ellipse"])
	}
	if count["diamond"] != 1 {
		t.Errorf("diamonds = %d, want 1", count["diamond"])
	}
	if count["arrow"] != 7 {
		t.Errorf("arrows = %d, want 7", count["arrow"])
	}
	// 7 bound box labels + the title + 2 edge labels ("yes"/"no").
	if count["text"] != 10 {
		t.Errorf("text elements = %d, want 10", count["text"])
	}
}

// Bindings are what keep arrows attached when the user drags a box after
// importing. Every referenced id must exist, and every bound label must point
// at its container and be listed by it — a dangling id makes Excalidraw drop
// the element on load.
func TestExcalidrawBindingsResolve(t *testing.T) {
	scene, _ := drawScene(t, signupElements)

	byID := map[string]map[string]any{}
	for _, el := range scene.Elements {
		id, _ := el["id"].(string)
		if id == "" {
			t.Fatal("every element needs an id")
		}
		if _, dup := byID[id]; dup {
			t.Fatalf("duplicate element id %q", id)
		}
		byID[id] = el
	}

	for _, el := range scene.Elements {
		switch el["type"] {
		case "arrow":
			for _, side := range []string{"startBinding", "endBinding"} {
				b, ok := el[side].(map[string]any)
				if !ok {
					t.Errorf("arrow %v has no %s", el["id"], side)
					continue
				}
				target, _ := b["elementId"].(string)
				if _, exists := byID[target]; !exists {
					t.Errorf("arrow %v %s points at unknown element %q", el["id"], side, target)
				}
			}
		case "text":
			container, _ := el["containerId"].(string)
			if container == "" {
				continue // title and edge labels are free-standing
			}
			parent, exists := byID[container]
			if !exists {
				t.Errorf("text %v is bound to unknown container %q", el["id"], container)
				continue
			}
			bound, _ := parent["boundElements"].([]any)
			found := false
			for _, b := range bound {
				if m, ok := b.(map[string]any); ok && m["id"] == el["id"] {
					found = true
				}
			}
			if !found {
				t.Errorf("container %q does not list its bound text %v", container, el["id"])
			}
		}
	}
}

// Redrawing the same diagram must produce byte-identical output, so a scene
// re-imported after an edit is recognisably the same drawing rather than a
// reshuffled one. This is why the ids and seeds are derived, not random.
func TestExcalidrawOutputIsDeterministic(t *testing.T) {
	first, _ := drawScene(t, signupElements)
	second, _ := drawScene(t, signupElements)

	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("two draws of the same diagram produced different scenes")
	}
}

// The scene must carry the labels the caller asked for — an element grid with
// no readable text is the failure that looks like success.
func TestExcalidrawCarriesLabels(t *testing.T) {
	scene, _ := drawScene(t, signupElements)

	seen := map[string]bool{}
	for _, el := range scene.Elements {
		if s, ok := el["text"].(string); ok {
			seen[s] = true
		}
	}
	for _, want := range []string{"User signup", "Start", "Create account", "Signed up", "yes", "no"} {
		if !seen[want] {
			t.Errorf("label %q missing from the scene", want)
		}
	}
}
