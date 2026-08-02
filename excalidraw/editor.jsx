// The Excalidraw editor, bundled for the browser.
//
// generate_diagram already writes canvas.excalidraw — the same scene format
// excalidraw.com opens. This puts Excalidraw's own React component inside the
// agent's page so that file can be edited where it was drawn, instead of being
// found on disk, dragged onto excalidraw.com, and exported back by hand.
//
// It is bundled rather than pulled from a CDN for the reason the rest of the
// page has no external references: this is a local tool that has to work with
// no network, and an editor that quietly stops existing offline is worse than
// no button at all. The fonts come from the installed package too, served by
// the Go server under window.EXCALIDRAW_ASSET_PATH.
//
// The bundle publishes one global — window.AgentExcalidraw — so index.html
// stays a plain script with no build step of its own. Everything the page needs
// is on the handle mount() returns: the scene to save, the SVG to show, and a
// way to take it all down again.

import { createRoot } from "react-dom/client";
import {
  Excalidraw,
  exportToSvg,
  getSceneVersion,
  serializeAsJSON,
} from "@excalidraw/excalidraw";
import "@excalidraw/excalidraw/index.css";

// mount puts an editor in container, opened on scene (the parsed contents of
// canvas.excalidraw, or null for an empty canvas).
//
// The scene is handed over as initialData rather than pushed in afterwards with
// updateScene: Excalidraw runs its own restore() over initialData, which is
// what makes a file written by another version of the library — the Node
// sidecar's, here — safe to open.
function mount(container, scene) {
  const root = createRoot(container);

  // The imperative API arrives in a callback after the first render, so every
  // method below waits on it rather than assuming a mounted editor.
  let ready;
  const api = new Promise((resolve) => {
    ready = resolve;
  });

  // What "unedited" looks like, so closing the editor can tell the difference
  // between throwing work away and closing a drawing nobody touched.
  //
  // It is taken from the first change Excalidraw reports, which is the scene it
  // has just mounted. Neither of the obvious alternatives works: the file on
  // disk is versioned differently once restore() has been over it, and the
  // editor is briefly empty between mounting and receiving initialData, so
  // reading it as soon as the API arrives records "unedited" as "blank".
  let saved = null;

  root.render(
    <Excalidraw
      excalidrawAPI={ready}
      onChange={(elements) => {
        if (saved !== null) return;
        saved = getSceneVersion(elements);
        // The drawing has just landed, so this is the moment to frame it. The
        // panel is narrower than a browser window and the agent's flowcharts
        // are tall, so opening at 100% usually shows the first two boxes and
        // nothing else; fitToContent only ever zooms out to make it all fit.
        // Nothing to frame on an empty canvas — that one opens where it is.
        if (elements.length) {
          api.then((editor) =>
            editor.scrollToContent(elements, { fitToContent: true, animate: false }),
          );
        }
      }}
      theme={
        window.matchMedia?.("(prefers-color-scheme: dark)").matches
          ? "dark"
          : "light"
      }
      initialData={{
        elements: scene?.elements ?? [],
        appState: scene?.appState ?? {},
        files: scene?.files ?? {},
        // The agent lays diagrams out from the origin, so an editor opened at
        // the default scroll position can look empty on a small panel.
        scrollToContent: true,
      }}
    />,
  );

  return {
    // json returns exactly what excalidraw.com's "Save to disk" writes, so the
    // file the agent reads back is a scene file in the ordinary sense — not
    // this page's private dialect of one.
    async json() {
      const editor = await api;
      return serializeAsJSON(
        editor.getSceneElements(),
        editor.getAppState(),
        editor.getFiles(),
        "local",
      );
    },

    // svg re-renders canvas.svg from what is on the canvas now. Exported here
    // rather than on the server because the elements are already in this
    // browser, and because it is Excalidraw itself drawing either way.
    //
    // The export is forced light and opaque to match the sidecar's: canvas.svg
    // is shown in an <img>, which has no page behind it to supply a background.
    async svg() {
      const editor = await api;
      const svg = await exportToSvg({
        elements: editor.getSceneElements(),
        appState: {
          ...editor.getAppState(),
          exportBackground: true,
          exportWithDarkMode: false,
        },
        files: editor.getFiles(),
      });
      return svg.outerHTML;
    },

    // dirty reports whether the drawing differs from the last saved state.
    // Excalidraw's own scene version — the sum of its elements' revisions — is
    // the answer, so moving a box counts and picking a tool doesn't.
    async dirty() {
      const editor = await api;
      return saved !== null && getSceneVersion(editor.getSceneElements()) !== saved;
    },

    // markSaved makes right now the new "unedited", for the page to call once a
    // save has actually reached the disk.
    async markSaved() {
      const editor = await api;
      saved = getSceneVersion(editor.getSceneElements());
    },

    unmount() {
      root.unmount();
    },
  };
}

window.AgentExcalidraw = { mount };
