package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/OpenRouterTeam/go-sdk/models/components"
	"github.com/aliworkshop/ai-engineering-course/internal/toolspec"
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

	// Table payload. Columns is the header row; Rows are the body rows.
	Columns []string   `json:"columns,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`

	// Chart payload. Chart is the kind (bar / line / pie); Data is the series.
	Chart string       `json:"chart,omitempty"`
	Data  []chartDatum `json:"data,omitempty"`

	// Title is an alias for Label on tables and charts. Models reach for it —
	// a table's caption reads as a title, not a label — and dropping it left
	// the caption off the drawing with no error to say why.
	Title string `json:"title,omitempty"`
}

// chartDatum is one labelled value in a chart series.
type chartDatum struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

func (GenerateDiagram) Spec() components.ChatFunctionTool {
	return toolspec.Define("generate_diagram",
		"Draw anything visual in ONE call: flowcharts, architecture sketches, org charts, tables, and bar/line/pie charts. Writes canvas.svg (open in a browser, refresh to see changes) and canvas.excalidraw (open at excalidraw.com to edit by hand). Every element — boxes, arrows, tables, charts — goes in the single `elements` array; there is no separate arrows parameter. Do NOT pass coordinates or sizes; the tool lays everything out itself. Tables and charts can stand alone (no arrows needed) or sit in a flow with arrows pointing at them.",
		`{
  "type": "object",
  "properties": {
    "title": {
      "type": "string",
      "description": "Optional heading drawn at the top of the canvas, e.g. 'User signup'"
    },
    "elements": {
      "type": "array",
      "description": "Every element of the drawing, in one array.",
      "items": {
        "type": "object",
        "properties": {
          "type": {
            "type": "string",
            "enum": ["box", "arrow", "table", "chart"],
            "description": "'box' is a labelled shape; 'arrow' connects two elements; 'table' is a grid of cells; 'chart' plots data."
          },
          "id": {
            "type": "string",
            "description": "Unique id for a box, table or chart, so arrows can reference it, e.g. 'start'."
          },
          "label": {
            "type": "string",
            "description": "Box text; an arrow's edge label ('yes'/'no'); or the caption above a table or chart."
          },
          "shape": {
            "type": "string",
            "enum": ["box", "ellipse", "circle", "diamond", "hexagon", "parallelogram", "cylinder", "document", "note", "triangle", "cloud", "pill"],
            "description": "Box shape, chosen by meaning: 'ellipse' start/end, 'diamond' decision, 'cylinder' database, 'parallelogram' input/output, 'document' a report or file, 'note' an annotation, 'cloud' an external service, 'hexagon' preparation, 'box' (default) a step. Domain words work too — 'database', 'decision', 'input' all map to the right shape."
          },
          "from": {"type": "string", "description": "For an arrow: the id of the source element."},
          "to": {"type": "string", "description": "For an arrow: the id of the target element."},
          "columns": {
            "type": "array",
            "description": "For a table: the header cells.",
            "items": {"type": "string"}
          },
          "rows": {
            "type": "array",
            "description": "For a table: the body rows. Every row must have as many cells as there are columns.",
            "items": {"type": "array", "items": {"type": "string"}}
          },
          "chart": {
            "type": "string",
            "enum": ["bar", "line", "pie"],
            "description": "For a chart: which plot to draw. 'bar' compares categories, 'line' shows a trend, 'pie' shows shares of a whole."
          },
          "data": {
            "type": "array",
            "description": "For a chart: the series, in the order it should be plotted.",
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string"},
                "value": {"type": "number"}
              },
              "required": ["label", "value"]
            }
          }
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
	if err := toolspec.Decode(args, &a); err != nil {
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
// spec itself so the CRUD tools have something to change later.
//
// Both tools go through here, which is what keeps a modified diagram identical
// to one drawn from scratch with the same elements.
func drawDiagram(dir string, spec diagramSpec) (boxes, arrows int, err error) {
	nodes, edges, err := buildDiagram(spec.Elements)
	if err != nil {
		return 0, 0, err
	}

	width, height := layoutDiagram(nodes, edges, spec.Title != "")

	// The authoring form: Excalidraw element skeletons. Written first because
	// it's the input to everything below, and useful on its own — it's what you
	// hand to convertToExcalidrawElements() in a JS project.
	skels := allSkeletons(spec.Title, nodes, edges)
	skeletonJSON, err := json.MarshalIndent(skels, "", "  ")
	if err != nil {
		return 0, 0, err
	}
	if err := os.WriteFile(filepath.Join(dir, skeletonFile), append(skeletonJSON, '\n'), 0o644); err != nil {
		return 0, 0, err
	}

	// Excalidraw draws it when the renderer is installed, so the picture is the
	// library's own output. Otherwise the built-in renderers take over — a
	// missing optional npm package should cost fidelity, not the feature.
	svg, scene := "", ""
	if script := findRenderer(); script != "" {
		rendered, rerr := renderWithExcalidraw(script, spec.Title, skels)
		if rerr != nil {
			// Worth surfacing: silently falling back would hide a broken
			// install behind a picture that merely looks a bit different.
			fmt.Fprintf(os.Stderr, "  [diagram] excalidraw renderer unavailable, drawing without it: %v\n", rerr)
		} else {
			svg = rendered.SVG
			scene = string(rendered.Scene) + "\n"
		}
	}
	if svg == "" {
		svg = renderSVG(spec.Title, nodes, edges, width, height)
		scene, err = renderExcalidraw(spec.Title, nodes, edges)
		if err != nil {
			return 0, 0, err
		}
	}

	if err := os.WriteFile(filepath.Join(dir, canvasFile), []byte(svg), 0o644); err != nil {
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

	// kind selects the renderer: "shape" for a labelled outline, "table" for a
	// grid, "chart" for a plot. Tables and charts are laid out as nodes like any
	// other, so arrows can point at them and they take their place in the flow
	// instead of needing a separate positioning pass.
	kind    string
	columns []string
	rows    [][]string
	chart   string
	data    []chartDatum
}

const (
	kindShape = "shape"
	kindTable = "table"
	kindChart = "chart"
)

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
		kind := elementKind(e)
		if kind == "arrow" || kind == "" {
			continue
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			return nil, nil, fmt.Errorf("elements[%d] is a %s with no id; every node needs a unique id for arrows to reference", i, kind)
		}
		if _, dup := byID[id]; dup {
			return nil, nil, fmt.Errorf("duplicate id %q; ids must be unique", id)
		}
		label := strings.TrimSpace(e.Label)
		if label == "" {
			label = strings.TrimSpace(e.Title)
		}
		if label == "" && kind == "box" {
			label = id
		}

		n := &diagramNode{id: id, label: label, lines: wrapLabel(label, 20)}
		switch kind {
		case kindTable:
			n.kind, n.columns, n.rows = kindTable, e.Columns, e.Rows
			if err := validateTable(i, n); err != nil {
				return nil, nil, err
			}
		case kindChart:
			n.kind, n.chart, n.data = kindChart, normalizeChart(e.Chart), e.Data
			if err := validateChart(i, n); err != nil {
				return nil, nil, err
			}
		default:
			// The shape may have arrived in "shape" or, when the model skipped
			// the box/shape distinction, in "type". Prefer the explicit field.
			shape, ok := lookupShape(e.Shape)
			if !ok {
				if fromType, okType := lookupShape(e.Type); okType {
					shape = fromType
				}
			}
			n.kind, n.shape = kindShape, shape
		}

		byID[id] = n
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, nil, fmt.Errorf("nothing to draw; elements needs at least one \"box\", \"table\" or \"chart\"")
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

	// A multi-box flowchart with nothing joining it is almost always a caller
	// that put its arrows somewhere this tool didn't look. Drawing it anyway
	// produces a page of loose boxes reported as a success, which is worse than
	// failing: say so and let the model correct itself.
	//
	// Tables and charts are exempt — two tables side by side is a perfectly good
	// canvas, and demanding an arrow between them would be nonsense.
	if len(nodes) > 1 && len(edges) == 0 && allShapes(nodes) {
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
	case "table", "grid", "matrix":
		return kindTable
	case "chart", "graph", "plot", "bar", "line", "pie":
		return kindChart
	}
	// Models routinely put the *shape* in the type field — {"type":"cylinder"}
	// rather than {"type":"box","shape":"cylinder"}. Read literally that's an
	// unknown type, and the element silently degrades to a plain rectangle: the
	// caller asked for a database and got a box, with no error to say so.
	if _, ok := lookupShape(e.Type); ok {
		return "box"
	}
	// Inference, for when "type" is absent or misspelled. The payload is
	// unambiguous: rows mean a table, data means a chart, from/to means an
	// arrow, a bare id means a box.
	if len(e.Rows) > 0 || len(e.Columns) > 0 {
		return kindTable
	}
	if len(e.Data) > 0 || strings.TrimSpace(e.Chart) != "" {
		return kindChart
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

// allShapes reports whether the canvas is nothing but plain flowchart nodes —
// the only case where "no arrows" is a caller mistake rather than a legitimate
// layout.
func allShapes(nodes []*diagramNode) bool {
	for _, n := range nodes {
		if n.kind != kindShape {
			return false
		}
	}
	return true
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

// boxSize sizes a node. Tables and charts are measured from their content;
// a shape is sized around its wrapped label and then corrected for the geometry,
// since a diamond's corners eat horizontal space its text can't use.
func boxSize(n *diagramNode) (float64, float64) {
	switch n.kind {
	case kindTable:
		return tableSize(n)
	case kindChart:
		return chartSize(n)
	}

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

	wMul, hMul := shapeAspect(n.shape)
	return w * wMul, h * hMul
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
  .data   { fill: #f3eefe; stroke: #7a52cc; stroke-width: 2; }
  .fold   { fill: #ddd0f7; stroke: #7a52cc; stroke-width: 1.5; }
  /* tables */
  .tgrid     { fill: none; stroke: #9aa7bd; stroke-width: 1.5; }
  .thead     { fill: #eef4ff; }
  .trule     { stroke: #c3cddd; stroke-width: 1; }
  .tcell     { fill: #14243d; font: 13px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .thcell    { fill: #14243d; font: 600 13px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .tabletitle{ fill: #14243d; font: 600 15px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  /* charts */
  .charttitle{ fill: #14243d; font: 600 15px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .axis      { stroke: #9aa7bd; stroke-width: 1.5; }
  .chartval  { fill: #40506b; font: 11px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .chartcat  { fill: #40506b; font: 12px -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; }
  .chartline { fill: none; stroke: #3b6bd6; stroke-width: 2.5; }
  .chartdot  { fill: #3b6bd6; }
  .c1 { fill: #4f83e0; } .c2 { fill: #58b368; } .c3 { fill: #e0a444; }
  .c4 { fill: #d96d6d; } .c5 { fill: #9a7ae0; } .c6 { fill: #4bb3c4; }
  @media (prefers-color-scheme: dark) {
    .bg     { fill: #12161d; }
    .shape  { fill: #1b2942; stroke: #6f9bf0; }
    .decide { fill: #33270f; stroke: #e0a444; }
    .term   { fill: #12301f; stroke: #56c98a; }
    .data   { fill: #241b3d; stroke: #a98bf0; }
    .fold   { fill: #35296b; stroke: #a98bf0; }
    .label, .title { fill: #e7edf7; }
    .edge   { stroke: #93a3bd; }
    .edgetx { fill: #b9c6db; }
    .edgebg { fill: #12161d; }
    .tgrid  { stroke: #66748c; }
    .thead  { fill: #1b2942; }
    .trule  { stroke: #3a465a; }
    .tcell, .thcell, .tabletitle, .charttitle { fill: #e7edf7; }
    .chartval, .chartcat { fill: #b9c6db; }
    .axis   { stroke: #66748c; }
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

// renderNode draws one node, dispatching on its kind. Tables and charts own
// their whole bounding box; a shape gets an outline plus a centred label.
func renderNode(n *diagramNode) string {
	switch n.kind {
	case kindTable:
		return renderTableSVG(n)
	case kindChart:
		return renderChartSVG(n)
	}

	var b strings.Builder
	b.WriteString(shapeSVG(n))

	// Vertically centre the block of wrapped lines on the shape's middle.
	cx, cy := n.x+n.w/2, n.y+n.h/2
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
