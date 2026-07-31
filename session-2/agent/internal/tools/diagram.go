package tools

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OpenRouterTeam/go-sdk/models/components"
)

// GenerateDiagram draws a whole diagram in one call and writes it to an SVG the
// user can open in a browser and refresh after each redraw.
//
// The model supplies only *structure* — boxes with labels, and arrows between
// them by id. It never supplies coordinates: asking a language model for pixel
// geometry produces overlapping boxes and crossed arrows, and it makes every
// redraw a fresh chance to get the arithmetic wrong. Placement is this tool's
// job (see layoutDiagram), so "draw a flowchart of user signup" only has to be
// answered at the level the model is actually good at.
//
// It writes one fixed filename in the working directory rather than a caller
// supplied path, so it can't be steered into overwriting arbitrary files and
// doesn't need the human-in-the-loop gate on every redraw.
type GenerateDiagram struct {
	// Dir is where canvas.svg is written. Empty means the working directory.
	Dir string
}

// canvasFile is the single file this tool ever writes. Keeping it constant is
// what makes the tool safe to run unattended: there is no path to traverse.
const canvasFile = "canvas.svg"

// diagramElement is one entry in the elements array. It's a union in the loose
// JSON sense: "box" uses id/label/shape, "arrow" uses from/to/label. Modelling
// both as one flat struct keeps the schema simple enough for a model to fill in
// reliably, which matters more here than type purity.
type diagramElement struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Label string `json:"label"`
	Shape string `json:"shape"`
	From  string `json:"from"`
	To    string `json:"to"`
}

func (GenerateDiagram) Spec() components.ChatFunctionTool {
	return defineTool("generate_diagram",
		"Draw a complete diagram (flowchart, sequence of steps, architecture sketch) in ONE call. Writes canvas.svg (open in a browser, refresh to see changes) and canvas.excalidraw (open at excalidraw.com to edit by hand). Boxes AND arrows all go in the single `elements` array — there is no separate arrows parameter. Do NOT pass coordinates; the tool lays the diagram out itself.",
		`{
  "type": "object",
  "properties": {
    "title": {
      "type": "string",
      "description": "Optional heading drawn at the top of the canvas, e.g. 'User signup'"
    },
    "elements": {
      "type": "array",
      "description": "Every box and arrow in the diagram, in one array.",
      "items": {
        "type": "object",
        "properties": {
          "type": {
            "type": "string",
            "enum": ["box", "arrow"],
            "description": "'box' is a node; 'arrow' connects two boxes."
          },
          "id": {
            "type": "string",
            "description": "For a box: a short unique id other elements refer to, e.g. 'start'."
          },
          "label": {
            "type": "string",
            "description": "For a box: the text inside it. For an arrow: an optional edge label, e.g. 'yes' / 'no'."
          },
          "shape": {
            "type": "string",
            "enum": ["box", "ellipse", "diamond"],
            "description": "Box shape. Use 'ellipse' for start/end, 'diamond' for a decision, 'box' (default) for a step."
          },
          "from": {"type": "string", "description": "For an arrow: the id of the source box."},
          "to": {"type": "string", "description": "For an arrow: the id of the target box."}
        },
        "required": ["type"]
      }
    }
  },
  "required": ["elements"]
}`)
}

func (t GenerateDiagram) Run(ctx context.Context, args string) (string, error) {
	var a struct {
		Title string           `json:"title"`
		Nodes []diagramElement `json:"elements"`
		// Arrows is not in the schema, but models reach for a separate array
		// anyway — it reads as the natural shape for "boxes and arrows". Left
		// unread, those arrows vanish and the caller gets a page of unconnected
		// boxes reported as a success. Accepting the key costs one line and
		// removes the whole failure mode.
		Arrows []diagramElement `json:"arrows"`
		Edges  []diagramElement `json:"edges"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}

	elements := a.Nodes
	for _, extra := range [][]diagramElement{a.Arrows, a.Edges} {
		for _, e := range extra {
			e.Type = "arrow" // whatever the key said it was, this is an arrow
			elements = append(elements, e)
		}
	}

	boxes, arrows, err := drawDiagram(t.Dir, diagramSpec{Title: a.Title, Elements: elements})
	if err != nil {
		// Returned as an error so Dispatch hands the model readable text it can
		// act on — a mistyped id is worth one retry, not a dead end.
		return "", err
	}
	return fmt.Sprintf("Drew %d boxes and %d arrows.\n%s", boxes, arrows, canvasLocations(t.Dir)), nil
}

// drawDiagram validates a spec, lays it out, and writes all three files: the
// SVG to refresh in a browser, the Excalidraw scene to edit by hand, and the
// spec itself so modify_diagram has something to change later.
//
// Both tools go through here, which is what keeps a modified diagram identical
// to one drawn from scratch with the same elements.
func drawDiagram(dir string, spec diagramSpec) (boxes, arrows int, err error) {
	nodes, edges, err := buildDiagram(spec.Elements)
	if err != nil {
		return 0, 0, err
	}

	width, height := layoutDiagram(nodes, edges, spec.Title != "")

	// Two renderings from one layout, so they can never disagree about the
	// picture — only the serialization differs.
	if err := os.WriteFile(filepath.Join(dir, canvasFile),
		[]byte(renderSVG(spec.Title, nodes, edges, width, height)), 0o644); err != nil {
		return 0, 0, err
	}

	scene, err := renderExcalidraw(spec.Title, nodes, edges)
	if err != nil {
		return 0, 0, err
	}
	if err := os.WriteFile(filepath.Join(dir, excalidrawFile), []byte(scene), 0o644); err != nil {
		return 0, 0, err
	}

	// Saved last, and only once the renders succeeded, so the spec on disk
	// always describes the picture that's actually there.
	if err := saveSpec(dir, spec); err != nil {
		return 0, 0, err
	}
	return len(nodes), len(edges), nil
}

func canvasLocations(dir string) string {
	return fmt.Sprintf("%s — open in a browser, refresh after each redraw.\n%s — open at excalidraw.com (File → Open, or drag it onto the canvas) to edit it by hand.",
		absPath(filepath.Join(dir, canvasFile)), absPath(filepath.Join(dir, excalidrawFile)))
}

// absPath reports the file's absolute location so the agent can hand the user
// something they can paste straight into a browser.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// diagramNode is a box after layout: identity and label from the model,
// geometry from layoutDiagram.
type diagramNode struct {
	id, label, shape string
	lines            []string // label wrapped for rendering
	layer            int
	order            int // position within its layer, for stable placement
	x, y, w, h       float64
}

// diagramEdge is an arrow between two laid-out boxes.
type diagramEdge struct {
	from, to *diagramNode
	label    string
}

// buildDiagram turns the flat element list into nodes and edges, rejecting the
// mistakes a model actually makes: boxes without ids, duplicate ids, and arrows
// pointing at boxes that were never declared.
func buildDiagram(elements []diagramElement) ([]*diagramNode, []diagramEdge, error) {
	byID := map[string]*diagramNode{}
	var nodes []*diagramNode

	for i, e := range elements {
		if elementKind(e) != "box" {
			continue
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			return nil, nil, fmt.Errorf("elements[%d] is a box with no id; every box needs a unique id for arrows to reference", i)
		}
		if _, dup := byID[id]; dup {
			return nil, nil, fmt.Errorf("duplicate box id %q; ids must be unique", id)
		}
		label := strings.TrimSpace(e.Label)
		if label == "" {
			label = id
		}
		n := &diagramNode{id: id, label: label, shape: normalizeShape(e.Shape), lines: wrapLabel(label, 20)}
		byID[id] = n
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("no boxes in elements; a diagram needs at least one element with type \"box\"")
	}

	var edges []diagramEdge
	for i, e := range elements {
		if elementKind(e) != "arrow" {
			continue
		}
		from, okFrom := byID[strings.TrimSpace(e.From)]
		to, okTo := byID[strings.TrimSpace(e.To)]
		if !okFrom || !okTo {
			missing := e.From
			if okFrom {
				missing = e.To
			}
			return nil, nil, fmt.Errorf("elements[%d] is an arrow to unknown box id %q; declared ids are %s",
				i, missing, strings.Join(sortedIDs(byID), ", "))
		}
		edges = append(edges, diagramEdge{from: from, to: to, label: strings.TrimSpace(e.Label)})
	}

	// A multi-box diagram with nothing joining it is almost always a caller that
	// put its arrows somewhere this tool didn't look. Drawing it anyway produces
	// a page of loose boxes reported as a success, which is worse than failing:
	// say so and let the model correct itself.
	if len(nodes) > 1 && len(edges) == 0 {
		return nil, nil, fmt.Errorf("%d boxes but no arrows, so nothing would be connected; put the arrows in the same `elements` array as the boxes, each as {\"type\":\"arrow\",\"from\":\"<id>\",\"to\":\"<id>\"}", len(nodes))
	}
	return nodes, edges, nil
}

// elementKind reports whether an element is a box or an arrow, inferring it
// when "type" is absent or misspelled. from/to means an arrow; a bare id means
// a box. Models are inconsistent about this field and the intent is never
// ambiguous, so guessing beats rejecting.
func elementKind(e diagramElement) string {
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "box", "node", "shape":
		return "box"
	case "arrow", "edge", "link", "connection":
		return "arrow"
	}
	if strings.TrimSpace(e.From) != "" && strings.TrimSpace(e.To) != "" {
		return "arrow"
	}
	if strings.TrimSpace(e.ID) != "" || strings.TrimSpace(e.Label) != "" {
		return "box"
	}
	return ""
}

func sortedIDs(byID map[string]*diagramNode) []string {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func normalizeShape(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ellipse", "oval", "round", "terminator":
		return "ellipse"
	case "diamond", "decision", "rhombus":
		return "diamond"
	default:
		return "box"
	}
}

// Layout geometry. Everything is in SVG user units (≈ CSS pixels).
const (
	diagramMargin  = 44.0
	layerGap       = 76.0 // vertical space between rows of boxes
	siblingGap     = 44.0 // horizontal space between boxes in one row
	lineHeight     = 19.0
	charWidth      = 8.2 // rough advance width of the label font at 14px
	minBoxWidth    = 132.0
	titleBandHeigh = 56.0
)

// layoutDiagram assigns every node a layer (how far it is from a starting box),
// then places the layers as centred rows. It returns the canvas size.
//
// Layering is the longest path from any root, computed by relaxing edges until
// nothing moves — but only over *forward* edges. Back edges are excluded first,
// because a retry loop ("invalid input → back to the form") would otherwise
// push its own targets down a layer on every pass, stretching an 8-box chart
// across 15 rows of mostly empty canvas. The back edges are still drawn; they
// just don't get a vote on depth.
func layoutDiagram(nodes []*diagramNode, edges []diagramEdge, hasTitle bool) (float64, float64) {
	for _, n := range nodes {
		n.w, n.h = boxSize(n)
	}

	back := findBackEdges(nodes, edges)
	for range nodes {
		moved := false
		for i, e := range edges {
			if back[i] {
				continue
			}
			if e.to.layer < e.from.layer+1 {
				e.to.layer = e.from.layer + 1
				moved = true
			}
		}
		if !moved {
			break
		}
	}

	// Group into rows, preserving declaration order inside each row so the
	// picture matches the order the model listed things in.
	rows := map[int][]*diagramNode{}
	maxLayer := 0
	for _, n := range nodes {
		rows[n.layer] = append(rows[n.layer], n)
		if n.layer > maxLayer {
			maxLayer = n.layer
		}
	}

	// Widest row decides the canvas width; every row is then centred in it.
	canvasWidth := 0.0
	for layer := 0; layer <= maxLayer; layer++ {
		row := rows[layer]
		total := 0.0
		for i, n := range row {
			total += n.w
			if i > 0 {
				total += siblingGap
			}
			n.order = i
		}
		if w := total + 2*diagramMargin; w > canvasWidth {
			canvasWidth = w
		}
	}

	top := diagramMargin
	if hasTitle {
		top += titleBandHeigh
	}
	rowTop := top
	for layer := 0; layer <= maxLayer; layer++ {
		row := rows[layer]
		total := 0.0
		rowHeight := 0.0
		for i, n := range row {
			total += n.w
			if i > 0 {
				total += siblingGap
			}
			if n.h > rowHeight {
				rowHeight = n.h
			}
		}
		x := (canvasWidth - total) / 2
		for _, n := range row {
			n.x = x
			n.y = rowTop + (rowHeight-n.h)/2 // centre shorter boxes in the row
			x += n.w + siblingGap
		}
		rowTop += rowHeight + layerGap
	}

	canvasHeight := rowTop - layerGap + diagramMargin
	return canvasWidth, canvasHeight
}

// findBackEdges returns the indices of edges that close a cycle, found by a
// depth-first walk: an edge into a node still on the current path points back
// up the flow. Roots are walked first so the tree edges follow the direction
// the diagram actually reads in, and self-loops fall out as back edges too.
func findBackEdges(nodes []*diagramNode, edges []diagramEdge) map[int]bool {
	const (
		unvisited = iota
		onPath
		done
	)
	outgoing := map[*diagramNode][]int{}
	indegree := map[*diagramNode]int{}
	for i, e := range edges {
		outgoing[e.from] = append(outgoing[e.from], i)
		indegree[e.to]++
	}

	state := map[*diagramNode]int{}
	back := map[int]bool{}

	var walk func(*diagramNode)
	walk = func(n *diagramNode) {
		state[n] = onPath
		for _, i := range outgoing[n] {
			switch state[edges[i].to] {
			case onPath:
				back[i] = true
			case unvisited:
				walk(edges[i].to)
			}
		}
		state[n] = done
	}

	for _, n := range nodes {
		if indegree[n] == 0 && state[n] == unvisited {
			walk(n)
		}
	}
	for _, n := range nodes { // anything left is inside a cycle with no root
		if state[n] == unvisited {
			walk(n)
		}
	}
	return back
}

// boxSize sizes a box around its wrapped label. Diamonds need noticeably more
// room than their text because the corners eat the horizontal space.
func boxSize(n *diagramNode) (float64, float64) {
	longest := 0
	for _, line := range n.lines {
		if c := utf8.RuneCountInString(line); c > longest {
			longest = c
		}
	}
	w := float64(longest)*charWidth + 40
	if w < minBoxWidth {
		w = minBoxWidth
	}
	h := float64(len(n.lines))*lineHeight + 30

	switch n.shape {
	case "diamond":
		return w * 1.5, h * 1.7
	case "ellipse":
		return w * 1.15, h * 1.15
	default:
		return w, h
	}
}

// wrapLabel breaks a label into at most three lines of roughly max characters,
// on word boundaries. Labels are short phrases here, so greedy wrapping is both
// sufficient and predictable.
func wrapLabel(label string, max int) []string {
	words := strings.Fields(label)
	if len(words) == 0 {
		return []string{label}
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if utf8.RuneCountInString(current)+1+utf8.RuneCountInString(w) <= max {
			current += " " + w
			continue
		}
		lines = append(lines, current)
		current = w
	}
	lines = append(lines, current)

	if len(lines) > 3 {
		lines = append(lines[:3:3], "")
		lines[2] += "…"
		lines = lines[:3]
	}
	return lines
}

// renderSVG writes the finished picture. The output is one self-contained file
// with no external references, so a browser reload is all it takes to see a
// redraw, and it carries a prefers-color-scheme block so a dark-mode browser
// doesn't show black text on a black page.
func renderSVG(title string, nodes []*diagramNode, edges []diagramEdge, width, height float64) string {
	var b strings.Builder

	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`+"\n",
		width, height, width, height)
	b.WriteString(`<style>
  :root { color-scheme: light dark; }
  .bg     { fill: #ffffff; }
  .shape  { fill: #eef4ff; stroke: #3b6bd6; stroke-width: 2; }
  .decide { fill: #fff5e3; stroke: #d68a1f; stroke-width: 2; }
  .term   { fill: #e8f7ee; stroke: #2f9e5f; stroke-width: 2; }
  .label  { fill: #14243d; font: 14px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .title  { fill: #14243d; font: 600 20px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .edge   { stroke: #5a6b85; stroke-width: 2; fill: none; }
  .edgetx { fill: #40506b; font: 12px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .edgebg { fill: #ffffff; }
  @media (prefers-color-scheme: dark) {
    .bg     { fill: #12161d; }
    .shape  { fill: #1b2942; stroke: #6f9bf0; }
    .decide { fill: #33270f; stroke: #e0a444; }
    .term   { fill: #12301f; stroke: #56c98a; }
    .label, .title { fill: #e7edf7; }
    .edge   { stroke: #93a3bd; }
    .edgetx { fill: #b9c6db; }
    .edgebg { fill: #12161d; }
  }
</style>
<defs>
  <marker id="arrowhead" viewBox="0 0 10 10" refX="9" refY="5"
          markerWidth="7" markerHeight="7" orient="auto-start-reverse">
    <path d="M 0 0 L 10 5 L 0 10 z" fill="#5a6b85"/>
  </marker>
</defs>
`)
	fmt.Fprintf(&b, `<rect class="bg" width="%.0f" height="%.0f"/>`+"\n", width, height)

	if title != "" {
		fmt.Fprintf(&b, `<text class="title" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			width/2, diagramMargin+8, html.EscapeString(title))
	}

	// Edges first so the boxes paint over the line ends.
	for _, e := range edges {
		b.WriteString(renderEdge(e))
	}
	for _, n := range nodes {
		b.WriteString(renderNode(n))
	}

	b.WriteString("</svg>\n")
	return b.String()
}

func renderNode(n *diagramNode) string {
	var b strings.Builder
	cx, cy := n.x+n.w/2, n.y+n.h/2

	switch n.shape {
	case "ellipse":
		fmt.Fprintf(&b, `<ellipse class="term" cx="%.1f" cy="%.1f" rx="%.1f" ry="%.1f"/>`+"\n",
			cx, cy, n.w/2, n.h/2)
	case "diamond":
		fmt.Fprintf(&b, `<polygon class="decide" points="%.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f"/>`+"\n",
			cx, n.y, n.x+n.w, cy, cx, n.y+n.h, n.x, cy)
	default:
		fmt.Fprintf(&b, `<rect class="shape" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="8"/>`+"\n",
			n.x, n.y, n.w, n.h)
	}

	// Vertically centre the block of wrapped lines on the shape's middle.
	startY := cy - (float64(len(n.lines)-1)*lineHeight)/2 + 5
	for i, line := range n.lines {
		fmt.Fprintf(&b, `<text class="label" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			cx, startY+float64(i)*lineHeight, html.EscapeString(line))
	}
	return b.String()
}

// renderEdge connects two boxes. A forward edge (down a layer) leaves the
// bottom and enters the top. Anything else — a back edge in a retry loop, or a
// hop between siblings — is routed as a curve out to the right, so it stays
// visible instead of cutting straight through the boxes in between.
func renderEdge(e diagramEdge) string {
	var b strings.Builder
	from, to := e.from, e.to

	var path string
	var lx, ly float64

	switch {
	case to.layer > from.layer:
		x1, y1 := from.x+from.w/2, from.y+from.h
		x2, y2 := to.x+to.w/2, to.y
		path = fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
			x1, y1, x1, y1+(y2-y1)/2, x2, y1+(y2-y1)/2, x2, y2)
		lx, ly = (x1+x2)/2, (y1+y2)/2
	default:
		// Back edge or same-row hop: bulge out to the right of both boxes.
		x1, y1 := from.x+from.w, from.y+from.h/2
		x2, y2 := to.x+to.w, to.y+to.h/2
		bulge := maxFloat(x1, x2) + 58
		path = fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
			x1, y1, bulge, y1, bulge, y2, x2, y2)
		lx, ly = bulge-6, (y1+y2)/2
	}

	fmt.Fprintf(&b, `<path class="edge" d="%s" marker-end="url(#arrowhead)"/>`+"\n", path)

	if e.label != "" {
		// A small backing rect keeps the label readable where it crosses a line.
		w := float64(utf8.RuneCountInString(e.label))*7 + 10
		fmt.Fprintf(&b, `<rect class="edgebg" x="%.1f" y="%.1f" width="%.1f" height="17" rx="4"/>`+"\n",
			lx-w/2, ly-12, w)
		fmt.Fprintf(&b, `<text class="edgetx" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			lx, ly, html.EscapeString(e.label))
	}
	return b.String()
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
