// Package sandbox is the one place model-written code runs.
//
// The idea is small and the whole of it fits in Run below: build a JavaScript
// world that contains nothing except the functions we chose to put in it, drop
// the model's code into that world, and put a clock on it. There is no
// require, no process, no fs, no fetch — not because they are blocked, but
// because they were never there.
//
// Be honest about the limit. This stops accidents, not a determined attacker:
// the interpreter runs inside our own process, so a timeout interrupts the
// code rather than killing a machine out from under it. Production runs
// untrusted code in a disposable container or micro-VM. What does not change
// is the shape — every dangerous capability goes through one mediated door,
// and that door is this file.
package sandbox

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// Result is what came back. It is JSON on purpose: the model reads it, so a
// failure has to be as legible as a success — a program that blew up should
// come back as something to fix, not as a wall.
type Result struct {
	OK    bool     `json:"ok"`
	Value any      `json:"result,omitempty"`
	Error string   `json:"error,omitempty"`
	Logs  []string `json:"logs"`
	Hint  string   `json:"hint,omitempty"`
}

// Run executes code with api as the only thing it can reach.
//
// api is a plain map of name to Go function. The interpreter converts the
// arguments and the return value, so a function written for Go is a function
// the model's JavaScript can call — and a function left out of the map is one
// that does not exist inside.
func Run(code string, api map[string]any, timeout time.Duration) Result {
	vm := goja.New()
	logs := []string{}

	// Go structs reach JavaScript with their GO field names unless you say
	// otherwise — Amount, not amount. The tool description promises the model
	// lowercase field names, and this is what keeps that promise: read them off
	// the json tags, the same names the data has everywhere else.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	// The entire world, in two lines.
	_ = vm.Set("tools", api)
	_ = vm.Set("console", map[string]any{
		"log": func(args ...goja.Value) {
			parts := make([]string, 0, len(args))
			for _, arg := range args {
				parts = append(parts, arg.String())
			}
			logs = append(logs, strings.Join(parts, " "))
		},
	})

	// The clock. An infinite loop never yields, so nothing but an interrupt
	// from outside will stop it.
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt("execution timed out after " + timeout.String())
	})
	defer timer.Stop()

	// Wrapped in a function so the model's code can `return` a result, which
	// is the only way it has of handing anything back.
	value, err := vm.RunString("(function(){" + code + "})()")
	if err != nil {
		return Result{Error: err.Error(), Logs: logs}
	}

	result := Result{OK: true, Logs: logs}
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		// A run that returns nothing reads as silence. Say what to do about
		// it, because the model cannot see why its answer vanished.
		result.Hint = "your code returned no value - end with `return <result>`"
		return result
	}

	// The result has to survive being turned into JSON, because that is how the
	// model will read it. Not everything does: arithmetic on a field the code
	// misspelled produces NaN, and NaN is not a JSON number. Caught here it is
	// a sentence the model can act on; uncaught it is an empty answer with no
	// explanation, which is how this was found.
	exported := value.Export()
	if _, err := json.Marshal(exported); err != nil {
		return Result{Error: "the result cannot be encoded as JSON: " + err.Error(), Logs: logs}
	}
	result.Value = exported
	return result
}
