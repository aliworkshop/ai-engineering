package diagram

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func drawArgs(t *testing.T, args string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), args); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, canvasFile))
	if err != nil {
		t.Fatalf("canvas: %v", err)
	}
	if err := xml.Unmarshal(raw, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("malformed SVG: %v", err)
	}
	return string(raw), dir
}

// ---------- tables ----------

const pricingTable = `{
  "title": "Pricing",
  "elements": [{
    "type": "table",
    "id": "plans",
    "label": "Plans",
    "columns": ["Plan", "Seats", "Price"],
    "rows": [["Free", "1", "$0"], ["Team", "10", "$99"], ["Enterprise", "unlimited", "call us"]]
  }]
}`

// A table on its own is a complete canvas — no arrows required.
func TestTableStandsAlone(t *testing.T) {
	svg, dir := drawArgs(t, pricingTable)

	for _, cell := range []string{"Plan", "Seats", "Price", "Free", "Enterprise", "call us", "$99"} {
		if !strings.Contains(svg, ">"+cell+"<") {
			t.Errorf("cell %q missing from the canvas", cell)
		}
	}
	// 3 columns x 4 rows including the header.
	if got := strings.Count(svg, `class="tcell"`) + strings.Count(svg, `class="thcell"`); got != 12 {
		t.Errorf("expected 12 rendered cells, got %d", got)
	}

	// Every cell is an editable rectangle with bound text in the scene.
	scene := readScene(t, dir)
	rects, texts := 0, 0
	for _, el := range scene.Elements {
		switch el["type"] {
		case "rectangle":
			rects++
		case "text":
			texts++
		}
	}
	if rects != 12 {
		t.Errorf("expected 12 cell rectangles in the scene, got %d", rects)
	}
	if texts < 12 {
		t.Errorf("expected at least 12 cell texts in the scene, got %d", texts)
	}
}

// Column widths come from the content, so nothing is clipped.
func TestTableSizesToItsWidestCell(t *testing.T) {
	narrow := &diagramNode{kind: kindTable, columns: []string{"a"}, rows: [][]string{{"b"}}}
	wide := &diagramNode{kind: kindTable, columns: []string{"a"},
		rows: [][]string{{"a considerably longer cell value"}}}

	nw, _ := tableSize(narrow)
	ww, _ := tableSize(wide)
	if ww <= nw {
		t.Fatalf("a wider cell must widen the table: narrow=%.0f wide=%.0f", nw, ww)
	}
}

// A ragged row is the mistake worth catching: it silently misaligns every
// column after it.
func TestTableRejectsRaggedRows(t *testing.T) {
	dir := t.TempDir()
	_, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), `{"elements":[{
      "type":"table","id":"t","columns":["A","B"],"rows":[["1","2"],["only one"]]
    }]}`)
	if err == nil {
		t.Fatal("expected a ragged row to be rejected")
	}
	if !strings.Contains(err.Error(), "row 2") || !strings.Contains(err.Error(), "columns wide") {
		t.Errorf("the error should say which row and how wide, got %q", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, canvasFile)); statErr == nil {
		t.Error("a rejected table must not leave a canvas behind")
	}
}

func TestTableRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := (GenerateDiagram{Dir: dir}).Run(context.Background(),
		`{"elements":[{"type":"table","id":"t"}]}`); err == nil {
		t.Fatal("expected an empty table to be rejected")
	}
}

// ---------- charts ----------

func TestBarChart(t *testing.T) {
	svg, dir := drawArgs(t, `{"elements":[{
      "type":"chart","id":"c","label":"Weekly signups","chart":"bar",
      "data":[{"label":"Mon","value":12},{"label":"Tue","value":30},{"label":"Wed","value":21}]
    }]}`)

	if got := strings.Count(svg, `class="c`); got < 3 {
		t.Errorf("expected 3 coloured bars, got %d", got)
	}
	for _, want := range []string{"Weekly signups", "Mon", "Tue", "Wed", ">30<", ">12<"} {
		if !strings.Contains(svg, want) {
			t.Errorf("expected %q on the chart", want)
		}
	}
	// Bars must be proportional: the tallest value gets the tallest bar.
	scene := readScene(t, dir)
	var heights []float64
	for _, el := range scene.Elements {
		if el["type"] == "rectangle" {
			heights = append(heights, el["height"].(float64))
		}
	}
	if len(heights) != 3 {
		t.Fatalf("expected 3 bars in the scene, got %d", len(heights))
	}
	if !(heights[1] > heights[2] && heights[2] > heights[0]) {
		t.Errorf("bar heights should follow the values 12<21<30, got %v", heights)
	}
}

func TestLineChart(t *testing.T) {
	svg, dir := drawArgs(t, `{"elements":[{
      "type":"chart","id":"c","label":"Latency","chart":"line",
      "data":[{"label":"v1","value":120},{"label":"v2","value":90},{"label":"v3","value":45}]
    }]}`)

	if !strings.Contains(svg, `class="chartline"`) {
		t.Error("no line drawn")
	}
	if got := strings.Count(svg, `class="chartdot"`); got != 3 {
		t.Errorf("expected 3 points, got %d", got)
	}
	scene := readScene(t, dir)
	for _, el := range scene.Elements {
		if el["type"] == "line" {
			pts, _ := el["points"].([]any)
			if len(pts) != 3 {
				t.Errorf("expected a 3-point line in the scene, got %d", len(pts))
			}
			return
		}
	}
	t.Error("no line element in the excalidraw scene")
}

func TestPieChart(t *testing.T) {
	svg, _ := drawArgs(t, `{"elements":[{
      "type":"chart","id":"c","label":"Traffic","chart":"pie",
      "data":[{"label":"Direct","value":50},{"label":"Search","value":30},{"label":"Social","value":20}]
    }]}`)

	if got := strings.Count(svg, "<path"); got < 3 {
		t.Errorf("expected 3 pie slices, got %d paths", got)
	}
	// The legend carries the shares, since thin wedges can't hold a label.
	for _, want := range []string{"Direct — 50%", "Search — 30%", "Social — 20%"} {
		if !strings.Contains(svg, want) {
			t.Errorf("expected legend entry %q", want)
		}
	}
}

// A single-value pie is a full circle, not a zero-width wedge.
func TestPieWithOneSlice(t *testing.T) {
	svg, _ := drawArgs(t, `{"elements":[{
      "type":"chart","id":"c","chart":"pie","data":[{"label":"All","value":7}]
    }]}`)
	if !strings.Contains(svg, "<circle") {
		t.Error("a single-slice pie should be drawn as a circle")
	}
}

func TestChartRejectsBadData(t *testing.T) {
	cases := map[string]string{
		"no data":  `{"elements":[{"type":"chart","id":"c","chart":"bar"}]}`,
		"all zero": `{"elements":[{"type":"chart","id":"c","chart":"bar","data":[{"label":"a","value":0}]}]}`,
		"negative": `{"elements":[{"type":"chart","id":"c","chart":"bar","data":[{"label":"a","value":-3}]}]}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := (GenerateDiagram{Dir: t.TempDir()}).Run(context.Background(), args); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// ---------- mixed canvases ----------

// The point of tables and charts being nodes: they take part in the flow, with
// arrows pointing at them.
func TestChartAndTableInAFlow(t *testing.T) {
	svg, dir := drawArgs(t, `{"title":"Report","elements":[
      {"type":"box","id":"src","label":"Raw events","shape":"cylinder"},
      {"type":"table","id":"summary","label":"Top pages","columns":["Page","Views"],"rows":[["/","900"],["/pricing","410"]]},
      {"type":"chart","id":"trend","label":"Views","chart":"bar","data":[{"label":"Mon","value":900},{"label":"Tue","value":410}]},
      {"type":"arrow","from":"src","to":"summary"},
      {"type":"arrow","from":"summary","to":"trend"}
    ]}`)

	for _, want := range []string{"Raw events", "Top pages", "/pricing", "Views", "Mon"} {
		if !strings.Contains(svg, want) {
			t.Errorf("expected %q on the canvas", want)
		}
	}
	if got := strings.Count(svg, `marker-end="url(#arrowhead)"`); got != 2 {
		t.Errorf("expected 2 arrows, got %d", got)
	}

	// Arrows touching a table or chart must not carry a dangling binding —
	// Excalidraw drops elements that reference an id that isn't in the scene.
	scene := readScene(t, dir)
	ids := map[string]bool{}
	for _, el := range scene.Elements {
		ids[el["id"].(string)] = true
	}
	for _, el := range scene.Elements {
		for _, side := range []string{"startBinding", "endBinding"} {
			b, ok := el[side].(map[string]any)
			if !ok {
				continue
			}
			if target, _ := b["elementId"].(string); !ids[target] {
				t.Errorf("%v has a dangling %s to %q", el["id"], side, target)
			}
		}
	}
}

// Two tables side by side need no arrows — the "unconnected boxes" guard is a
// flowchart rule and must not fire here.
func TestTablesNeedNoArrows(t *testing.T) {
	dir := t.TempDir()
	_, err := (GenerateDiagram{Dir: dir}).Run(context.Background(), `{"elements":[
      {"type":"table","id":"a","columns":["x"],"rows":[["1"]]},
      {"type":"table","id":"b","columns":["y"],"rows":[["2"]]}
    ]}`)
	if err != nil {
		t.Fatalf("two tables should be a valid canvas: %v", err)
	}
}

// But the guard must still protect flowcharts.
func TestLooseBoxesStillRejected(t *testing.T) {
	if _, err := (GenerateDiagram{Dir: t.TempDir()}).Run(context.Background(), `{"elements":[
      {"type":"box","id":"a","label":"A"},
      {"type":"box","id":"b","label":"B"}
    ]}`); err == nil {
		t.Fatal("expected unconnected boxes to still be rejected")
	}
}

// Payload alone identifies the kind, for when "type" is missing or wrong.
func TestKindInferredFromPayload(t *testing.T) {
	cases := map[string]diagramElement{
		kindTable: {ID: "t", Columns: []string{"a"}, Rows: [][]string{{"1"}}},
		kindChart: {ID: "c", Data: []chartDatum{{Label: "a", Value: 1}}},
		"arrow":   {From: "a", To: "b"},
		"box":     {ID: "b", Label: "B"},
	}
	for want, el := range cases {
		if got := elementKind(el); got != want {
			t.Errorf("elementKind(%+v) = %q, want %q", el, got, want)
		}
	}
}

// Models routinely put the shape in the type field — {"type":"cylinder"} rather
// than {"type":"box","shape":"cylinder"}. Read literally that's an unknown type
// and the element degrades to a plain rectangle with no error: the caller asked
// for a database and got a box. Observed in a live run, hence this test.
func TestShapeInTypeFieldIsRecovered(t *testing.T) {
	svg, _ := drawArgs(t, `{"elements":[
      {"type":"cylinder","id":"db","label":"Events DB"},
      {"type":"hexagon","id":"etl","label":"ETL job"},
      {"type":"arrow","from":"db","to":"etl"}
    ]}`)

	if !strings.Contains(svg, `class="data"`) {
		t.Error("the cylinder degraded to a plain box")
	}
	if !strings.Contains(svg, "<polygon") {
		t.Error("the hexagon degraded to a plain box")
	}
}
