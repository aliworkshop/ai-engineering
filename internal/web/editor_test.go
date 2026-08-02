package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	testScene = `{"type":"excalidraw","version":2,"source":"http://localhost:8080","elements":[],"appState":{},"files":{}}`
	testSVG   = `<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`
)

// newDrawingServer is newTestServer with somewhere to keep the drawing, for the
// tests that read and write the canvas files.
func newDrawingServer(t *testing.T, ask func(*Session, string) (string, error)) (*Server, *httptest.Server, *http.Client, string) {
	t.Helper()

	dir := t.TempDir()
	srv, ts, client := newTestServer(t, ask)
	// Safe to set after the handler is running: nothing has asked for a canvas
	// yet, and Dir is only read while serving one.
	srv.Dir = dir
	return srv, ts, client, dir
}

func read(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(data)
}

// The editor opens canvas.excalidraw, so the server has to hand it over — and
// say plainly when there isn't one yet rather than inventing an empty scene.
func TestSceneIsServedForEditing(t *testing.T) {
	_, ts, client, dir := newDrawingServer(t, func(*Session, string) (string, error) { return "", nil })

	res, err := client.Get(ts.URL + "/canvas.excalidraw")
	if err != nil {
		t.Fatalf("GET scene: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("with nothing drawn yet: got %d, want 404", res.StatusCode)
	}

	if err := os.WriteFile(filepath.Join(dir, sceneFile), []byte(testScene), 0o644); err != nil {
		t.Fatalf("write scene: %v", err)
	}

	res, err = client.Get(ts.URL + "/canvas.excalidraw")
	if err != nil {
		t.Fatalf("GET scene: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != testScene {
		t.Fatalf("got %d %q, want the scene back", res.StatusCode, body)
	}
}

// Saving a hand-edited diagram has to update both halves of it: the scene the
// next edit starts from, and the picture everything else looks at.
func TestSavingWritesBothHalvesOfTheDrawing(t *testing.T) {
	_, ts, client, dir := newDrawingServer(t, func(*Session, string) (string, error) { return "", nil })

	res := post(t, client, ts.URL+"/api/canvas", map[string]string{"scene": testScene, "svg": testSVG})
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("save: got %d: %s", res.StatusCode, body)
	}

	if got := read(t, filepath.Join(dir, sceneFile)); got != testScene {
		t.Fatalf("saved scene is %q", got)
	}
	if got := read(t, filepath.Join(dir, canvasFile)); got != testSVG {
		t.Fatalf("saved picture is %q", got)
	}
}

// The save route is the one thing in the page that writes to the working
// directory, so what it accepts matters: anything that isn't the drawing it
// claims to be has to bounce before it reaches the disk.
func TestJunkIsRefusedInsteadOfSaved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scene string
		svg   string
	}{
		{"not JSON at all", "not a scene", testSVG},
		{"JSON, but not a scene", `{"type":"something-else","elements":[]}`, testSVG},
		{"a scene, but no drawing", testScene, "<html>hello</html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ts, client, dir := newDrawingServer(t, func(*Session, string) (string, error) { return "", nil })

			res := post(t, client, ts.URL+"/api/canvas", map[string]string{"scene": tc.scene, "svg": tc.svg})
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", res.StatusCode)
			}
			for _, name := range []string{sceneFile, canvasFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("%s was written anyway", name)
				}
			}
		})
	}
}

// A hand edit landing in the middle of a redraw would leave the picture and the
// scene describing two different diagrams, so the save waits its turn — and
// says why instead of silently doing nothing.
func TestSaveIsRefusedWhileTheAgentIsDrawing(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)

	_, ts, client, dir := newDrawingServer(t, func(*Session, string) (string, error) {
		close(started)
		<-release
		return "drawn", nil
	})

	// Deferred report, so a failure inside ask — which ends this goroutine on
	// the spot — doesn't hang the wait at the end of the test.
	turn := make(chan struct{})
	go func() {
		defer close(turn)
		ask(t, ts, client, "draw me a flowchart", nil)
	}()
	<-started

	res := post(t, client, ts.URL+"/api/canvas", map[string]string{"scene": testScene, "svg": testSVG})
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("save during a turn: got %d, want 409", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, sceneFile)); !os.IsNotExist(err) {
		t.Fatal("the drawing was saved mid-turn anyway")
	}

	// Let the turn finish before the test returns: its stream is still open, and
	// tearing the server down underneath it reports as a failure of its own.
	unblock()
	<-turn
}

// The editor is an optional npm build. Without it every route under /editor/ is
// simply absent, which is what the page probes for before offering to edit.
func TestEditorIsAbsentWithoutABuiltBundle(t *testing.T) {
	srv, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "", nil })
	srv.EditorDir = ""

	for _, path := range []string{"/editor/editor-bundle.js", "/editor/editor-bundle.css", "/editor/assets/fonts/x.woff2"} {
		res, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: got %d, want 404", path, res.StatusCode)
		}
	}
}

// With the bundle built, the page gets the editor and Excalidraw gets its fonts
// — both out of the same excalidraw/ folder the render sidecar lives in.
func TestEditorIsServedFromTheBundleDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, editorScript), []byte("/* editor */"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	fonts := filepath.Join(dir, editorAssetDir, "fonts")
	if err := os.MkdirAll(fonts, 0o755); err != nil {
		t.Fatalf("make font dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fonts, "Excalifont.woff2"), []byte("font"), 0o644); err != nil {
		t.Fatalf("write font: %v", err)
	}

	srv, ts, client := newTestServer(t, func(*Session, string) (string, error) { return "", nil })
	srv.EditorDir = dir

	for path, want := range map[string]string{
		"/editor/editor-bundle.js":              "/* editor */",
		"/editor/assets/fonts/Excalifont.woff2": "font",
	} {
		res, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || string(body) != want {
			t.Fatalf("GET %s: got %d %q, want %q", path, res.StatusCode, body, want)
		}
	}

	// The asset route is the only one that takes a path from the request, so it
	// is the only one that could be walked out of its directory.
	res, err := client.Get(ts.URL + "/editor/assets/../../../../etc/passwd")
	if err != nil {
		t.Fatalf("GET traversal: %v", err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("served a file from outside the assets directory")
	}
}

// A save that fails halfway must not leave a half-written drawing where the
// editor, the page, or the agent's own tools will read it.
func TestSavesAreWrittenWholeOrNotAtAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, canvasFile)
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writeAtomic(path, []byte("replacement")); err != nil {
		t.Fatalf("write atomic: %v", err)
	}
	if got := read(t, path); got != "replacement" {
		t.Fatalf("file holds %q", got)
	}

	// The temporary file it wrote through must not be left behind: the diagram
	// directory is one the user looks at.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("left %d files behind, want just the drawing", len(entries))
	}
}
