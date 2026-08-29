// Package evals runs the agent's live evaluations and reports them to
// Braintrust, so a score becomes a point in a series rather than a line of test
// output that scrolls away.
//
// # Why this is a separate module
//
// The Braintrust SDK pulls roughly sixty modules — gRPC, protobuf, OpenTelemetry,
// Google Cloud. The agent itself has two direct dependencies and intends to keep
// them: "raw SDK, no frameworks" is a property of this repo, not an accident.
//
// A nested module keeps both. Go's internal rule is path-prefix based rather
// than module based, so this module can import the parent's internal/ packages;
// and the parent's `go build ./...` and `go test ./...` ignore nested modules
// entirely. The agent's go.mod is untouched by anything in here.
//
// The OpenRouter SDK is pinned to the same version the parent uses. Minimal
// version selection would otherwise happily resolve a newer one here, and an
// eval that grades a different build of the agent than you ship is worse than
// no eval.
//
// # What it does not replace
//
// The Go eval tests in internal/agent stay exactly as they are, and still run
// offline with -short. These are additive: same datasets, same scorers,
// reported somewhere they accumulate.
//
// Run:  cd evals && go test -v
// (needs OPENROUTER_API_KEY and BRAINTRUST_API_KEY in ../.env)
package evals

import (
	"context"
	"os"
	"testing"

	braintrust "github.com/braintrustdata/braintrust-sdk-go"
	"github.com/braintrustdata/braintrust-sdk-go/eval"
	"github.com/joho/godotenv"
	"go.opentelemetry.io/otel/sdk/trace"

	openrouter "github.com/OpenRouterTeam/go-sdk"
	"github.com/aliworkshop/ai-engineering-course/internal/llm"
)

// projectName is the Braintrust project every experiment lands in. Stable on
// purpose: experiments are only comparable to each other within a project, and
// a name that drifts is a history that starts over.
const projectName = "ai-engineering-agent"

// evalModel is the model under evaluation. It travels into each experiment's
// metadata, because "the score dropped" and "the score dropped after we changed
// model" are different findings and the dashboard should be able to tell them
// apart.
const evalModel = "openai/gpt-4o-mini"

// corpusDir is the teacher's reference, relative to this module. The evals read
// the corpus that ships, not a fixture — retrieval graded against a stand-in
// tells you nothing about the agent you run.
const corpusDir = "../corpus"

// setup builds the Braintrust client and the model client, or skips.
//
// Skipping rather than failing is deliberate: a missing key means this machine
// is not set up to report evals, which is not a broken test. The messages say
// exactly which key is missing, because "skipped" with no reason is how a suite
// quietly stops running.
func setup(t *testing.T) (*braintrust.Client, *openrouter.OpenRouter) {
	t.Helper()

	// Loaded before any chdir a case might do, while the relative path resolves.
	_ = godotenv.Load("../.env")

	modelKey := os.Getenv("OPENROUTER_API_KEY")
	if modelKey == "" {
		t.Skip("OPENROUTER_API_KEY not set — nothing to evaluate")
	}
	if os.Getenv("BRAINTRUST_API_KEY") == "" {
		t.Skip("BRAINTRUST_API_KEY not set — get one at braintrust.dev/app/settings and add it to .env")
	}

	tp := trace.NewTracerProvider()
	client, err := braintrust.New(tp, braintrust.WithProject(projectName))
	if err != nil {
		t.Fatalf("braintrust: %v", err)
	}

	// Shutdown flushes the exporter. Without it a fast test exits before the
	// spans are sent and the experiment shows up empty — which looks like a
	// broken integration rather than a missing flush.
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return client, llm.NewOpenRouter(modelKey)
}

// report prints the experiment's permalink. The URL is the actual deliverable —
// a scorecard in scrollback is what we already had.
//
// It takes the result rather than a method value on it, because a failed run
// hands back a nil *Result and reaching for a method on that turns a useful
// error message into a nil-pointer panic.
func report(t *testing.T, name string, res *eval.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res == nil {
		t.Fatalf("%s: no result and no error, which should not happen", name)
	}
	t.Logf("%s: %s", name, res.String())
	if link, linkErr := res.Permalink(); linkErr == nil {
		t.Logf("%s -> %s", name, link)
	}
}

// approve is a stub human answering yes/no to every approval request, matching
// the one the in-repo evals use.
type approve bool

func (a approve) Confirm(string) bool { return bool(a) }
