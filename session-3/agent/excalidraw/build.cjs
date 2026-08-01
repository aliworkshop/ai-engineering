// Bundles Excalidraw for Node.
//
// Done through esbuild's JS API rather than a CLI line in package.json: the
// --define value has to reach esbuild as the quoted string "production", and
// npm's JSON escaping plus the shell between them strips the quotes, leaving a
// bare identifier that blows up React at load time.
const esbuild = require("esbuild");
const path = require("path");

esbuild
  .build({
    entryPoints: [path.join(__dirname, "entry.js")],
    outfile: path.join(__dirname, "excalidraw-bundle.cjs"),
    bundle: true,
    platform: "node",
    format: "cjs",
    define: { "process.env.NODE_ENV": '"production"' },
    loader: { ".css": "text" },
    logLevel: "error",
  })
  .catch(() => process.exit(1));
