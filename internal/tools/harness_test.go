package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// yes is a human who approves everything, so a test can reach the tool itself
// rather than the gate in front of it.
type yes struct{}

func (yes) Confirm(string) bool { return true }

// TestSensitiveToolsAreDeclared is the list everything else keys off. If a new
// dangerous tool forgets its Sensitive method it silently becomes reachable
// from sandboxed code and from the generalist agent — so it is worth an
// explicit test rather than a convention.
func TestSensitiveToolsAreDeclared(t *testing.T) {
	var ran bool
	registry := Default(yes{}, WithExtra(gated{Approver: yes{}, ran: &ran}))

	dangerous := map[string]bool{"change_thing": true}
	safe := map[string]bool{"get_weather": true}

	for _, name := range registry.Names() {
		tool := registry.byName[name]
		switch {
		case dangerous[name] && !isSensitive(tool):
			t.Errorf("%s changes the world but does not declare Sensitive()", name)
		case safe[name] && isSensitive(tool):
			t.Errorf("%s is read-only but declares Sensitive()", name)
		}
	}

	for _, name := range registry.SafeNames() {
		if dangerous[name] {
			t.Errorf("SafeNames included the dangerous tool %s", name)
		}
	}
}

// TestSandboxSeesNoDangerousTools is the Part 3 promise, checked at the seam
// where it is actually decided.
func TestSandboxSeesNoDangerousTools(t *testing.T) {
	var ran bool
	registry := Default(yes{}, WithSandbox(t.TempDir()),
		WithExtra(gated{Approver: yes{}, ran: &ran}))

	exposed := sandboxable(registry)
	for _, name := range exposed {
		if isSensitive(registry.byName[name]) {
			t.Fatalf("sandboxed code can reach %s", name)
		}
		if name == "run_code" {
			t.Fatalf("run_code exposed to itself; a sandbox that can spawn a sandbox has no timeout")
		}
	}
	if len(exposed) == 0 {
		t.Fatalf("code mode with no tools at all is just a sandbox")
	}

	// And the wiring actually happened — an unconfigured RunCode would run code
	// with no bridge and quietly lose half the point.
	code, ok := registry.byName["run_code"].(*RunCode)
	if !ok {
		t.Fatalf("run_code is not a *RunCode: %T", registry.byName["run_code"])
	}
	if code.Dispatch == nil || len(code.Expose) == 0 {
		t.Fatalf("run_code was added without its tool bridge wiring")
	}
}

// TestSubsetIsLeastPrivilege: an agent given a subset holds exactly that, and a
// tool it does not hold is not merely discouraged — it is not there.
func TestSubsetIsLeastPrivilege(t *testing.T) {
	var ran bool
	registry := Default(yes{}, WithSandbox(t.TempDir()),
		WithExtra(gated{Approver: yes{}, ran: &ran}))
	assistant := registry.Subset("get_weather", "run_code")

	if len(assistant.Names()) != 2 {
		t.Fatalf("subset holds %v, want exactly the two named", assistant.Names())
	}
	for _, forbidden := range []string{"change_thing"} {
		if assistant.Has(forbidden) {
			t.Fatalf("%s leaked into the restricted subset", forbidden)
		}
	}

	// Advertised specs must match too — a tool the model is told about but the
	// registry cannot dispatch is worse than not having it.
	if got := len(assistant.Specs()); got != 2 {
		t.Fatalf("subset advertises %d tools, holds 2", got)
	}
	if out := assistant.Dispatch(context.Background(), "change_thing", `{"what":"x"}`); !strings.Contains(out, "unknown tool") {
		t.Fatalf("a restricted agent dispatched a tool it does not hold: %q", out)
	}

	// A name that doesn't exist is skipped, not fatal: a roster is
	// configuration, and a typo in it should not take the process down.
	if got := registry.Subset("get_weather", "no_such_tool").Names(); len(got) != 1 {
		t.Fatalf("unknown names should be skipped, got %v", got)
	}
}

// TestHandoffValidatesItsTarget keeps an invented agent name from becoming a
// silent no-op the model can't diagnose.
func TestHandoffValidatesItsTarget(t *testing.T) {
	tool := Handoff{To: map[string]string{"operator": "changes files"}}

	out, err := tool.Run(context.Background(), `{"to":"operator","reason":"needs a write"}`)
	if err != nil {
		t.Fatalf("valid handoff failed: %v", err)
	}
	var marker HandoffResult
	if err := json.Unmarshal([]byte(out), &marker); err != nil {
		t.Fatalf("handoff result is not decodable: %v", err)
	}
	if !marker.Handoff || marker.To != "operator" {
		t.Fatalf("handoff marker is wrong: %+v", marker)
	}

	if _, err := tool.Run(context.Background(), `{"to":"nobody","reason":"x"}`); err == nil {
		t.Fatalf("expected an unknown agent name to be reported")
	} else if !strings.Contains(err.Error(), "operator") {
		t.Fatalf("the error should list the agents that DO exist: %v", err)
	}
}

// TestHandoffIsOptIn: without specialists configured there is no handoff tool,
// which is the right default for a single-agent setup.
func TestHandoffIsOptIn(t *testing.T) {
	if Default(yes{}).Has(HandoffTool) {
		t.Fatalf("handoff was advertised with no specialists to hand off to")
	}
	withSpecialists := Default(yes{}, WithSpecialists(map[string]string{"operator": "changes files"}))
	if !withSpecialists.Has(HandoffTool) {
		t.Fatalf("handoff should appear once specialists exist")
	}
}

// TestRunCodeIsOptIn guards the reason code mode is a flag: turning it on
// changes how the agent solves things, and the eval suite depends on the old
// shape.
func TestRunCodeIsOptIn(t *testing.T) {
	if Default(yes{}).Has("run_code") {
		t.Fatalf("run_code appeared without WithSandbox")
	}
	if !Default(yes{}, WithSandbox(t.TempDir())).Has("run_code") {
		t.Fatalf("WithSandbox did not enable run_code")
	}
}

// TestRunCodeReachesTheAgentsTools is code mode end to end: the model writes a
// program, the program calls a tool, the answer comes back in one turn.
func TestRunCodeReachesTheAgentsTools(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}

	var seen []string
	tool := RunCode{
		Scratch: t.TempDir(),
		Dispatch: func(_ context.Context, name, args string) string {
			seen = append(seen, name)
			return `{"temp_c": 21}`
		},
		Expose: []string{"get_weather"},
	}

	out, err := tool.Run(context.Background(), mustJSON(t, map[string]string{
		"language": "python",
		"code": `import agent_tools, json
data = json.loads(agent_tools.call("get_weather", city="Tehran"))
print("F:", round(data["temp_c"] * 9 / 5 + 32))`,
	}))
	if err != nil {
		t.Fatalf("run_code: %v", err)
	}

	var result struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if !result.OK {
		t.Fatalf("program failed: %s\n%s", result.Error, result.Output)
	}
	// The arithmetic happened inside the program, not across three model turns.
	if !strings.Contains(result.Output, "F: 70") {
		t.Fatalf("expected the converted temperature, got %q", result.Output)
	}
	if len(seen) != 1 || seen[0] != "get_weather" {
		t.Fatalf("tool bridge saw %v", seen)
	}
}

// TestRunCodeRejectsUnknownLanguages keeps the interpreter list closed, so a
// typo is an error rather than an unexpected program.
func TestRunCodeRejectsUnknownLanguages(t *testing.T) {
	tool := RunCode{Scratch: t.TempDir()}
	if _, err := tool.Run(context.Background(), `{"language":"ruby","code":"puts 1"}`); err == nil {
		t.Fatalf("expected an unsupported language to be refused")
	}
	if _, err := tool.Run(context.Background(), `{"language":"python","code":"   "}`); err == nil {
		t.Fatalf("expected empty code to be refused")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
