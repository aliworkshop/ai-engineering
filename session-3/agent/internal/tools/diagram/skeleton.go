package diagram

import "fmt"

// The Excalidraw element skeleton.
//
// https://docs.excalidraw.com/docs/@excalidraw/excalidraw/api/excalidraw-element-skeleton
//
// A skeleton is the *authoring* form of an Excalidraw element: type, position,
// and intent. Everything mechanical — ids, seeds, version nonces, the bound text
// element that lives inside a shape, the two-way binding that keeps an arrow
// attached — is derived. Excalidraw ships convertToExcalidrawElements() to do
// that derivation.
//
// That function is JavaScript, and this is a Go program, so calling it is not an
// option: the tool has to hand the user a .excalidraw file that opens directly,
// not a skeleton file plus instructions to run npm. So the skeleton *format* is
// adopted as the thing this package authors, and the conversion documented on
// that page is implemented below.
//
// The win is in what the drawing code no longer says. A labelled box used to
// mean emitting two elements, inventing two ids, wiring containerId one way and
// boundElements the other, and keeping them consistent. Now it's:
//
//	{Type: "rectangle", X: x, Y: y, Width: w, Height: h, Label: &skeletonLabel{Text: "Start"}}
//
// The skeletons are also written to canvas.skeleton.json, so the same drawing
// can be fed to the real convertToExcalidrawElements() in a JS project.

// skeletonElement is one ExcalidrawElementSkeleton. Only the fields this package
// draws with are modelled; the rest of the documented surface (frames, images,
// magicframes) has no diagram to draw yet.
type skeletonElement struct {
	Type string `json:"type"`

	// ID is optional in the skeleton format. Supplying one is what lets an
	// arrow bind to this element by id rather than by inlining a copy of it.
	ID string `json:"id,omitempty"`

	X float64 `json:"x"`
	Y float64 `json:"y"`

	// Width and Height are optional: Excalidraw derives them from the label
	// when absent. This package always knows them — layout ran first — so they
	// are always set, which keeps the two renderings pixel-aligned.
	Width  float64 `json:"width,omitempty"`
	Height float64 `json:"height,omitempty"`

	// Label turns a shape into a text container, or an arrow into a labelled
	// arrow. The text element and both halves of the binding are derived.
	Label *skeletonLabel `json:"label,omitempty"`

	// Start and End bind an arrow to what it connects.
	Start *skeletonBinding `json:"start,omitempty"`
	End   *skeletonBinding `json:"end,omitempty"`

	// Text is the content of a standalone "text" element.
	Text     string  `json:"text,omitempty"`
	FontSize float64 `json:"fontSize,omitempty"`

	// Points is the outline of a "line", relative to X/Y.
	Points [][]float64 `json:"points,omitempty"`

	BackgroundColor string `json:"backgroundColor,omitempty"`
	StrokeColor     string `json:"strokeColor,omitempty"`

	// TextAlign and VerticalAlign apply to standalone text; a label carries its
	// own.
	TextAlign     string `json:"textAlign,omitempty"`
	VerticalAlign string `json:"verticalAlign,omitempty"`
}

// skeletonLabel is the documented label object. Only Text is required.
type skeletonLabel struct {
	Text          string  `json:"text"`
	FontSize      float64 `json:"fontSize,omitempty"`
	StrokeColor   string  `json:"strokeColor,omitempty"`
	TextAlign     string  `json:"textAlign,omitempty"`
	VerticalAlign string  `json:"verticalAlign,omitempty"`
}

// skeletonBinding is an arrow endpoint. The docs allow either a reference to an
// existing element by id, or an inline element to create. Only the by-id form
// is used here, because every shape this package draws already exists as its own
// skeleton — inlining would draw it twice.
type skeletonBinding struct {
	ID string `json:"id,omitempty"`
}

// convertToExcalidrawElements is the Go port of Excalidraw's function of the
// same name: it expands skeletons into fully qualified elements, deriving the
// pieces the skeleton format deliberately leaves out.
//
// Three derivations matter, and they are exactly the bookkeeping the skeleton
// format exists to remove:
//
//  1. A missing id becomes a stable generated one. Stable rather than random —
//     the upstream default is regenerateIds:true, but redrawing the same diagram
//     should produce the same file, not a scene that looks brand new every time.
//  2. A Label becomes a second text element bound to its container: the text
//     carries containerId, the container lists it in boundElements. Getting one
//     half of that wrong is what makes labels vanish on import.
//  3. Start/End become startBinding/endBinding on the arrow, and the arrow is
//     added to each target's boundElements. Without the second half the arrow
//     detaches the first time the user drags the shape.
func convertToExcalidrawElements(skeletons []skeletonElement) []map[string]any {
	// First pass: assign an id to everything, so bindings can be resolved
	// regardless of the order elements were declared in.
	ids := make([]string, len(skeletons))
	for i, s := range skeletons {
		if s.ID != "" {
			ids[i] = s.ID
			continue
		}
		ids[i] = fmt.Sprintf("%s-%d", s.Type, i+1)
	}

	elements := make([]map[string]any, 0, len(skeletons)*2)
	// bound accumulates the boundElements entries each element ends up owning,
	// which can only be known after every arrow and label has been seen.
	bound := map[string][]map[string]any{}
	byID := map[string]map[string]any{}

	for i, s := range skeletons {
		id := ids[i]
		el := exBase(id, s.Type, s.X, s.Y, s.Width, s.Height, exSeed(id))

		if s.BackgroundColor != "" {
			el["backgroundColor"] = s.BackgroundColor
		}
		if s.StrokeColor != "" {
			el["strokeColor"] = s.StrokeColor
		}

		switch s.Type {
		case "rectangle":
			el["roundness"] = map[string]any{"type": 3}
		case "text":
			applyTextFields(el, s.Text, orFloat(s.FontSize, exFontSize),
				orDefault(s.TextAlign, "left"), orDefault(s.VerticalAlign, "top"), "")
		case "line", "arrow":
			el["points"] = s.Points
			if s.Points == nil {
				el["points"] = [][]float64{{0, 0}, {round(s.Width), round(s.Height)}}
			}
			el["lastCommittedPoint"] = nil
			if s.Type == "arrow" {
				el["startArrowhead"] = nil
				el["endArrowhead"] = "arrow"
				el["roundness"] = map[string]any{"type": 2}
			}
		}

		// Derivation 3: arrow endpoints bind both ways.
		if s.Start != nil && s.Start.ID != "" {
			el["startBinding"] = map[string]any{"elementId": s.Start.ID, "focus": 0, "gap": 4}
			bound[s.Start.ID] = append(bound[s.Start.ID], map[string]any{"id": id, "type": s.Type})
		}
		if s.End != nil && s.End.ID != "" {
			el["endBinding"] = map[string]any{"elementId": s.End.ID, "focus": 0, "gap": 4}
			bound[s.End.ID] = append(bound[s.End.ID], map[string]any{"id": id, "type": s.Type})
		}

		elements = append(elements, el)
		byID[id] = el

		// Derivation 2: a label becomes a bound text element.
		if s.Label != nil && s.Label.Text != "" {
			textID := id + "-label"
			text := exBase(textID, "text", s.X, s.Y, s.Width, s.Height, exSeed(textID))
			applyTextFields(text, s.Label.Text,
				orFloat(s.Label.FontSize, exFontSize),
				orDefault(s.Label.TextAlign, "center"),
				orDefault(s.Label.VerticalAlign, "middle"),
				id)
			if s.Label.StrokeColor != "" {
				text["strokeColor"] = s.Label.StrokeColor
			}
			elements = append(elements, text)
			bound[id] = append(bound[id], map[string]any{"id": textID, "type": "text"})
		}
	}

	// Second pass: attach everything that accumulated against each element.
	for id, entries := range bound {
		if el, ok := byID[id]; ok {
			el["boundElements"] = entries
		}
	}
	return elements
}

// applyTextFields fills the properties every Excalidraw text element carries. A
// non-empty containerID makes it bound text — wrapped inside its container
// rather than sized to itself, which is why autoResize is off in that case.
func applyTextFields(el map[string]any, text string, fontSize float64, align, vAlign, containerID string) {
	el["text"] = text
	el["originalText"] = text
	el["fontSize"] = fontSize
	el["fontFamily"] = exHandFont
	el["textAlign"] = align
	el["verticalAlign"] = vAlign
	el["lineHeight"] = exLineHeight
	if containerID != "" {
		el["containerId"] = containerID
		el["autoResize"] = false
	} else {
		el["containerId"] = nil
		el["autoResize"] = true
	}
}

func orFloat(v, fallback float64) float64 {
	if v == 0 {
		return fallback
	}
	return v
}
