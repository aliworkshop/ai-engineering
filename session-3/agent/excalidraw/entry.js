// Bundled for Node by esbuild. Excalidraw's dist imports "roughjs/bin/rough",
// which Node's ESM resolver rejects for having no extension; a bundler resolves
// it the way a browser build would.
import { convertToExcalidrawElements, exportToSvg } from "@excalidraw/excalidraw";

module.exports = { convertToExcalidrawElements, exportToSvg };
