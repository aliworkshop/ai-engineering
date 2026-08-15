package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// handoffMarker is the shape the handoff tool returns. The agent package
// decodes it itself rather than importing the tools package, because the
// dependency points the other way — the agent knows an abstract ToolBox, not
// any concrete tool. What crosses the boundary is a documented JSON shape,
// which is exactly what crosses it between the model and the harness anyway.
type handoffMarker struct {
	Handoff bool   `json:"handoff"`
	To      string `json:"to"`
	Reason  string `json:"reason"`
}

// decodeHandoff reads a transfer request out of a tool result, and reports
// false for anything else — so the loop can try it on every result without
// first checking which tool produced it.
func decodeHandoff(result string) (handoffMarker, bool) {
	var h handoffMarker
	if err := json.Unmarshal([]byte(result), &h); err != nil || !h.Handoff || h.To == "" {
		return handoffMarker{}, false
	}
	return h, true
}

// newWorkflowID names a run. Short because a human types it into the approve
// command, random because workflows from different sessions share a directory.
func newWorkflowID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "wf000000"
	}
	return hex.EncodeToString(b[:])
}
