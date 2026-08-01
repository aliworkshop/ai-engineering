package diagram

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every shape in the vocabulary must actually draw, in both renderings. A shape
// the schema advertises but the renderer silently turns into a rectangle is
// worse than one that isn't offered at all.
func TestEveryShapeRenders(t *testing.T) {
	shapes := []string{
		"box", "ellipse", "circle", "diamond", "hexagon", "parallelogram",
		"cylinder", "document", "note", "triangle", "cloud", "pill",
	}

	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			dir := t.TempDir()
			args := `{"elements":[{"type":"box","id":"a","label":"Shape","shape":"` + shape + `"}]}`
			if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), args); err != nil {
				t.Fatalf("run: %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(dir, canvasFile))
			if err != nil {
				t.Fatalf("canvas: %v", err)
			}
			if err := xml.Unmarshal(raw, new(struct{ XMLName xml.Name })); err != nil {
				t.Fatalf("%s produced malformed SVG: %v", shape, err)
			}
			svg := string(raw)
			// Something must have been drawn beyond the background rect and the
			// label — otherwise the shape fell through to nothing.
			if !strings.Contains(svg, "<path") && !strings.Contains(svg, "<polygon") &&
				!strings.Contains(svg, "<ellipse") && !strings.Contains(svg, "<circle") &&
				strings.Count(svg, "<rect") < 2 {
				t.Errorf("%s drew no outline: %s", shape, svg)
			}
			if !strings.Contains(svg, ">Shape<") {
				t.Errorf("%s lost its label", shape)
			}

			// And the Excalidraw scene must carry a real element for it too.
			scene := readScene(t, dir)
			kinds := map[string]int{}
			for _, el := range scene.Elements {
				kinds[el["type"].(string)]++
			}
			if kinds["rectangle"]+kinds["ellipse"]+kinds["diamond"]+kinds["line"] == 0 {
				t.Errorf("%s produced no excalidraw primitive: %v", shape, kinds)
			}
		})
	}
}

// Domain words are what a model actually writes — "database", not "cylinder".
func TestShapeSynonyms(t *testing.T) {
	cases := map[string]string{
		"database": "cylinder", "db": "cylinder", "store": "cylinder",
		"decision": "diamond", "if": "diamond",
		"start": "ellipse", "end": "ellipse",
		"input": "parallelogram", "output": "parallelogram",
		"report": "document", "file": "document",
		"external": "cloud", "service": "cloud",
		"comment": "note", "sticky": "note",
		"":        "box",
		"unknown": "box",
	}
	for word, want := range cases {
		if got := normalizeShape(word); got != want {
			t.Errorf("normalizeShape(%q) = %q, want %q", word, got, want)
		}
	}
}

// A shape's bounding box has to grow for geometry that eats space, or the label
// spills outside the outline.
func TestShapeAspectGivesCornersRoom(t *testing.T) {
	for _, shape := range []string{"diamond", "circle", "triangle", "hexagon"} {
		w, h := shapeAspect(shape)
		if w < 1 || h < 1 {
			t.Errorf("%s aspect (%g, %g) shrinks the box below its label", shape, w, h)
		}
	}
	if w, h := shapeAspect("box"); w != 1 || h != 1 {
		t.Errorf("a plain box should not be resized, got (%g, %g)", w, h)
	}
}

// Line-drawn shapes must close, or Excalidraw renders an open squiggle.
func TestExShapePointsClose(t *testing.T) {
	for _, shape := range []string{"hexagon", "parallelogram", "triangle", "note", "document", "cylinder"} {
		pts := exShapePoints(shape, 100, 60)
		if len(pts) < 4 {
			t.Fatalf("%s: expected an outline, got %v", shape, pts)
		}
		first, last := pts[0], pts[len(pts)-1]
		if first[0] != last[0] || first[1] != last[1] {
			t.Errorf("%s outline does not close: starts %v ends %v", shape, first, last)
		}
	}
	// The natives must NOT be line-drawn.
	for _, shape := range []string{"box", "ellipse", "diamond"} {
		if pts := exShapePoints(shape, 100, 60); pts != nil {
			t.Errorf("%s should use an excalidraw primitive, got points %v", shape, pts)
		}
	}
}

func readScene(t *testing.T, dir string) excalidrawScene {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, excalidrawFile))
	if err != nil {
		t.Fatalf("scene: %v", err)
	}
	var scene excalidrawScene
	if err := json.Unmarshal(raw, &scene); err != nil {
		t.Fatalf("scene is not valid JSON: %v", err)
	}
	return scene
}
