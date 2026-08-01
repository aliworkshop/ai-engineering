// The Excalidraw renderer, run as a sidecar by the generate_diagram tool.
//
// Reads {"title": ..., "skeletons": [...]} on stdin and writes
// {"svg": "...", "scene": {...}} on stdout. The skeletons are exactly the
// ExcalidrawElementSkeleton format:
//   https://docs.excalidraw.com/docs/@excalidraw/excalidraw/api/excalidraw-element-skeleton
//
// Two real Excalidraw APIs do the work:
//   convertToExcalidrawElements  — skeletons → fully qualified elements
//   exportToSvg                  — elements → the rendered drawing
//
// Excalidraw is a browser library, so getting it to run under Node takes a
// deliberate environment. Everything below is that environment, and each piece
// is here because the bundle fails to load without it:
//
//   jsdom            — window/document/DOM. The library reaches for these at
//                      module scope, not just at call time.
//   canvas (native)  — jsdom's getContext("2d") returns null without it, and
//                      the bundle evaluates `"filter" in ctx` while loading.
//   devicePixelRatio — read as a bare global, not off window.
//   FontFace/fonts   — jsdom has no font loading API; exportToSvg registers
//                      Excalidraw's fonts through it.
//
// The bundle itself is built by `npm install` (see package.json): Excalidraw's
// dist imports "roughjs/bin/rough", which Node's ESM resolver rejects because
// it has no extension. A bundler resolves it the way a browser build would.

const fs = require("fs");
const path = require("path");
const { JSDOM } = require("jsdom");

const dom = new JSDOM("<!doctype html><html><body></body></html>", { pretendToBeVisual: true });
const w = dom.window;

// Some globals (navigator, location) are getter-only on globalThis in Node 22,
// so plain assignment throws — fall back to defineProperty.
const define = (key, value) => {
  try {
    globalThis[key] = value;
  } catch {
    Object.defineProperty(globalThis, key, { value, configurable: true });
  }
};

for (const key of [
  "window", "document", "navigator", "location", "HTMLElement", "HTMLCanvasElement",
  "HTMLImageElement", "Image", "DOMParser", "XMLSerializer", "Node", "SVGElement",
  "Element", "CustomEvent", "MutationObserver", "Blob", "FileReader", "DOMRect",
  "getComputedStyle", "requestAnimationFrame", "cancelAnimationFrame",
]) {
  if (w[key] !== undefined) define(key, w[key]);
}

define("self", globalThis);
define("devicePixelRatio", 1);
define("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
define("matchMedia", () => ({
  matches: false, media: "", onchange: null,
  addListener() {}, removeListener() {},
  addEventListener() {}, removeEventListener() {},
  dispatchEvent() { return false; },
}));
w.matchMedia = globalThis.matchMedia;

// Font loading: jsdom implements none of it, and exportToSvg registers the
// hand-drawn faces before rendering. Accepting and ignoring is enough — the SVG
// references fonts by name rather than embedding metrics.
class FontFaceStub {
  constructor(family, source, descriptors) {
    this.family = family;
    this.source = source;
    Object.assign(this, descriptors || {});
    this.status = "loaded";
  }
  load() { return Promise.resolve(this); }
}
define("FontFace", FontFaceStub);

const registered = new Set();
const fonts = {
  add(f) { registered.add(f); return fonts; },
  delete(f) { return registered.delete(f); },
  clear() { registered.clear(); },
  check() { return true; },
  load() { return Promise.resolve([]); },
  ready: Promise.resolve(),
  forEach(cb) { registered.forEach(cb); },
  values() { return registered.values(); },
  keys() { return registered.keys(); },
  entries() { return registered.entries(); },
  [Symbol.iterator]() { return registered[Symbol.iterator](); },
  get size() { return registered.size; },
  addEventListener() {},
  removeEventListener() {},
};
try {
  Object.defineProperty(w.document, "fonts", { value: fonts, configurable: true });
} catch { /* older jsdom already defines it */ }

const bundlePath = path.join(__dirname, "excalidraw-bundle.cjs");
if (!fs.existsSync(bundlePath)) {
  console.error(
    "excalidraw-bundle.cjs is missing. Run `npm install` in " + __dirname + " to build it."
  );
  process.exit(2);
}
const { convertToExcalidrawElements, exportToSvg } = require(bundlePath);

// exportToSvg sets xmlns on the root element itself, and jsdom's XMLSerializer
// then emits the namespace again — so the serialized root carries xmlns twice.
// That is not well-formed XML: a browser opening canvas.svg shows "Attribute
// xmlns redefined" instead of the drawing, which defeats the point of writing
// an SVG at all. Keep the first occurrence of any repeated attribute.
function dedupeRootAttributes(svg) {
  const match = /^<svg\b([^>]*)>/.exec(svg);
  if (!match) return svg;

  const seen = new Set();
  const kept = [];
  const attr = /([\w:.-]+)\s*=\s*"([^"]*)"/g;
  let m;
  while ((m = attr.exec(match[1])) !== null) {
    if (seen.has(m[1])) continue;
    seen.add(m[1]);
    kept.push(`${m[1]}="${m[2]}"`);
  }
  return svg.replace(match[0], `<svg ${kept.join(" ")}>`);
}

const readStdin = () =>
  new Promise((resolve, reject) => {
    let data = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => (data += chunk));
    process.stdin.on("end", () => resolve(data));
    process.stdin.on("error", reject);
  });

(async () => {
  const input = JSON.parse(await readStdin());
  const skeletons = input.skeletons || [];
  if (skeletons.length === 0) {
    throw new Error("no skeletons on stdin");
  }

  // regenerateIds:false keeps the ids this tool assigned, so redrawing the same
  // diagram produces the same scene rather than one that merely looks alike.
  const elements = convertToExcalidrawElements(skeletons, { regenerateIds: false });

  const appState = {
    viewBackgroundColor: "#ffffff",
    exportWithDarkMode: false,
    exportBackground: true,
  };
  const svgEl = await exportToSvg({ elements, appState, files: null });
  const svg = dedupeRootAttributes(new w.XMLSerializer().serializeToString(svgEl));

  const scene = {
    type: "excalidraw",
    version: 2,
    source: "https://excalidraw.com",
    elements,
    appState: { gridSize: null, viewBackgroundColor: "#ffffff" },
    files: {},
  };

  process.stdout.write(JSON.stringify({ svg, scene }));
})().catch((err) => {
  console.error(err && err.stack ? err.stack : String(err));
  process.exit(1);
});
