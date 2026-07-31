package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Excalidraw export.
//
// The .excalidraw format is a plain JSON scene file: excalidraw.com opens it
// with File → Open (or a drag and drop onto the canvas), and everything in it
// stays editable — drag a box and its arrows follow, retype a label, restyle
// the lot. That editability is the whole point of exporting here rather than
// only rendering a flat SVG.
//
// The layout is shared with the SVG renderer: same layerDiagram pass, same
// coordinates. Only the serialization differs, so the two files always show the
// same picture.

// excalidrawFile is the scene wrapper excalidraw.com expects.
const (
	excalidrawFile   = "canvas.excalidraw"
	excalidrawSource = "https://excalidraw.com"

	// Excalidraw's own palette, so an imported scene looks native rather than
	// like something pasted in from another tool.
	exStroke     = "#1e1e1e"
	exBlueFill   = "#a5d8ff"
	exGreenFill  = "#b2f2bb"
	exYellowFill = "#ffec99"

	exFontSize   = 20.0
	exLineHeight = 1.25
	// fontFamily 1 is Excalidraw's hand-drawn face; 2 is Helvetica, 3 is code.
	exHandFont = 1
)

// renderExcalidraw serializes the laid-out diagram as an Excalidraw scene.
func renderExcalidraw(title string, nodes []*diagramNode, edges []diagramEdge) (string, error) {
	// ids and seeds must be stable across redraws, otherwise re-importing a
	// scene looks like a set of brand new elements rather than the same diagram.
	// A counter gives us that without needing randomness, which the layout code
	// deliberately avoids anyway.
	seq := 0
	nextID := func(prefix string) string {
		seq++
		return fmt.Sprintf("%s-%d", prefix, seq)
	}

	elements := make([]map[string]any, 0, len(nodes)*2+len(edges)*2+1)
	idOf := make(map[*diagramNode]string, len(nodes))

	if title != "" {
		elements = append(elements, exText(nextID("title"), title,
			diagramMargin, diagramMargin-14, 28, "left", "", ""))
	}

	for _, n := range nodes {
		boxID := nextID("shape")
		textID := nextID("label")
		idOf[n] = boxID

		elements = append(elements,
			exShape(boxID, n, textID),
			// A bound label carries containerId; Excalidraw then keeps it
			// centred in the shape. Feed it the same line breaks the SVG uses,
			// so both files break a long label in the same places instead of
			// letting each renderer wrap it its own way.
			exText(textID, strings.Join(n.lines, "\n"), n.x, n.y, exFontSize, "center", "middle", boxID),
		)
	}

	for _, e := range edges {
		elements = append(elements, exArrow(nextID("arrow"), e, idOf[e.from], idOf[e.to]))
		if e.label != "" {
			// Edge labels ride along as free text at the midpoint; binding text
			// to an arrow needs the arrow's own label slot, which older
			// Excalidraw builds ignore, so plain text is the portable choice.
			lx, ly := edgeLabelAnchor(e)
			elements = append(elements, exText(nextID("edgelabel"), e.label, lx, ly, 16, "center", "middle", ""))
		}
	}

	scene := map[string]any{
		"type":    "excalidraw",
		"version": 2,
		"source":  excalidrawSource,
		"elements": func() []map[string]any {
			return elements
		}(),
		"appState": map[string]any{
			"gridSize":            nil,
			"viewBackgroundColor": "#ffffff",
		},
		"files": map[string]any{},
	}

	out, err := json.MarshalIndent(scene, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// exBase is the field set every Excalidraw element carries. Leaving any of them
// out makes older builds of the app fall over on import, so they're all spelled
// out rather than relying on defaults.
func exBase(id, typ string, x, y, w, h float64, seed int) map[string]any {
	return map[string]any{
		"id":              id,
		"type":            typ,
		"x":               round(x),
		"y":               round(y),
		"width":           round(w),
		"height":          round(h),
		"angle":           0,
		"strokeColor":     exStroke,
		"backgroundColor": "transparent",
		"fillStyle":       "solid",
		"strokeWidth":     2,
		"strokeStyle":     "solid",
		"roughness":       1, // 1 = "artist": hand-drawn, the reason to use Excalidraw
		"opacity":         100,
		"groupIds":        []string{},
		"frameId":         nil,
		"roundness":       nil,
		"seed":            seed,
		"version":         1,
		"versionNonce":    seed,
		"isDeleted":       false,
		"boundElements":   nil,
		"updated":         1,
		"link":            nil,
		"locked":          false,
	}
}

// exShape maps a diagram box onto the matching Excalidraw primitive, coloured
// by role the same way the SVG is: green terminators, amber decisions, blue
// steps.
func exShape(id string, n *diagramNode, textID string) map[string]any {
	typ, fill := "rectangle", exBlueFill
	switch n.shape {
	case "ellipse":
		typ, fill = "ellipse", exGreenFill
	case "diamond":
		typ, fill = "diamond", exYellowFill
	}

	el := exBase(id, typ, n.x, n.y, n.w, n.h, exSeed(id))
	el["backgroundColor"] = fill
	if typ == "rectangle" {
		el["roundness"] = map[string]any{"type": 3} // adaptive rounded corners
	}
	el["boundElements"] = []map[string]any{{"id": textID, "type": "text"}}
	return el
}

// exText builds a text element. A non-empty containerID binds it inside a
// shape; otherwise it's free-standing (the title and edge labels).
func exText(id, text string, x, y, fontSize float64, align, vAlign, containerID string) map[string]any {
	lines := 1
	for _, r := range text {
		if r == '\n' {
			lines++
		}
	}
	w := float64(len([]rune(text)))*fontSize*0.55 + 8
	h := float64(lines) * fontSize * exLineHeight

	el := exBase(id, "text", x, y, w, h, exSeed(id))
	el["text"] = text
	el["originalText"] = text
	el["fontSize"] = fontSize
	el["fontFamily"] = exHandFont
	el["textAlign"] = align
	el["verticalAlign"] = orDefault(vAlign, "top")
	el["lineHeight"] = exLineHeight
	if containerID != "" {
		el["containerId"] = containerID
		// Bound text must wrap to its container rather than stretch it — with
		// autoResize on, a long label grows the shape and the layout this tool
		// just computed stops matching the SVG.
		el["autoResize"] = false
	} else {
		el["containerId"] = nil
		el["autoResize"] = true
	}
	return el
}

// exArrow connects two shapes. The bindings are what make the arrow stay
// attached when the user drags a box around after importing — without them the
// scene imports as loose lines that drift away from their boxes on the first
// edit.
func exArrow(id string, e diagramEdge, fromID, toID string) map[string]any {
	x1, y1, x2, y2 := edgeAnchors(e)

	el := exBase(id, "arrow", x1, y1, x2-x1, y2-y1, exSeed(id))
	el["roundness"] = map[string]any{"type": 2}
	el["points"] = [][]float64{
		{0, 0},
		{round(x2 - x1), round(y2 - y1)},
	}
	el["lastCommittedPoint"] = nil
	el["startArrowhead"] = nil
	el["endArrowhead"] = "arrow"
	el["startBinding"] = map[string]any{"elementId": fromID, "focus": 0, "gap": 4}
	el["endBinding"] = map[string]any{"elementId": toID, "focus": 0, "gap": 4}
	return el
}

// edgeAnchors returns where an arrow starts and ends, matching the SVG
// renderer: down the flow it leaves the bottom and enters the top; a back edge
// leaves and enters on the right.
func edgeAnchors(e diagramEdge) (x1, y1, x2, y2 float64) {
	from, to := e.from, e.to
	if to.layer > from.layer {
		return from.x + from.w/2, from.y + from.h,
			to.x + to.w/2, to.y
	}
	return from.x + from.w, from.y + from.h/2,
		to.x + to.w, to.y + to.h/2
}

func edgeLabelAnchor(e diagramEdge) (float64, float64) {
	x1, y1, x2, y2 := edgeAnchors(e)
	return (x1 + x2) / 2, (y1 + y2) / 2
}

// exSeed derives a stable per-element seed from its id, so redrawing the same
// diagram produces the same hand-drawn jitter instead of reshuffling it.
func exSeed(id string) int {
	h := 0
	for _, r := range id {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return h%1_000_000 + 1
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// round keeps the JSON readable — sub-pixel precision means nothing here and
// full float noise makes the file miserable to diff.
func round(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
