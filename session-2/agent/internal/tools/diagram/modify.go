package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ai-course/session-2/agent/internal/toolspec"
	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// ModifyDiagram changes one element of the diagram already on the canvas and
// redraws it, so a follow-up like "rename that box" or "point that arrow
// somewhere else" doesn't mean restating the whole picture.
//
// It works because generate_diagram saves the spec it drew from. Without that,
// a modify tool would have to be handed the entire diagram again to change one
// label — which is just generate_diagram with extra steps.
type ModifyDiagram struct {
	// Dir is where the canvas files live. Empty means the working directory.
	Dir string
}

// specFile holds the last drawn diagram. It sits next to the rendered files and
// is the reason a one-element edit is possible at all.
const specFile = "canvas.diagram.json"

// diagramSpec is exactly what generate_diagram was called with: enough to
// redraw the identical picture, and the thing modify_diagram edits.
type diagramSpec struct {
	Title    string           `json:"title"`
	Elements []diagramElement `json:"elements"`
}

func (ModifyDiagram) Spec() components.ChatFunctionTool {
	return toolspec.Define("modify_diagram",
		"Change ONE element of the diagram already drawn by generate_diagram, then redraw both canvas files. Use this for follow-ups like renaming a box, changing a shape, or repointing an arrow — do not redraw the whole diagram for a one-element change. Boxes are addressed by their id; arrows by \"from->to\" (e.g. \"start->form\") unless they were given an explicit id.",
		`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which element to change: a box id, or an arrow written as \"from->to\", e.g. \"validate->create\"."
    },
    "updates": {
      "type": "object",
      "description": "Only the fields to change; anything omitted is left alone.",
      "properties": {
        "label": {"type": "string", "description": "New text. For a box, the text inside it; for an arrow, its edge label."},
        "shape": {"type": "string", "enum": ["box", "ellipse", "diamond"], "description": "New box shape."},
        "from":  {"type": "string", "description": "For an arrow: repoint its source to this box id."},
        "to":    {"type": "string", "description": "For an arrow: repoint its target to this box id."}
      }
    }
  },
  "required": ["id", "updates"]
}`)
}

func (t ModifyDiagram) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		ID      string `json:"id"`
		Updates struct {
			// Pointers so an omitted field is distinguishable from one
			// deliberately set to "" — clearing an edge label is a real edit.
			Label *string `json:"label"`
			Shape *string `json:"shape"`
			From  *string `json:"from"`
			To    *string `json:"to"`
		} `json:"updates"`
	}
	if err := toolspec.Decode(args, &a); err != nil {
		return "", err
	}

	id := strings.TrimSpace(a.ID)
	if id == "" {
		return "", fmt.Errorf("id is required: name the element to change")
	}

	spec, err := loadSpec(t.Dir)
	if err != nil {
		return "", err
	}

	index := findElement(spec.Elements, id)
	if index < 0 {
		return "", fmt.Errorf("no element with id %q on the canvas; it currently has %s",
			id, strings.Join(elementIDs(spec.Elements), ", "))
	}

	// Mutate a copy. If the edit produces something that won't build — an arrow
	// repointed at a box that doesn't exist — the canvas and the saved spec are
	// both left exactly as they were.
	edited := spec
	edited.Elements = append([]diagramElement(nil), spec.Elements...)
	target := &edited.Elements[index]

	var changes []string
	if v := a.Updates.Label; v != nil && *v != target.Label {
		changes = append(changes, fmt.Sprintf("label %q → %q", target.Label, *v))
		target.Label = *v
	}
	if v := a.Updates.Shape; v != nil {
		if got := normalizeShape(*v); got != normalizeShape(target.Shape) {
			changes = append(changes, fmt.Sprintf("shape %s → %s", normalizeShape(target.Shape), got))
			target.Shape = got
		}
	}
	if v := a.Updates.From; v != nil && *v != target.From {
		changes = append(changes, fmt.Sprintf("from %q → %q", target.From, *v))
		target.From = *v
	}
	if v := a.Updates.To; v != nil && *v != target.To {
		changes = append(changes, fmt.Sprintf("to %q → %q", target.To, *v))
		target.To = *v
	}

	if len(changes) == 0 {
		return fmt.Sprintf("%q is already in that state; nothing to change.", id), nil
	}

	boxes, arrows, err := drawDiagram(t.Dir, edited)
	if err != nil {
		return "", fmt.Errorf("that change would break the diagram, so nothing was modified: %w", err)
	}

	return fmt.Sprintf("Updated %s: %s.\nRedrew %d boxes and %d arrows.\n%s",
		id, strings.Join(changes, ", "), boxes, arrows, canvasLocations(t.Dir)), nil
}

// findElement locates an element by the id a caller would naturally use: a
// box's own id, or an arrow's explicit id, or an arrow written "from->to".
func findElement(elements []diagramElement, id string) int {
	for i, e := range elements {
		if elementID(e) == id {
			return i
		}
	}
	// Second pass, tolerant of spacing around the arrow: "a -> b" and "a->b"
	// are the same element to a human, and the model writes both.
	want := strings.ReplaceAll(id, " ", "")
	for i, e := range elements {
		if strings.ReplaceAll(elementID(e), " ", "") == want {
			return i
		}
	}
	return -1
}

// elementID is how an element is addressed. Boxes already have ids; arrows
// usually don't, so they fall back to "from->to", which reads naturally and is
// what a model guesses without being told.
func elementID(e diagramElement) string {
	if id := strings.TrimSpace(e.ID); id != "" {
		return id
	}
	if elementKind(e) == "arrow" {
		return strings.TrimSpace(e.From) + "->" + strings.TrimSpace(e.To)
	}
	return ""
}

// elementIDs lists what's addressable, for the error message when an id misses.
// Sorted so the same diagram always reports the same list.
func elementIDs(elements []diagramElement) []string {
	var ids []string
	for _, e := range elements {
		if id := elementID(e); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func loadSpec(dir string) (diagramSpec, error) {
	raw, err := os.ReadFile(filepath.Join(dir, specFile))
	if os.IsNotExist(err) {
		return diagramSpec{}, fmt.Errorf("there is no diagram to modify yet; call generate_diagram first")
	}
	if err != nil {
		return diagramSpec{}, err
	}
	var spec diagramSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return diagramSpec{}, fmt.Errorf("the saved diagram is unreadable (%v); redraw it with generate_diagram", err)
	}
	return spec, nil
}

func saveSpec(dir string, spec diagramSpec) error {
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, specFile), append(raw, '\n'), 0o644)
}
