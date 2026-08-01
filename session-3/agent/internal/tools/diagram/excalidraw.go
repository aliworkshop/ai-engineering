package diagram

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
// The drawing is authored as Excalidraw element *skeletons* (see skeleton.go)
// and expanded by convertToExcalidrawElements. This file therefore says what to
// draw; none of the bookkeeping — ids, seeds, containerId/boundElements pairs,
// two-way arrow bindings — appears here at all.
//
// The layout is shared with the SVG renderer: same layoutDiagram pass, same
// coordinates. Only the serialization differs, so the two files always show the
// same picture.

const (
	excalidrawFile   = "canvas.excalidraw"
	skeletonFile     = "canvas.skeleton.json"
	excalidrawSource = "https://excalidraw.com"

	// Excalidraw's own palette, so an imported scene looks native rather than
	// like something pasted in from another tool.
	exStroke     = "#1e1e1e"
	exBlueFill   = "#a5d8ff"
	exGreenFill  = "#b2f2bb"
	exYellowFill = "#ffec99"
	exVioletFill = "#d0bfff"

	exFontSize   = 20.0
	exLineHeight = 1.25
	// fontFamily 1 is Excalidraw's hand-drawn face; 2 is Helvetica, 3 is code.
	exHandFont = 1
)

// buildSkeletons describes the laid-out diagram in Excalidraw's skeleton form.
// Every node and edge becomes one or more skeletons; nothing here computes an
// id, a binding or a bound text element.
func buildSkeletons(title string, nodes []*diagramNode, edges []diagramEdge) []skeletonElement {
	var skels []skeletonElement

	if title != "" {
		skels = append(skels, skeletonElement{
			Type: "text", X: diagramMargin, Y: diagramMargin - 14,
			Text: title, FontSize: 28,
		})
	}

	// Ids are needed only where something binds to the element — which means
	// the shapes an arrow can attach to. Everything else is left unnamed and
	// the converter names it.
	idOf := make(map[*diagramNode]string, len(nodes))
	for i, n := range nodes {
		idOf[n] = fmt.Sprintf("node-%d", i+1)
	}

	for _, n := range nodes {
		// Tables and charts expand into many primitives rather than one shape,
		// so the result stays editable in the app — a cell you can click and
		// retype, a bar you can drag taller.
		switch n.kind {
		case kindTable:
			skels = append(skels, tableSkeletons(n)...)
			continue
		case kindChart:
			skels = append(skels, chartSkeletons(n)...)
			continue
		}
		skels = append(skels, shapeSkeleton(idOf[n], n))
	}

	for _, e := range edges {
		x1, y1, x2, y2 := edgeAnchors(e)
		arrow := skeletonElement{
			Type: "arrow",
			X:    x1, Y: y1,
			Width: x2 - x1, Height: y2 - y1,
			// Explicit points as well as width/height. A bound arrow gets
			// re-routed from its endpoints anyway, but an unbound one — the
			// arrows that touch a table or chart — is drawn from these, and
			// width alone loses the sign of an arrow pointing left or up.
			Points: [][]float64{{0, 0}, {round(x2 - x1), round(y2 - y1)}},
			Start:  &skeletonBinding{ID: idOf[e.from]},
			End:    &skeletonBinding{ID: idOf[e.to]},
		}
		// A labelled arrow is a first-class skeleton, so the text rides the
		// arrow and moves with it. The previous hand-built export dropped edge
		// labels as loose text at the midpoint, which drifted away from the
		// arrow the moment anything was dragged.
		if e.label != "" {
			arrow.Label = &skeletonLabel{Text: e.label, FontSize: 16}
		}
		if !bindable(e.from) {
			arrow.Start = nil
		}
		if !bindable(e.to) {
			arrow.End = nil
		}
		skels = append(skels, arrow)
	}
	return skels
}

// bindable reports whether an arrow may name this node as an endpoint.
//
// Excalidraw binds arrows only to its closed primitives — rectangle, ellipse,
// diamond. Naming anything else crashes convertToExcalidrawElements with
// "Unhandled element start type undefined", because it looks the target up and
// finds a type its binding switch has no case for. Two kinds of node fall
// outside that set:
//
//   - shapes drawn as a closed line (cylinder, hexagon, note, …)
//   - tables and charts, which are many primitives with no single element
//
// An unbound arrow still draws, using its own points; it just doesn't follow
// the shape when the user drags it afterwards.
func bindable(n *diagramNode) bool {
	if n.kind != kindShape {
		return false
	}
	return exShapePoints(n.shape, n.w, n.h) == nil
}

// shapeSkeleton maps a diagram box onto the matching skeleton. Excalidraw has
// three closed primitives; anything else is drawn as a closed "line" through the
// shape's own outline, which keeps it a real editable object — every vertex is
// draggable after importing — rather than being flattened to a rectangle.
func shapeSkeleton(id string, n *diagramNode) skeletonElement {
	s := skeletonElement{
		Type: "rectangle",
		ID:   id,
		X:    n.x, Y: n.y, Width: n.w, Height: n.h,
		BackgroundColor: exFillForClass(shapeClass(n.shape)),
		Label:           &skeletonLabel{Text: strings.Join(n.lines, "\n")},
	}

	switch n.shape {
	case "ellipse", "circle", "cloud":
		s.Type = "ellipse"
	case "diamond":
		s.Type = "diamond"
	default:
		if pts := exShapePoints(n.shape, n.w, n.h); pts != nil {
			s.Type = "line"
			s.Points = pts
			// A line can't contain bound text, so the label becomes its own
			// centred text element instead.
			s.Label = nil
		}
	}
	return s
}

// labelSkeleton is a free-standing text element centred on a box, for the
// shapes that can't contain bound text.
//
// With textAlign/verticalAlign centred, x and y are the *anchor* the text is
// centred on, not its top-left corner — Excalidraw measures the string and
// offsets it itself. Subtracting an estimated half-width here (an earlier
// attempt did) shifts the label off by exactly that estimate, which is why
// "Events DB" sat 27px left of its shape.
func labelSkeleton(n *diagramNode) skeletonElement {
	return skeletonElement{
		Type: "text",
		X:    n.x + n.w/2,
		Y:    n.y + n.h/2,
		Text: strings.Join(n.lines, "\n"), FontSize: exFontSize,
		TextAlign: "center", VerticalAlign: "middle",
	}
}

// exFillForClass keeps the Excalidraw palette in step with the SVG's role
// colours, so a database is the same colour in both renderings.
func exFillForClass(class string) string {
	switch class {
	case "term":
		return exGreenFill
	case "decide":
		return exYellowFill
	case "data":
		return exVioletFill
	default:
		return exBlueFill
	}
}

// allSkeletons is the complete authoring form of a laid-out diagram: the
// skeletons for every node and edge, plus the free-standing labels belonging to
// shapes that can't contain bound text.
func allSkeletons(title string, nodes []*diagramNode, edges []diagramEdge) []skeletonElement {
	skels := buildSkeletons(title, nodes, edges)
	for _, n := range nodes {
		if n.kind == kindShape && exShapePoints(n.shape, n.w, n.h) != nil {
			skels = append(skels, labelSkeleton(n))
		}
	}
	return skels
}

// renderExcalidraw converts the skeletons into a scene file excalidraw.com can
// open directly. It's the fallback path — when the Excalidraw renderer is
// installed, the library produces the scene itself.
func renderExcalidraw(title string, nodes []*diagramNode, edges []diagramEdge) (string, error) {
	skels := allSkeletons(title, nodes, edges)

	scene := map[string]any{
		"type":     "excalidraw",
		"version":  2,
		"source":   excalidrawSource,
		"elements": convertToExcalidrawElements(skels),
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
