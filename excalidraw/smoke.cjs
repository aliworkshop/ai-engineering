// Proves the renderer works end to end without involving the Go agent.
const { execFileSync } = require("child_process");
const input = JSON.stringify({
  title: "smoke",
  skeletons: [
    { type: "rectangle", id: "a", x: 40, y: 40, width: 180, height: 60, backgroundColor: "#a5d8ff", label: { text: "Start" } },
    { type: "diamond", id: "b", x: 20, y: 180, width: 220, height: 90, backgroundColor: "#ffec99", label: { text: "Valid?" } },
    { type: "arrow", x: 130, y: 100, width: 0, height: 80, start: { id: "a" }, end: { id: "b" }, label: { text: "go" } },
  ],
});
const out = execFileSync("node", [__dirname + "/render.cjs"], { input, maxBuffer: 64 * 1024 * 1024 });
const { svg, scene } = JSON.parse(out);
console.log("elements:", scene.elements.length, "| svg bytes:", svg.length);
if (!svg.startsWith("<svg")) throw new Error("not an svg");
console.log("OK");
