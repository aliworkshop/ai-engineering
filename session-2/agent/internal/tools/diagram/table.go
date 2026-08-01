package diagram

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

// Tables.
//
// A table is a node like any other: it gets laid out in the flow, arrows can
// point at it, and it lives in both renderings. What's different is that its
// size comes from its content — column widths are measured from the widest cell
// in each column, so nothing is clipped and nothing is padded to a guess.

const (
	tableRowHeight = 32.0
	tableCellPad   = 14.0
	tableMinCol    = 60.0
	tableTitleGap  = 26.0 // room above the grid for the table's label
)

// validateTable rejects the shapes a model actually gets wrong: a table with no
// rows at all, and rows that don't line up with the header.
func validateTable(index int, n *diagramNode) error {
	if len(n.columns) == 0 && len(n.rows) == 0 {
		return fmt.Errorf("elements[%d] is a table with no columns and no rows; give it \"columns\" (the header) and \"rows\"", index)
	}
	width := len(n.columns)
	if width == 0 {
		width = len(n.rows[0])
	}
	for r, row := range n.rows {
		if len(row) != width {
			return fmt.Errorf("elements[%d] is a table whose row %d has %d cells but the table is %d columns wide; every row must have the same number of cells",
				index, r+1, len(row), width)
		}
	}
	return nil
}

// tableGrid returns the full grid including the header row, which is what both
// renderers iterate over.
func tableGrid(n *diagramNode) [][]string {
	grid := make([][]string, 0, len(n.rows)+1)
	if len(n.columns) > 0 {
		grid = append(grid, n.columns)
	}
	return append(grid, n.rows...)
}

// tableColumnWidths measures each column against its widest cell.
func tableColumnWidths(n *diagramNode) []float64 {
	grid := tableGrid(n)
	if len(grid) == 0 {
		return nil
	}
	widths := make([]float64, len(grid[0]))
	for _, row := range grid {
		for c, cell := range row {
			if c >= len(widths) {
				continue
			}
			w := float64(utf8.RuneCountInString(cell))*charWidth + 2*tableCellPad
			if w < tableMinCol {
				w = tableMinCol
			}
			if w > widths[c] {
				widths[c] = w
			}
		}
	}
	return widths
}

// tableSize is the bounding box the layout engine places.
func tableSize(n *diagramNode) (float64, float64) {
	total := 0.0
	for _, w := range tableColumnWidths(n) {
		total += w
	}
	h := float64(len(tableGrid(n))) * tableRowHeight
	if n.label != "" {
		h += tableTitleGap
	}
	return total, h
}

// renderTableSVG draws the grid: a header band, then one row per record, with
// rules between them. Cells are left-aligned because tabular text reads better
// that way than centred.
func renderTableSVG(n *diagramNode) string {
	var b strings.Builder
	grid := tableGrid(n)
	widths := tableColumnWidths(n)

	top := n.y
	if n.label != "" {
		fmt.Fprintf(&b, `<text class="tabletitle" x="%.1f" y="%.1f">%s</text>`+"\n",
			n.x, n.y+16, html.EscapeString(n.label))
		top += tableTitleGap
	}
	gridH := float64(len(grid)) * tableRowHeight

	// Header band first, so the rules and text land on top of it.
	if len(n.columns) > 0 {
		fmt.Fprintf(&b, `<rect class="thead" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`+"\n",
			n.x, top, n.w, tableRowHeight)
	}
	fmt.Fprintf(&b, `<rect class="tgrid" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="6"/>`+"\n",
		n.x, top, n.w, gridH)

	for r, row := range grid {
		rowY := top + float64(r)*tableRowHeight
		if r > 0 {
			fmt.Fprintf(&b, `<line class="trule" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n",
				n.x, rowY, n.x+n.w, rowY)
		}
		cellX := n.x
		for c, cell := range row {
			if c >= len(widths) {
				break
			}
			if c > 0 {
				fmt.Fprintf(&b, `<line class="trule" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/>`+"\n",
					cellX, top, cellX, top+gridH)
			}
			class := "tcell"
			if r == 0 && len(n.columns) > 0 {
				class = "thcell"
			}
			fmt.Fprintf(&b, `<text class="%s" x="%.1f" y="%.1f">%s</text>`+"\n",
				class, cellX+tableCellPad, rowY+tableRowHeight/2+5, html.EscapeString(cell))
			cellX += widths[c]
		}
	}
	return b.String()
}

// exTableElements renders a table for Excalidraw as one rectangle per cell with
// the text bound inside it. Bound text is what makes the result editable: click
// a cell after importing and type. A single image or a pile of loose lines
// would look the same and edit like a rock.
func exTableElements(n *diagramNode, nextID func(string) string) []map[string]any {
	var out []map[string]any
	grid := tableGrid(n)
	widths := tableColumnWidths(n)

	top := n.y
	if n.label != "" {
		out = append(out, exText(nextID("tabletitle"), n.label, n.x, n.y, 18, "left", "", ""))
		top += tableTitleGap
	}

	for r, row := range grid {
		rowY := top + float64(r)*tableRowHeight
		cellX := n.x
		for c, cell := range row {
			if c >= len(widths) {
				break
			}
			cellID := nextID("cell")
			textID := nextID("celltext")

			el := exBase(cellID, "rectangle", cellX, rowY, widths[c], tableRowHeight, exSeed(cellID))
			if r == 0 && len(n.columns) > 0 {
				el["backgroundColor"] = exBlueFill
			} else {
				el["backgroundColor"] = "transparent"
			}
			el["boundElements"] = []map[string]any{{"id": textID, "type": "text"}}
			out = append(out, el)
			out = append(out, exText(textID, cell, cellX, rowY, 14, "center", "middle", cellID))

			cellX += widths[c]
		}
	}
	return out
}
