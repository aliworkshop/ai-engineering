package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/aliworkshop/ai-engineering-course/internal/sandbox"
)

// searchKnowledgeBase inside the sandbox is searchKB written twice, once in
// each language. This holds the two to the same answers, and the charges to
// the same field names the runCode description promises the model.
func TestSandboxSearchMatchesSearchKB(t *testing.T) {
	_ = godotenv.Load("../../.env")
	queries := []string{"double BILLING and a refund", "export broke", "pricing?", "nothing at all"}

	raw, _ := json.Marshal(queries)
	got := sandbox.New().Run(context.Background(), `
		const c = tools.getCharges("cus_88121")[0];
		return {
			charge: [c.id, c.amount, c.date, c.description].join("|"),
			search: `+string(raw)+`.map((q) => tools.searchKnowledgeBase(q)),
		};
	`, SandboxWorld(), 2*time.Second)
	if strings.Contains(got.Error, "could not be started") {
		t.Skip("no OpenSandbox server - run `docker compose up -d` and set OPEN_SANDBOX_API_KEY")
	}
	if !got.OK {
		t.Fatalf("run failed: %s", got.Error)
	}

	var seen struct {
		Charge string     `json:"charge"`
		Search [][]string `json:"search"`
	}
	raw, _ = json.Marshal(got.Value)
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}

	if want := "ch_001|4900|2026-05-01|Pro plan - monthly"; seen.Charge != want {
		t.Errorf("charge = %q, want %q", seen.Charge, want)
	}
	for i, q := range queries {
		if want := searchKB(q); !reflect.DeepEqual(seen.Search[i], want) {
			t.Errorf("search(%q) = %q in the sandbox, %q in Go", q, seen.Search[i], want)
		}
	}
}
