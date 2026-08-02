package web

// The diagram editor.
//
// generate_diagram writes two files for every drawing: canvas.svg to look at,
// and canvas.excalidraw — a scene file in Excalidraw's own format — to edit.
// Until now editing it meant finding it on disk and dragging it onto
// excalidraw.com, which is a lot of ceremony for "make that box red".
//
// So the page serves Excalidraw itself. The editor is the real library, bundled
// out of the same excalidraw/ folder the rendering sidecar already lives in
// (see excalidraw/editor.jsx), and what it saves is what excalidraw.com would
// save. Nothing here is a reimplementation of a drawing tool.
//
// This is the one place the browser writes to the working directory. It writes
// exactly the two files the diagram tools own, by their fixed names, and never
// a path that came from the request.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aliworkshop/ai-engineering-course/internal/tools/diagram"
)

const (
	// sceneFile is the editable half of a drawing, beside canvas.svg.
	sceneFile = "canvas.excalidraw"

	// editorScript and editorStyle are built by `npm install` in excalidraw/.
	// When they're missing the page simply doesn't offer an Edit button — the
	// same bargain the renderer makes, where an optional npm package costs a
	// feature rather than breaking the agent.
	editorScript = "editor-bundle.js"
	editorStyle  = "editor-bundle.css"

	// editorAssetPrefix is what window.EXCALIDRAW_ASSET_PATH points at: the
	// published Excalidraw dist, where the library fetches its hand-drawn fonts
	// from at runtime. Serving them locally is what keeps the editor working
	// with no network, since the library's default is a CDN.
	editorAssetPrefix = "/editor/assets/"

	// maxCanvasBytes caps one save. A scene with a few pasted images is a
	// megabyte or two; well past that is a mistake, not a diagram.
	maxCanvasBytes = 32 << 20
)

// editorAssetDir is where Excalidraw's runtime assets live inside the sidecar
// folder, relative to it.
var editorAssetDir = filepath.Join("node_modules", "@excalidraw", "excalidraw", "dist", "prod")

// handleScene hands the editor the drawing to open. It is the file itself,
// unmodified: whatever wrote it last — the render sidecar, or a previous save
// from this editor — is what gets loaded.
func (s *Server) handleScene(w http.ResponseWriter, r *http.Request) {
	scene, err := os.ReadFile(filepath.Join(s.Dir, sceneFile))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(scene)
}

// handleSaveCanvas writes back a hand-edited drawing: the scene, so the next
// edit picks up where this one left off, and the SVG, so the picture beside the
// chat is the edited one.
//
// The SVG is exported in the browser rather than re-rendered here. The elements
// are already there, it is Excalidraw drawing either way, and the alternative —
// a Node round trip per save — would make the button feel slow for no gain in
// fidelity.
func (s *Server) handleSaveCanvas(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scene string `json:"scene"`
		SVG   string `json:"svg"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCanvasBytes)).Decode(&req); err != nil {
		http.Error(w, "could not read the drawing", http.StatusBadRequest)
		return
	}
	if err := checkScene(req.Scene); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	svg := strings.TrimSpace(req.SVG)
	if !strings.HasPrefix(svg, "<svg") {
		http.Error(w, "that is not an SVG drawing", http.StatusBadRequest)
		return
	}

	// A turn in flight may be about to redraw these very files. Taking the same
	// lock a question takes means a hand edit and a redraw can't interleave and
	// leave canvas.svg showing one drawing while canvas.excalidraw holds
	// another; the page turns the refusal into "the agent is drawing".
	sess := s.session(w, r)
	if !sess.turn.TryLock() {
		http.Error(w, "the agent is working on this diagram right now", http.StatusConflict)
		return
	}
	defer sess.turn.Unlock()

	// The scene first: it is the editable original, so a failure between the two
	// writes leaves the source of truth saved and only the picture stale.
	if err := writeAtomic(filepath.Join(s.Dir, sceneFile), []byte(req.Scene)); err != nil {
		http.Error(w, "could not save the drawing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := writeAtomic(filepath.Join(s.Dir, canvasFile), []byte(svg)); err != nil {
		http.Error(w, "could not save the picture: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkScene rejects anything that isn't an Excalidraw scene before it reaches
// the disk. It is a sanity gate, not a schema: the file is written by
// Excalidraw's own serializer, and the thing worth catching is a request that
// never came from it at all.
func checkScene(scene string) error {
	var head struct {
		Type     string            `json:"type"`
		Elements []json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal([]byte(scene), &head); err != nil {
		return fmt.Errorf("the drawing is not readable JSON: %v", err)
	}
	if head.Type != "excalidraw" {
		return fmt.Errorf("that is not an Excalidraw scene")
	}
	return nil
}

// writeAtomic replaces a file in one step, so a reader — the page reloading
// canvas.svg, the agent's tools loading the scene — never sees half of a save.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// Removes the leftover on any failure below; a no-op once the rename lands.
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the diagram files are readable like the
	// ones the tools write.
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// editorHandler serves the built editor bundle, or 404s every route under it
// when the bundle isn't there. The page probes for the script and hides its Edit
// button if it's missing, so "not built" degrades to the read-only UI that was
// here before rather than to a broken button.
func (s *Server) editorHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /editor/"+editorScript, s.serveEditorFile(editorScript, "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /editor/"+editorStyle, s.serveEditorFile(editorStyle, "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+editorAssetPrefix, s.serveEditorAsset)
	return mux
}

// serveEditorAsset hands over one of Excalidraw's runtime assets — the fonts it
// fetches while drawing. This is a directory served wholesale, so it is the one
// route in the page that takes a path from the request; http.Dir is what keeps
// that path inside the dist folder.
func (s *Server) serveEditorAsset(w http.ResponseWriter, r *http.Request) {
	if s.EditorDir == "" {
		http.NotFound(w, r)
		return
	}
	dist := http.Dir(filepath.Join(s.EditorDir, editorAssetDir))
	http.StripPrefix(editorAssetPrefix, http.FileServer(dist)).ServeHTTP(w, r)
}

// serveEditorFile serves one built file out of the sidecar folder. The content
// type is stated rather than sniffed: a bundle this size is not worth sniffing,
// and a JavaScript file served as text/plain doesn't run.
func (s *Server) serveEditorFile(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.EditorDir == "" {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(s.EditorDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// ServeContent answers the page's HEAD probe and revalidates on
		// modification time, so a rebuilt bundle is picked up on reload.
		http.ServeContent(w, r, name, info.ModTime(), f)
	}
}

// defaultEditorDir asks the diagram package where its Excalidraw folder is.
//
// It is a deliberate exception to the layering: everywhere else this package
// talks to the agent through an interface, but the editor is the browser half
// of a specific tool's output, and that tool already knows how to find its own
// npm install from any working directory. Duplicating that search here would be
// a second thing to get wrong.
func defaultEditorDir() string { return diagram.AssetDir() }
