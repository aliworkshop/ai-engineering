// Bundles Excalidraw twice: once for Node, once for the browser.
//
// Done through esbuild's JS API rather than a CLI line in package.json: the
// --define value has to reach esbuild as the quoted string "production", and
// npm's JSON escaping plus the shell between them strips the quotes, leaving a
// bare identifier that blows up React at load time.
const esbuild = require("esbuild");
const path = require("path");

// The sidecar generate_diagram runs (see render.cjs): the library's rendering
// half, loaded under jsdom, with its CSS reduced to inert text it never uses.
const node = {
  entryPoints: [path.join(__dirname, "entry.js")],
  outfile: path.join(__dirname, "excalidraw-bundle.cjs"),
  bundle: true,
  platform: "node",
  format: "cjs",
  define: { "process.env.NODE_ENV": '"production"' },
  loader: { ".css": "text" },
  logLevel: "error",
};

// The editor the browser UI mounts (see editor.jsx): the whole app, styles
// included. esbuild writes the imported CSS beside the JS as editor-bundle.css,
// and the Go server serves the pair.
const browser = {
  entryPoints: [path.join(__dirname, "editor.jsx")],
  outfile: path.join(__dirname, "editor-bundle.js"),
  bundle: true,
  platform: "browser",
  format: "iife",
  // The runtime imports React itself, so nothing has to be in scope here.
  jsx: "automatic",
  // Excalidraw's package exports gate index.css behind a development /
  // production condition and offer no default, so without this the stylesheet
  // simply doesn't resolve.
  conditions: ["production"],
  define: { "process.env.NODE_ENV": '"production"' },
  // The UI font is the one asset the stylesheet reaches for by relative URL.
  // Inlining it (~110 KB) keeps the build to two files that can be served from
  // anywhere, rather than a directory whose layout the server has to mirror.
  // The hand-drawn canvas fonts are not in here — those are fetched at runtime
  // from window.EXCALIDRAW_ASSET_PATH.
  loader: { ".woff2": "dataurl" },
  minify: true,
  // One known consequence of bundling as an IIFE: Excalidraw looks for its font
  // subsetting worker relative to a module URL there isn't one of, logs
  // "WorkerUrlNotDefinedError" once per export, and does the work on the main
  // thread instead. The export is identical; only the console is noisier.
  logLevel: "error",
};

Promise.all([esbuild.build(node), esbuild.build(browser)]).catch(() =>
  process.exit(1),
);
