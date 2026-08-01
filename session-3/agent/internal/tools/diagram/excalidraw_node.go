package diagram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Rendering with the real Excalidraw API.
//
// The skeletons this package builds are handed to a Node sidecar that runs
// Excalidraw's own convertToExcalidrawElements() and exportToSvg(). So the
// drawing on screen is produced by Excalidraw, not by an imitation of it: the
// hand-drawn stroke, the font, the arrow routing and the label placement are
// all the library's.
//
// It's a sidecar because Excalidraw is a browser library and this is a Go
// program — see excalidraw/render.cjs for the environment that takes.
//
// When the sidecar isn't installed the built-in Go renderers are used instead.
// That fallback is not a nicety: an agent that stops being able to draw because
// an optional npm package is missing is worse than one that draws a slightly
// plainer picture.

// rendererScript is the sidecar entry point, relative to the search roots.
const rendererScript = "excalidraw/render.cjs"

// rendererTimeout bounds the sidecar. Loading a 13 MB bundle and rendering is
// ~1s in practice; anything past this is a hang, not slow work.
const rendererTimeout = 60 * time.Second

// excalidrawRender is what the sidecar returns: the drawing Excalidraw rendered
// and the scene file behind it.
type excalidrawRender struct {
	SVG   string          `json:"svg"`
	Scene json.RawMessage `json:"scene"`
}

// findRenderer locates the sidecar, or returns "" when it isn't available.
//
// The search is explicit rather than clever, because a compiled binary has no
// idea where its source tree went:
//
//  1. $AGENT_EXCALIDRAW_RENDERER, a direct path to render.cjs.
//  2. excalidraw/render.cjs under the working directory, then each parent —
//     which covers running from the agent directory or anywhere beneath it.
//
// A renderer whose bundle hasn't been built yet is treated as absent: it would
// only fail at the point of drawing, and the fallback draws something.
func findRenderer() string {
	if explicit := os.Getenv("AGENT_EXCALIDRAW_RENDERER"); explicit != "" {
		// "off" forces the built-in renderers — useful for testing that path,
		// and for anyone who'd rather not spawn Node per diagram.
		switch strings.ToLower(explicit) {
		case "off", "none", "0", "false":
			return ""
		}
		if usableRenderer(explicit) {
			return explicit
		}
		return ""
	}

	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, rendererScript)
		if usableRenderer(candidate) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// usableRenderer reports whether the script and its built bundle both exist.
func usableRenderer(script string) bool {
	if _, err := os.Stat(script); err != nil {
		return false
	}
	bundle := filepath.Join(filepath.Dir(script), "excalidraw-bundle.cjs")
	_, err := os.Stat(bundle)
	return err == nil
}

// renderWithExcalidraw pipes the skeletons through the sidecar and returns what
// Excalidraw made of them.
func renderWithExcalidraw(script, title string, skeletons []skeletonElement) (*excalidrawRender, error) {
	input, err := json.Marshal(map[string]any{"title": title, "skeletons": skeletons})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), rendererTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// The sidecar's stderr is the useful part — a missing bundle, a load
		// failure — so it goes in the error rather than the exit status alone.
		msg := stderr.String()
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		return nil, fmt.Errorf("excalidraw renderer failed (%v): %s", err, msg)
	}

	var out excalidrawRender
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("excalidraw renderer returned unreadable output: %w", err)
	}
	if out.SVG == "" || len(out.Scene) == 0 {
		return nil, fmt.Errorf("excalidraw renderer returned an empty drawing")
	}
	return &out, nil
}
