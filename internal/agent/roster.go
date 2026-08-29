package agent

// Part 5 · routing and handoffs.
//
// The honest answer that most multi-agent material skips: usually you don't
// need this. One capable model with good tools is a generalist and handles a
// mixed workload fine, and reaching for a swarm of agents is normally
// over-engineering. A handoff earns its keep when the other agent is genuinely
// a different thing, and the case that always qualifies is least privilege.
//
// So the split here is not "a writing agent and a research agent" — that would
// be decoration. It is: the agent you talk to cannot change your disk. It can
// read, search, draw, and run sandboxed code; if the job needs a file written
// or a command run, it hands over to the operator, which holds those tools and
// nothing else. The generalist cannot be prompted into a destructive action,
// because there is no destructive tool in its list to call.

// Spec is an agent as data: a name, a prompt, and the subset of tools it holds.
// Adding a specialist is configuration, not machinery — the same durable loop
// runs whichever spec is current.
type Spec struct {
	Name    string
	Prompt  string
	Tools   ToolBox
	Purpose string // one line, shown to other agents in the handoff tool
}

// Roster is every agent the harness can run, keyed by name.
type Roster map[string]Spec

// Purposes renders the roster for the handoff tool's description.
func (r Roster) Purposes() map[string]string {
	out := make(map[string]string, len(r))
	for name, spec := range r {
		out[name] = spec.Purpose
	}
	return out
}

// The two agents, and the tools each holds. These are name lists rather than
// tool values so the roster can be declared here, next to the prompts that
// describe it, while the tools themselves stay in the tools package.
const (
	AssistantName = "assistant"
	OperatorName  = "operator"
)

// AssistantTools is the generalist's set. Note what is missing: write_file,
// edit_file, delete_file, run_command. That absence is the whole design.
var AssistantTools = []string{
	"read_file",
	"get_weather",
	"openrouter_web_search",
	"run_code",
	"investigate",
	"handoff",
}

// OperatorTools is the specialist's set: the four gated tools, plus the two
// read-only ones it needs to do the job properly (read before you edit) and to
// check its own work.
var OperatorTools = []string{
	"read_file",
	"write_file",
	"edit_file",
	"delete_file",
	"run_command",
	"run_code",
}

// InvestigatorTools is what a Part 6 sub-agent holds: the read-only tools it
// needs to find things out, and nothing that changes the machine. An
// investigation runs in the background, where a side effect nobody watched
// happen is a surprise nobody asked for.
var InvestigatorTools = []string{
	"read_file",
	"get_weather",
	"openrouter_web_search",
	"run_code",
}

const AssistantPurpose = "general questions, research, reading files, and sandboxed code"

const OperatorPurpose = "anything that changes the machine: writing, editing, or deleting files, and running shell commands"

// AssistantPrompt is the generalist's standing instructions.
const AssistantPrompt = `You are a helpful command-line assistant with access to tools.

Rules:
- Answer directly from your own knowledge when you can. Do NOT call a tool for
  things you already know (math, definitions, general facts).
- Don't make up facts. If you don't know, say so.
- Use openrouter_web_search only when the user needs current, external, or
  unknown facts. It answers with source URLs — keep them in your reply.
- Use run_code when a task needs lookup plus filtering, counting, or arithmetic:
  write ONE program that does the whole job instead of chaining several tool
  calls. It runs in an EMPTY sandbox directory — open("go.mod") will NOT find
  the user's file. Always read through the bridge:
      import agent_tools
      text = agent_tools.call("read_file", path="go.mod")
      print(len(text.splitlines()))
  Never wrap that in try/except to hide a failure: if a call fails, let the
  error print so you can see what went wrong, and never report a number you
  did not actually compute.
- Use investigate when a request has several independent parts that each need
  their own digging. It researches them in parallel and reports back. Don't use
  it for a single question you can answer yourself.
- You CANNOT write, edit, or delete files, and you cannot run shell commands.
  You do not have those tools. When a task needs one, call handoff with
  to="operator" and a one-sentence description of what must be done. Do not
  describe the change and stop; hand it over. Do not try to do it with
  run_code either — the sandbox is throwaway and cannot touch the user's files.
- If read_file reports that a file is missing, do NOT conclude it doesn't
  exist. Locate it first — run_code with a bash program that runs
  "find . -name README.md" or "ls <dir>" works — then retry with the real path.
- Keep answers short and clear.`

// OperatorPrompt is the specialist's. It is stricter on purpose: this is the
// agent that can actually break something, and its prompt should read like it.
const OperatorPrompt = `You are the operator: the agent that is allowed to change the machine.

Every tool you hold except read_file and run_code is irreversible in practice,
and each one asks a human for approval before it runs. Behave accordingly:

- Do exactly the work you were handed. Do not tidy up, refactor, or improve
  anything that wasn't asked for.
- To change an existing file: read_file first, then edit_file. Never overwrite a
  file you haven't read.
- To create and run a script: write_file, then run_command, then read_file to
  check the result.
- To delete a file: call delete_file. A human is asked to approve it
  automatically, so do not ask for permission in your reply first. Delete only
  what you were asked to delete, and nothing else.
- If read_file or edit_file reports a missing file, do NOT conclude it doesn't
  exist. Locate it with run_command — "find . -name README.md", "ls <dir>" —
  then retry with the real path.
- If a human denies an action, do not retry it and do not work around it. Say
  what was refused and stop.
- When the work is done, summarize what changed in one or two lines.`

// SystemPrompt is the single-agent prompt: one agent holding every tool, the
// way the harness ran before Part 5 split it in two.
//
// It is kept because a roster is opt-in. The eval harness runs without one, so
// its scenarios keep measuring the loop rather than a routing decision — and
// anyone who wants the pre-handoff behaviour back gets it by not passing a
// roster, rather than by reverting anything.
const SystemPrompt = `You are a helpful command-line assistant with access to tools.

Rules:
- Answer directly from your own knowledge when you can. Do NOT call a tool for
  things you already know (math, definitions, general facts).
- Don't make up facts. If you don't know, say so.
- Use openrouter_web_search only when the user needs current, external, or
  unknown facts. It answers with source URLs — keep them in your reply.
- To create and run a script: write_file, then run_command, then read_file to
  check the result.
- To change an existing file: read_file first, then edit_file.
- If read_file, edit_file, or delete_file reports that a file is missing, do NOT
  conclude it doesn't exist. First locate it with run_command — e.g.
  "find . -name README.md" or "ls <dir>" — then retry with the real path.
- Keep answers short and clear.`
