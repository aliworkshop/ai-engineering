package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Bridge is what makes this code mode rather than just a sandbox: the program
// the model writes can call the agent's own tools.
//
// Without it the model has to choose between "write a program" and "use the
// tools", and it will pick badly. With it, one script can read a file, search
// the web, and do the arithmetic in between — one turn instead of six.
//
// The channel is a unix socket inside the scratch directory, which is a
// deliberate choice: it lives and dies with the run, it is reachable only by
// something already inside the sandbox, and it exposes exactly the tools handed
// to Serve — never the dangerous ones, which stay behind the approval gate
// where a human can see them.
type Bridge struct {
	listener net.Listener
	path     string
	call     Caller
	allowed  map[string]bool
	names    []string

	wg     sync.WaitGroup
	closed chan struct{}
}

// Caller runs one tool by name with JSON arguments and returns its result.
// tools.Registry.Dispatch has exactly this shape, which is the point — the
// bridge exposes the agent's real tools, not a parallel implementation that
// can drift from them.
type Caller func(ctx context.Context, name, args string) string

// request and response are the wire format: one JSON object per line, in each
// direction. Line-delimited JSON because it is the format every language in the
// sandbox can produce with its standard library and no dependencies.
type request struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type response struct {
	OK     bool   `json:"ok"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Serve starts a bridge on a socket for this run, exposing only the named
// tools. The caller closes it when the run is over.
func Serve(dir string, call Caller, expose []string) (*Bridge, error) {
	listener, path, err := listen(dir)
	if err != nil {
		return nil, err
	}

	b := &Bridge{
		listener: listener,
		path:     path,
		call:     call,
		allowed:  make(map[string]bool, len(expose)),
		names:    append([]string(nil), expose...),
		closed:   make(chan struct{}),
	}
	for _, n := range expose {
		b.allowed[n] = true
	}
	sort.Strings(b.names)

	b.wg.Add(1)
	go b.accept()
	return b, nil
}

// sunPathMax is the practical ceiling on a unix socket path. The kernel struct
// is 104 bytes on macOS and 108 on Linux; the smaller number is the safe one,
// and a few bytes of headroom costs nothing.
const sunPathMax = 100

// listen opens the socket, preferring the run's own scratch directory — the
// socket then lives and dies with the run, and only something already inside
// the sandbox can reach it.
//
// The fallback exists because a unix socket path has a hard length limit that
// has nothing to do with the filesystem's. A deep TMPDIR (macOS hands out
// /var/folders/xx/…/T/ paths, and a temp dir under one gets long fast) blows
// past it with a bind: invalid argument that reads like a permissions problem
// and isn't. Falling back to a short path in the system temp directory keeps
// code mode working there; losing the tool bridge over a long pathname would be
// an absurd way to degrade.
func listen(dir string) (net.Listener, string, error) {
	candidates := []string{filepath.Join(dir, "tools.sock")}

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err == nil {
		candidates = append(candidates,
			filepath.Join(os.TempDir(), "ag"+hex.EncodeToString(suffix[:])+".sock"))
	}

	var lastErr error
	for _, path := range candidates {
		if len(path) > sunPathMax {
			lastErr = fmt.Errorf("socket path is %d bytes, over the %d-byte limit", len(path), sunPathMax)
			continue
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			lastErr = err
			continue
		}
		return listener, path, nil
	}
	return nil, "", fmt.Errorf("sandbox: start tool bridge: %w", lastErr)
}

// Env is what the sandboxed process needs to find the bridge.
func (b *Bridge) Env() []string { return []string{"AGENT_TOOLS_SOCKET=" + b.path} }

// Close stops accepting and waits for in-flight calls, so a tool that is
// halfway through writing a file is not abandoned when the script exits.
func (b *Bridge) Close() error {
	select {
	case <-b.closed:
		return nil // already closed
	default:
		close(b.closed)
	}
	err := b.listener.Close()
	b.wg.Wait()
	_ = os.Remove(b.path)
	return err
}

func (b *Bridge) accept() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return // listener closed, or the socket went away with the scratch dir
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(conn)
		}()
	}
}

// handle serves one connection, which may carry several calls — a script that
// makes ten tool calls should not pay ten connection setups.
func (b *Bridge) handle(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	for {
		// A sandboxed program that opens the socket and then hangs shouldn't be
		// able to pin a goroutine for the life of the process.
		_ = conn.SetReadDeadline(time.Now().Add(DefaultTimeout))

		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}

		var req request
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			b.reply(conn, response{Error: "malformed request: " + jsonErr.Error()})
			return
		}

		// The allowlist is the security story. A tool that is not on it does not
		// exist as far as sandboxed code is concerned — no approval prompt, no
		// partial execution, just a name that isn't there.
		if !b.allowed[req.Tool] {
			b.reply(conn, response{Error: fmt.Sprintf(
				"tool %q is not available inside the sandbox (available: %s)",
				req.Tool, strings.Join(b.names, ", "))})
			continue
		}

		args := string(req.Args)
		if args == "" || args == "null" {
			args = "{}"
		}

		// The bridge outlives no single call: its own context bounds tool work
		// so a slow tool cannot outlast the sandbox run that asked for it.
		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		result := b.call(ctx, req.Tool, args)
		cancel()

		b.reply(conn, response{OK: true, Result: result})

		if err != nil {
			return // last line of the stream
		}
	}
}

func (b *Bridge) reply(conn net.Conn, resp response) {
	raw, err := json.Marshal(resp)
	if err != nil {
		raw = []byte(`{"ok":false,"error":"bridge failed to encode a reply"}`)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(append(raw, '\n'))
}

// pythonShim is the client half, written into the scratch dir so the model's
// program can `import agent_tools`. It is stdlib-only on purpose: the sandbox
// has no package manager and no network install, and a helper that needs one
// would be a helper nobody can use.
const pythonShim = `"""Tools the agent lends to sandboxed code (Part 3, code mode)."""
import json, os, socket

_PATH = os.environ.get("AGENT_TOOLS_SOCKET")


def call(tool, **args):
    """Run one of the agent's tools. Returns its result as a string."""
    if not _PATH:
        raise RuntimeError("no tool bridge in this sandbox")
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(_PATH)
    try:
        sock.sendall((json.dumps({"tool": tool, "args": args}) + "\n").encode())
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = sock.recv(65536)
            if not chunk:
                break
            buf += chunk
    finally:
        sock.close()
    reply = json.loads(buf.decode() or '{"ok": false, "error": "empty reply"}')
    if not reply.get("ok"):
        raise RuntimeError(reply.get("error", "tool call failed"))
    return reply.get("result", "")


def json_call(tool, **args):
    """Same, but parse the result as JSON when the tool returns JSON."""
    raw = call(tool, **args)
    try:
        return json.loads(raw)
    except ValueError:
        return raw
`

// shellShim gives bash the same door, as a `tools <name> <json>` command. It
// leans on the python shim rather than reimplementing the protocol, so there is
// one client to keep correct instead of two.
const shellShim = `#!/bin/sh
# tools <name> '<json-args>'  — call one of the agent's tools from a shell.
exec python3 -c '
import json, sys
sys.path.insert(0, ".")
import agent_tools
name = sys.argv[1]
args = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}
sys.stdout.write(agent_tools.call(name, **args))
' "$@"
`

// WriteShims drops the client helpers into the scratch dir. Called once per
// run, right after Serve.
func WriteShims(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "agent_tools.py"), []byte(pythonShim), 0o600); err != nil {
		return fmt.Errorf("sandbox: write python shim: %w", err)
	}
	path := filepath.Join(dir, "tools")
	if err := os.WriteFile(path, []byte(shellShim), 0o700); err != nil {
		return fmt.Errorf("sandbox: write shell shim: %w", err)
	}
	return nil
}
