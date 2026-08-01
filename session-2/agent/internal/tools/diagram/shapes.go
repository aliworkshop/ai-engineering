package diagram

import (
	"fmt"
	"strings"
)

// The shape vocabulary.
//
// Every shape here is drawn from its bounding box, so the layout engine never
// has to know which one it's placing — it sizes a box around the label, applies
// the shape's aspect correction, and the renderer fills that box. Adding a
// shape means adding a row to these three functions and nothing else.

// normalizeShape maps what a model actually writes onto the shapes we draw,
// falling back to a plain box for anything unrecognised.
func normalizeShape(s string) string {
	shape, _ := lookupShape(s)
	return shape
}

// lookupShape is normalizeShape with the "did I actually recognise this?"
// answer kept, which is what lets a shape word appearing in the *type* field be
// told apart from a genuine unknown. Models reach for domain words
// ("database", "decision", "start") far more often than geometry, so the
// synonyms matter more than the canonical names.
func lookupShape(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ellipse", "oval", "terminator", "start", "end":
		return "ellipse", true
	case "circle", "round", "dot", "state":
		return "circle", true
	case "diamond", "decision", "rhombus", "condition", "if":
		return "diamond", true
	case "hexagon", "hex", "preparation":
		return "hexagon", true
	case "parallelogram", "input", "output", "io", "data":
		return "parallelogram", true
	case "cylinder", "database", "db", "store", "storage", "disk":
		return "cylinder", true
	case "document", "doc", "report", "file":
		return "document", true
	case "note", "comment", "annotation", "sticky":
		return "note", true
	case "triangle", "delta", "warning":
		return "triangle", true
	case "cloud", "internet", "external", "service":
		return "cloud", true
	case "pill", "stadium", "capsule", "rounded":
		return "pill", true
	default:
		return "box", false
	}
}

// shapeAspect corrects the label-derived bounding box for shapes whose corners
// or curves eat space the text can't use. A diamond needs half again its text
// width or the label spills out of the points.
func shapeAspect(shape string) (w, h float64) {
	switch shape {
	case "diamond":
		return 1.5, 1.7
	case "ellipse":
		return 1.15, 1.15
	case "cloud":
		// The cloud's arcs sit inside their bounding box rather than filling it,
		// so a label sized against the box alone overflows the outline. Measured
		// against the drawn path, not the box.
		return 1.3, 1.5
	case "circle":
		return 1.3, 1.3
	case "hexagon", "parallelogram":
		return 1.25, 1.0
	case "triangle":
		return 1.6, 1.5
	case "cylinder", "document":
		return 1.0, 1.25
	default: // box, note, pill
		return 1.0, 1.0
	}
}

// shapeClass colours a shape by the role it conventionally plays, so a diagram
// reads at a glance without the caller having to specify styling.
func shapeClass(shape string) string {
	switch shape {
	case "ellipse", "circle", "pill":
		return "term" // green: entry and exit points
	case "diamond", "hexagon", "triangle":
		return "decide" // amber: something branches here
	case "cylinder", "document", "note", "parallelogram", "cloud":
		return "data" // violet: data at rest, in transit, or off-system
	default:
		return "shape" // blue: a step
	}
}

// shapeSVG draws the outline. Everything is derived from the bounding box, so
// the shapes stay consistent with each other at any size.
func shapeSVG(n *diagramNode) string {
	x, y, w, h := n.x, n.y, n.w, n.h
	cx, cy := x+w/2, y+h/2
	class := shapeClass(n.shape)

	switch n.shape {
	case "ellipse":
		return fmt.Sprintf(`<ellipse class="%s" cx="%.1f" cy="%.1f" rx="%.1f" ry="%.1f"/>`+"\n",
			class, cx, cy, w/2, h/2)

	case "circle":
		r := minFloat(w, h) / 2
		return fmt.Sprintf(`<circle class="%s" cx="%.1f" cy="%.1f" r="%.1f"/>`+"\n", class, cx, cy, r)

	case "diamond":
		return polygonSVG(class, cx, y, x+w, cy, cx, y+h, x, cy)

	case "hexagon":
		inset := w * 0.18
		return polygonSVG(class, x+inset, y, x+w-inset, y, x+w, cy, x+w-inset, y+h, x+inset, y+h, x, cy)

	case "parallelogram":
		skew := w * 0.18
		return polygonSVG(class, x+skew, y, x+w, y, x+w-skew, y+h, x, y+h)

	case "triangle":
		return polygonSVG(class, cx, y, x+w, y+h, x, y+h)

	case "pill":
		return fmt.Sprintf(`<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.1f"/>`+"\n",
			class, x, y, w, h, h/2)

	case "cylinder":
		// A database: an open-ended tube with an ellipse cap. The body is drawn
		// first, then the top rim over it, so the seam doesn't show.
		ry := h * 0.12
		return fmt.Sprintf(
			`<path class="%s" d="M %.1f %.1f V %.1f A %.1f %.1f 0 0 0 %.1f %.1f V %.1f"/>`+"\n"+
				`<ellipse class="%s" cx="%.1f" cy="%.1f" rx="%.1f" ry="%.1f"/>`+"\n",
			class, x, y+ry, y+h-ry, w/2, ry, x+w, y+h-ry, y+ry,
			class, cx, y+ry, w/2, ry)

	case "document":
		// A rectangle whose bottom edge waves, the classic "report" glyph.
		wave := h * 0.14
		return fmt.Sprintf(
			`<path class="%s" d="M %.1f %.1f H %.1f V %.1f Q %.1f %.1f %.1f %.1f T %.1f %.1f Z"/>`+"\n",
			class, x, y, x+w, y+h-wave,
			x+w*0.75, y+h, x+w*0.5, y+h-wave*0.6,
			x, y+h-wave)

	case "note":
		// A sticky note: rectangle with the top-right corner folded over.
		fold := minFloat(w, h) * 0.22
		return fmt.Sprintf(
			`<path class="%s" d="M %.1f %.1f H %.1f L %.1f %.1f V %.1f H %.1f Z"/>`+"\n"+
				`<path class="fold" d="M %.1f %.1f L %.1f %.1f H %.1f Z"/>`+"\n",
			class, x, y, x+w-fold, x+w, y+fold, y+h, x,
			x+w-fold, y, x+w, y+fold, x+w-fold)

	case "cloud":
		// Four lobes around the full bounding box. The earlier version traced
		// its arcs through only the lower band of the box, so a label centred on
		// the box sat outside the outline — the shape has to fill what it's
		// measured against.
		p := func(fx, fy float64) string {
			return fmt.Sprintf("%.1f %.1f", x+w*fx, y+h*fy)
		}
		return fmt.Sprintf(
			`<path class="%s" d="M %s C %s, %s, %s C %s, %s, %s C %s, %s, %s C %s, %s, %s Z"/>`+"\n",
			class,
			p(0.12, 0.88),
			p(-0.04, 0.86), p(-0.03, 0.50), p(0.14, 0.46),
			p(0.13, 0.12), p(0.48, 0.02), p(0.56, 0.28),
			p(0.74, 0.12), p(1.02, 0.26), p(0.92, 0.52),
			p(1.06, 0.62), p(1.02, 0.88), p(0.88, 0.88))

	default: // box
		return fmt.Sprintf(`<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="8"/>`+"\n",
			class, x, y, w, h)
	}
}

// polygonSVG builds a closed polygon from flat x,y pairs.
func polygonSVG(class string, coords ...float64) string {
	var pts strings.Builder
	for i := 0; i+1 < len(coords); i += 2 {
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", coords[i], coords[i+1])
	}
	return fmt.Sprintf(`<polygon class="%s" points="%s"/>`+"\n", class, pts.String())
}

// exShapePoints returns the outline of a shape as points relative to its own
// top-left corner, for the shapes Excalidraw has no primitive for. Excalidraw
// draws these as a closed "line" element, which stays editable — the user can
// drag any vertex after importing.
//
// Returning nil means "Excalidraw has a native primitive for this", and
// exShape uses that instead.
func exShapePoints(shape string, w, h float64) [][]float64 {
	switch shape {
	case "hexagon":
		i := w * 0.18
		return closed([][]float64{{i, 0}, {w - i, 0}, {w, h / 2}, {w - i, h}, {i, h}, {0, h / 2}})
	case "parallelogram":
		s := w * 0.18
		return closed([][]float64{{s, 0}, {w, 0}, {w - s, h}, {0, h}})
	case "triangle":
		return closed([][]float64{{w / 2, 0}, {w, h}, {0, h}})
	case "note":
		f := minFloat(w, h) * 0.22
		return closed([][]float64{{0, 0}, {w - f, 0}, {w, f}, {w, h}, {0, h}})
	case "document":
		return closed([][]float64{{0, 0}, {w, 0}, {w, h * 0.86}, {w * 0.5, h}, {0, h * 0.86}})
	case "cylinder":
		// Excalidraw has no arc, so a database reads as a tall hexagonal tube.
		r := h * 0.12
		return closed([][]float64{{0, r}, {w * 0.5, 0}, {w, r}, {w, h - r}, {w * 0.5, h}, {0, h - r}})
	default:
		return nil
	}
}

func closed(pts [][]float64) [][]float64 {
	return append(pts, []float64{pts[0][0], pts[0][1]})
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
