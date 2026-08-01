package diagram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
//  1. $AGENT_EXCALIDRAW_RENDERER, a direct path to render.cjs, or "off".
//  2. Beside the source this package was compiled from.
//  3. Beside the working directory.
//
// Order matters. The sidecar ships inside the agent tree, so the source-relative
// answer is the binary's *own* renderer — the right one even when the process is
// run from somewhere else entirely, and specifically not some other copy of the
// agent that happens to sit near the working directory.
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

	for _, candidate := range rendererCandidates() {
		if usableRenderer(candidate) {
			return candidate
		}
	}
	return ""
}

// rendererCandidates lists everywhere the sidecar might be, best first.
func rendererCandidates() []string {
	var out []string
	// runtime.Caller gives this file's path as it was at compile time, which
	// for `go run` and a locally built binary is the real source tree. That is
	// what makes the search work from any working directory — including one
	// with no relationship to the agent at all. If the binary was moved to a
	// machine without that tree, the paths simply don't exist and the working
	// directory search below takes over.
	if _, file, _, ok := runtime.Caller(0); ok {
		out = append(out, ancestorCandidates(filepath.Dir(file))...)
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, ancestorCandidates(wd)...)
	}
	return out
}

// ancestorCandidates probes a directory and each of its parents.
//
// Two shapes are tried at every level: excalidraw/render.cjs, for a directory
// at or under the agent itself, and agent/excalidraw/render.cjs, for one
// *containing* it — running from the session folder rather than the agent
// folder is the easy mistake, and it used to fall back silently because the
// walk only ever went up.
func ancestorCandidates(start string) []string {
	var out []string
	for dir := start; ; {
		out = append(out,
			filepath.Join(dir, rendererScript),
			filepath.Join(dir, "agent", rendererScript))
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
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
