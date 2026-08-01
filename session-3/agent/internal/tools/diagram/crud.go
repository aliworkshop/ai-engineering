package diagram

import (
	"context"
	"fmt"
	"strings"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/aliworkshop/ai-engineering-course/session-3/agent/internal/toolspec"
)

// Focused CRUD over the diagram already on the canvas: add_elements,
// update_elements, remove_elements.
//
// Three properties shape all three tools:
//
//   - Additive. Each one changes only what it names. Nothing is regenerated
//     from scratch, so an edit can't quietly restyle or relabel the rest of the
//     picture the way a full redraw does.
//   - Batch. Each takes a list, because real edits arrive in groups ("add the
//     retry path" is a box and two arrows) and three round trips to add three
//     elements is three chances for the model to drift.
//   - Explicit. Nothing cascades. Removing a box that still has arrows on it is
//     an error naming those arrows, not a silent deletion of edges the caller
//     never mentioned.
//
// All three are atomic: the edit is applied to a clone, the redraw is attempted,
// and only then is anything written. A batch that would break the diagram
// leaves the canvas and the saved spec untouched.

// AddElements appends boxes and arrows to the current diagram.
type AddElements struct {
	// Dir is where the canvas files live. Empty means the working directory.
	Dir string
}

func (AddElements) Spec() components.ChatFunctionTool {
	return toolspec.Define("add_elements",
		"Add one or more boxes and/or arrows to the diagram already drawn by generate_diagram, leaving everything else untouched. Pass them all in one call. Same element shape as generate_diagram: boxes need an id, arrows need from/to.",
		`{
  "type": "object",
  "properties": {
    "elements": {
      "type": "array",
      "description": "The new boxes and arrows to append.",
      "items": {
        "type": "object",
        "properties": {
          "type":  {"type": "string", "enum": ["box", "arrow"]},
          "id":    {"type": "string", "description": "For a box: a new unique id."},
          "label": {"type": "string", "description": "Box text, or an arrow's edge label."},
          "shape": {"type": "string", "enum": ["box", "ellipse", "diamond"]},
          "from":  {"type": "string", "description": "For an arrow: source box id."},
          "to":    {"type": "string", "description": "For an arrow: target box id."}
        },
        "required": ["type"]
      }
    }
  },
  "required": ["elements"]
}`)
}

func (t AddElements) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Elements []diagramElement `json:"elements"`
	}
	if err := toolspec.Decode(args, &a); err != nil {
		return "", err
	}
	if len(a.Elements) == 0 {
		return "", fmt.Errorf("elements is empty: name at least one box or arrow to add")
	}

	spec, err := loadSpec(t.Dir)
	if err != nil {
		return "", err
	}

	edited := spec.clone()
	var added []string
	for i, e := range a.Elements {
		id := elementID(e)
		if id == "" {
			return "", fmt.Errorf("elements[%d] has no id and no from/to, so there is nothing to add", i)
		}
		// Adding is additive, not upsert: a colliding id is a caller mistake,
		// and silently overwriting would lose whatever was already there.
		if findElement(edited.Elements, id) >= 0 {
			return "", fmt.Errorf("%q is already on the canvas; use update_elements to change it", id)
		}
		edited.Elements = append(edited.Elements, e)
		added = append(added, id)
	}

	boxes, arrows, err := drawDiagram(t.Dir, edited)
	if err != nil {
		return "", fmt.Errorf("those additions would break the diagram, so nothing was added: %w", err)
	}
	return fmt.Sprintf("Added %s.\nRedrew %d boxes and %d arrows.\n%s",
		strings.Join(quoteAll(added), ", "), boxes, arrows, canvasLocations(t.Dir)), nil
}

// UpdateElements changes fields on elements that already exist.
type UpdateElements struct {
	Dir string
}

func (UpdateElements) Spec() components.ChatFunctionTool {
	return toolspec.Define("update_elements",
		"Change one or more existing elements of the diagram — labels, shapes, where an arrow points — and redraw. Only the fields you name change; everything else is left alone. Boxes are addressed by their id, arrows by \"from->to\" (e.g. \"validate->create\"). Do not call generate_diagram to edit an existing diagram.",
		`{
  "type": "object",
  "properties": {
    "updates": {
      "type": "array",
      "description": "One entry per element to change.",
      "items": {
        "type": "object",
        "properties": {
          "id":    {"type": "string", "description": "Box id, or an arrow written \"from->to\"."},
          "label": {"type": "string", "description": "New text. For a box, the text inside it; for an arrow, its edge label."},
          "shape": {"type": "string", "enum": ["box", "ellipse", "diamond"], "description": "New box shape."},
          "from":  {"type": "string", "description": "For an arrow: repoint its source to this box id."},
          "to":    {"type": "string", "description": "For an arrow: repoint its target to this box id."}
        },
        "required": ["id"]
      }
    }
  },
  "required": ["updates"]
}`)
}

// elementUpdate is one requested change. The fields are pointers so an omitted
// field is distinguishable from one deliberately set to "" — clearing an edge
// label is a real edit, not a no-op.
type elementUpdate struct {
	ID    string  `json:"id"`
	Label *string `json:"label"`
	Shape *string `json:"shape"`
	From  *string `json:"from"`
	To    *string `json:"to"`
}

func (t UpdateElements) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Updates []elementUpdate `json:"updates"`
	}
	if err := toolspec.Decode(args, &a); err != nil {
		return "", err
	}
	if len(a.Updates) == 0 {
		return "", fmt.Errorf("updates is empty: name at least one element to change")
	}

	spec, err := loadSpec(t.Dir)
	if err != nil {
		return "", err
	}

	edited := spec.clone()
	var report []string
	for _, u := range a.Updates {
		id := strings.TrimSpace(u.ID)
		if id == "" {
			return "", fmt.Errorf("every update needs an id naming the element to change")
		}
		i := findElement(edited.Elements, id)
		if i < 0 {
			return "", fmt.Errorf("no element with id %q on the canvas; it currently has %s",
				id, strings.Join(elementIDs(edited.Elements), ", "))
		}
		if changes := applyUpdate(&edited.Elements[i], u); len(changes) > 0 {
			report = append(report, fmt.Sprintf("%s (%s)", id, strings.Join(changes, ", ")))
		}
	}

	if len(report) == 0 {
		return "Every element is already in the requested state; nothing to change.", nil
	}

	boxes, arrows, err := drawDiagram(t.Dir, edited)
	if err != nil {
		return "", fmt.Errorf("those changes would break the diagram, so nothing was modified: %w", err)
	}
	return fmt.Sprintf("Updated %s.\nRedrew %d boxes and %d arrows.\n%s",
		strings.Join(report, "; "), boxes, arrows, canvasLocations(t.Dir)), nil
}

// applyUpdate writes the requested fields onto an element and reports what
// actually changed, so a no-op edit can be told apart from a real one.
func applyUpdate(target *diagramElement, u elementUpdate) []string {
	var changes []string
	if v := u.Label; v != nil && *v != target.Label {
		changes = append(changes, fmt.Sprintf("label %q → %q", target.Label, *v))
		target.Label = *v
	}
	if v := u.Shape; v != nil {
		if got := normalizeShape(*v); got != normalizeShape(target.Shape) {
			changes = append(changes, fmt.Sprintf("shape %s → %s", normalizeShape(target.Shape), got))
			target.Shape = got
		}
	}
	if v := u.From; v != nil && *v != target.From {
		changes = append(changes, fmt.Sprintf("from %q → %q", target.From, *v))
		target.From = *v
	}
	if v := u.To; v != nil && *v != target.To {
		changes = append(changes, fmt.Sprintf("to %q → %q", target.To, *v))
		target.To = *v
	}
	return changes
}

// RemoveElements deletes elements from the diagram.
type RemoveElements struct {
	Dir string
}

func (RemoveElements) Spec() components.ChatFunctionTool {
	return toolspec.Define("remove_elements",
		"Delete one or more elements from the diagram and redraw. Boxes are addressed by their id, arrows by \"from->to\". Removing a box does NOT remove its arrows automatically — name those in the same call, or the tool will tell you which ones are left dangling.",
		`{
  "type": "object",
  "properties": {
    "ids": {
      "type": "array",
      "description": "Ids to delete: box ids, and/or arrows written \"from->to\".",
      "items": {"type": "string"}
    }
  },
  "required": ["ids"]
}`)
}

func (t RemoveElements) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		IDs []string `json:"ids"`
	}
	if err := toolspec.Decode(args, &a); err != nil {
		return "", err
	}
	if len(a.IDs) == 0 {
		return "", fmt.Errorf("ids is empty: name at least one element to remove")
	}

	spec, err := loadSpec(t.Dir)
	if err != nil {
		return "", err
	}

	// Resolve every id up front. Deleting half a batch and then failing would
	// leave the caller guessing what survived.
	edited := spec.clone()
	doomed := map[int]bool{}
	var removed []string
	for _, raw := range a.IDs {
		id := strings.TrimSpace(raw)
		i := findElement(edited.Elements, id)
		if i < 0 {
			return "", fmt.Errorf("no element with id %q on the canvas; it currently has %s",
				id, strings.Join(elementIDs(edited.Elements), ", "))
		}
		if doomed[i] {
			continue // named twice in one batch; harmless
		}
		doomed[i] = true
		removed = append(removed, id)
	}

	kept := make([]diagramElement, 0, len(edited.Elements))
	for i, e := range edited.Elements {
		if !doomed[i] {
			kept = append(kept, e)
		}
	}
	edited.Elements = kept

	if orphans := danglingArrows(edited.Elements); len(orphans) > 0 {
		return "", fmt.Errorf("that would leave %s pointing at a box that no longer exists, so nothing was removed; name %s in the same call",
			strings.Join(quoteAll(orphans), ", "),
			map[bool]string{true: "it", false: "them"}[len(orphans) == 1])
	}

	if len(edited.Elements) == 0 {
		return "", fmt.Errorf("that would remove every element; a diagram needs at least one box, so nothing was removed")
	}

	boxes, arrows, err := drawDiagram(t.Dir, edited)
	if err != nil {
		return "", fmt.Errorf("that removal would break the diagram, so nothing was removed: %w", err)
	}
	return fmt.Sprintf("Removed %s.\nRedrew %d boxes and %d arrows.\n%s",
		strings.Join(quoteAll(removed), ", "), boxes, arrows, canvasLocations(t.Dir)), nil
}

// danglingArrows reports arrows whose endpoints are no longer present. This is
// what makes removal explicit rather than cascading: the caller is told exactly
// which edges they'd have to delete too, instead of the tool quietly deleting
// them.
func danglingArrows(elements []diagramElement) []string {
	boxes := map[string]bool{}
	for _, e := range elements {
		if elementKind(e) == "box" {
			boxes[strings.TrimSpace(e.ID)] = true
		}
	}
	var orphans []string
	for _, e := range elements {
		if elementKind(e) != "arrow" {
			continue
		}
		if !boxes[strings.TrimSpace(e.From)] || !boxes[strings.TrimSpace(e.To)] {
			orphans = append(orphans, elementID(e))
		}
	}
	return orphans
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}
