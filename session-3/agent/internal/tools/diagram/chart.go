package diagram

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Charts.
//
// Bar, line and pie, drawn from a labelled series. Like tables, a chart is a
// node in the layout, so it can sit inside a flow with arrows pointing at it.
//
// The axis is deliberately minimal — a baseline, a max-value tick, and the
// values printed on the marks. A chart in a diagram is there to show shape and
// magnitude at a glance; anything more starts competing with the diagram it
// sits in.

const (
	chartPlotHeight = 170.0
	chartBarWidth   = 54.0
	chartBarGap     = 18.0
	chartTitleGap   = 26.0
	chartLabelBand  = 34.0 // room under the plot for category labels
	chartMinWidth   = 240.0
	pieRadius       = 90.0
	pieLegendWidth  = 150.0
)

// chartPalette cycles through distinguishable fills. Six is enough for the
// category counts a diagram can legibly hold.
var chartPalette = []string{"c1", "c2", "c3", "c4", "c5", "c6"}

func normalizeChart(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "line", "trend", "series":
		return "line"
	case "pie", "donut", "share", "proportion", "breakdown":
		return "pie"
	default:
		return "bar"
	}
}

// validateChart rejects charts that can't be drawn meaningfully: no data at
// all, or values that would make every bar zero-height.
func validateChart(index int, n *diagramNode) error {
	if len(n.data) == 0 {
		return fmt.Errorf("elements[%d] is a chart with no data; give it \"data\" as a list of {\"label\": ..., \"value\": ...}", index)
	}
	for _, d := range n.data {
		if d.Value < 0 {
			return fmt.Errorf("elements[%d] has a negative value (%s: %g); this chart draws magnitudes, so values must be zero or above",
				index, d.Label, d.Value)
		}
	}
	if chartMax(n.data) <= 0 {
		return fmt.Errorf("elements[%d] is a chart whose values are all zero, so there would be nothing to see", index)
	}
	return nil
}

func chartMax(data []chartDatum) float64 {
	max := 0.0
	for _, d := range data {
		if d.Value > max {
			max = d.Value
		}
	}
	return max
}

func chartTotal(data []chartDatum) float64 {
	total := 0.0
	for _, d := range data {
		total += d.Value
	}
	return total
}

// chartSize is the bounding box the layout engine places.
func chartSize(n *diagramNode) (float64, float64) {
	h := chartPlotHeight + chartLabelBand
	if n.label != "" {
		h += chartTitleGap
	}

	if n.chart == "pie" {
		return 2*pieRadius + pieLegendWidth, math.Max(2*pieRadius+20, h)
	}

	w := float64(len(n.data))*(chartBarWidth+chartBarGap) + chartBarGap
	if w < chartMinWidth {
		w = chartMinWidth
	}
	return w, h
}

// renderChartSVG dispatches to the right plot.
func renderChartSVG(n *diagramNode) string {
	var b strings.Builder
	top := n.y
	if n.label != "" {
		fmt.Fprintf(&b, `<text class="charttitle" x="%.1f" y="%.1f">%s</text>`+"\n",
			n.x, n.y+16, html.EscapeString(n.label))
		top += chartTitleGap
	}

	switch n.chart {
	case "pie":
		b.WriteString(renderPieSVG(n, top))
	case "line":
		b.WriteString(renderLineSVG(n, top))
	default:
		b.WriteString(renderBarsSVG(n, top))
	}
	return b.String()
}

func renderBarsSVG(n *diagramNode, top float64) string {
	var b strings.Builder
	max := chartMax(n.data)
	baseline := top + chartPlotHeight

	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n",
		n.x, baseline, n.x+n.w, baseline)

	x := n.x + chartBarGap
	for i, d := range n.data {
		h := chartPlotHeight * (d.Value / max)
		y := baseline - h
		fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="3"/>`+"\n",
			chartPalette[i%len(chartPalette)], x, y, chartBarWidth, h)
		// Value above the bar, category beneath the axis.
		fmt.Fprintf(&b, `<text class="chartval" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			x+chartBarWidth/2, y-6, formatValue(d.Value))
		fmt.Fprintf(&b, `<text class="chartcat" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			x+chartBarWidth/2, baseline+18, html.EscapeString(truncate(d.Label, 10)))
		x += chartBarWidth + chartBarGap
	}
	return b.String()
}

func renderLineSVG(n *diagramNode, top float64) string {
	var b strings.Builder
	max := chartMax(n.data)
	baseline := top + chartPlotHeight

	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n",
		n.x, baseline, n.x+n.w, baseline)

	step := n.w / float64(maxInt(len(n.data)-1, 1))
	var path strings.Builder
	type pt struct{ x, y float64 }
	pts := make([]pt, 0, len(n.data))

	for i, d := range n.data {
		x := n.x + float64(i)*step
		if len(n.data) == 1 {
			x = n.x + n.w/2
		}
		y := baseline - chartPlotHeight*(d.Value/max)
		pts = append(pts, pt{x, y})
		if i == 0 {
			fmt.Fprintf(&path, "M %.1f %.1f", x, y)
		} else {
			fmt.Fprintf(&path, " L %.1f %.1f", x, y)
		}
	}
	fmt.Fprintf(&b, `<path class="chartline" d="%s"/>`+"\n", path.String())

	for i, p := range pts {
		fmt.Fprintf(&b, `<circle class="chartdot" cx="%.1f" cy="%.1f" r="4"/>`+"\n", p.x, p.y)
		fmt.Fprintf(&b, `<text class="chartval" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			p.x, p.y-10, formatValue(n.data[i].Value))
		fmt.Fprintf(&b, `<text class="chartcat" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n",
			p.x, baseline+18, html.EscapeString(truncate(n.data[i].Label, 10)))
	}
	return b.String()
}

func renderPieSVG(n *diagramNode, top float64) string {
	var b strings.Builder
	total := chartTotal(n.data)
	cx, cy := n.x+pieRadius, top+pieRadius
	angle := -math.Pi / 2 // start at twelve o'clock, the way pies are read

	for i, d := range n.data {
		sweep := 2 * math.Pi * (d.Value / total)
		end := angle + sweep

		// A slice that's the whole pie has no arc to draw — it's a circle.
		if len(n.data) == 1 || sweep >= 2*math.Pi-1e-9 {
			fmt.Fprintf(&b, `<circle class="%s" cx="%.1f" cy="%.1f" r="%.1f"/>`+"\n",
				chartPalette[i%len(chartPalette)], cx, cy, pieRadius)
		} else {
			x1, y1 := cx+pieRadius*math.Cos(angle), cy+pieRadius*math.Sin(angle)
			x2, y2 := cx+pieRadius*math.Cos(end), cy+pieRadius*math.Sin(end)
			large := 0
			if sweep > math.Pi {
				large = 1
			}
			fmt.Fprintf(&b, `<path class="%s" d="M %.1f %.1f L %.1f %.1f A %.1f %.1f 0 %d 1 %.1f %.1f Z"/>`+"\n",
				chartPalette[i%len(chartPalette)], cx, cy, x1, y1, pieRadius, pieRadius, large, x2, y2)
		}

		// Legend beside the pie: a swatch, the label, and the share. Slice
		// labels inside the wedges become unreadable as soon as a slice is thin.
		ly := top + 14 + float64(i)*22
		lx := n.x + 2*pieRadius + 16
		fmt.Fprintf(&b, `<rect class="%s" x="%.1f" y="%.1f" width="12" height="12" rx="2"/>`+"\n",
			chartPalette[i%len(chartPalette)], lx, ly-10)
		fmt.Fprintf(&b, `<text class="chartcat" x="%.1f" y="%.1f">%s — %.0f%%</text>`+"\n",
			lx+18, ly, html.EscapeString(truncate(d.Label, 14)), 100*d.Value/total)

		angle = end
	}
	return b.String()
}

// chartSkeletons describes a chart in skeleton form. Bars and pie swatches are
// rectangles, a line series is a line — all editable primitives rather than a
// flattened picture. Bars carry their category as a Label, so the caption is
// bound inside the bar and moves with it when dragged.
//
// Pie slices are the one thing that doesn't survive the trip: Excalidraw has no
// arc primitive, so the scene gets the legend and a plain circle instead of
// wedges. The SVG is the faithful rendering for pies.
func chartSkeletons(n *diagramNode) []skeletonElement {
	var out []skeletonElement
	top := n.y
	if n.label != "" {
		out = append(out, skeletonElement{
			Type: "text", X: n.x, Y: n.y, Text: n.label, FontSize: 18,
		})
		top += chartTitleGap
	}
	baseline := top + chartPlotHeight
	max := chartMax(n.data)

	switch n.chart {
	case "pie":
		total := chartTotal(n.data)
		out = append(out, skeletonElement{
			Type: "ellipse", X: n.x, Y: top,
			Width: 2 * pieRadius, Height: 2 * pieRadius,
		})
		for i, d := range n.data {
			ly := top + float64(i)*24
			out = append(out,
				skeletonElement{
					Type: "rectangle", X: n.x + 2*pieRadius + 16, Y: ly,
					Width: 14, Height: 14, BackgroundColor: exFillFor(i),
				},
				skeletonElement{
					Type: "text", X: n.x + 2*pieRadius + 38, Y: ly,
					Text:     fmt.Sprintf("%s — %.0f%%", d.Label, 100*d.Value/total),
					FontSize: 14,
				})
		}

	case "line":
		step := n.w / float64(maxInt(len(n.data)-1, 1))
		pts := make([][]float64, 0, len(n.data))
		for i, d := range n.data {
			dx := float64(i) * step
			if len(n.data) == 1 {
				dx = n.w / 2
			}
			pts = append(pts, []float64{round(dx), round(chartPlotHeight - chartPlotHeight*(d.Value/max))})
		}
		out = append(out, skeletonElement{
			Type: "line", X: n.x, Y: top, Width: n.w, Height: chartPlotHeight,
			Points: pts,
		})
		for i, d := range n.data {
			out = append(out, skeletonElement{
				Type: "text", X: n.x + pts[i][0], Y: baseline + 16,
				Text: d.Label, FontSize: 14, TextAlign: "center",
			})
		}

	default: // bar
		x := n.x + chartBarGap
		for i, d := range n.data {
			h := chartPlotHeight * (d.Value / max)
			out = append(out,
				skeletonElement{
					Type: "rectangle", X: x, Y: baseline - h,
					Width: chartBarWidth, Height: h,
					BackgroundColor: exFillFor(i),
				},
				// Value above the bar, category beneath it — the same split the
				// SVG uses. Combining them into one caption made a label wider
				// than its bar, so neighbouring categories ran together.
				skeletonElement{
					Type: "text", X: x + chartBarWidth/2, Y: baseline - h - 12,
					Text: formatValue(d.Value), FontSize: 12,
					TextAlign: "center", VerticalAlign: "middle",
				},
				skeletonElement{
					Type: "text", X: x + chartBarWidth/2, Y: baseline + 16,
					Text: truncate(d.Label, 10), FontSize: 14,
					TextAlign: "center", VerticalAlign: "middle",
				})
			x += chartBarWidth + chartBarGap
		}
	}
	return out
}

// exFillFor cycles Excalidraw's palette in step with the SVG's classes, so the
// same series is the same colour in both renderings.
func exFillFor(i int) string {
	fills := []string{exBlueFill, exGreenFill, exYellowFill, "#ffc9c9", "#d0bfff", "#99e9f2"}
	return fills[i%len(fills)]
}

// formatValue prints a number the way a chart label should: no trailing zeros
// on whole numbers, at most two decimals otherwise.
func formatValue(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
