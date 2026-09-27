// An external test package, because internal/agent imports this one: the loop
// asks triage which items a task holds and whether a call needs gating.
//
// This test is worth the extra file. ParseItems reads a format that is written
// down in exactly one other place — agent.SampleTask — and nothing but this
// connects them. Change the sample's shape and the parser silently falls back
// to treating the whole task as a single item, which looks like the agent
// working and costs you the triage.
package triage_test

import (
	"reflect"
	"testing"

	"github.com/aliworkshop/ai-engineering-course/internal/agent"
	"github.com/aliworkshop/ai-engineering-course/internal/triage"
)

func TestParseItemsReadsTheSampleTask(t *testing.T) {
	got := triage.ParseItems(agent.SampleTask)
	want := []triage.Item{
		{ID: "item-1", Kind: "customer_message", Text: "I was charged twice and need help."},
		{ID: "item-2", Kind: "bug_report", Text: "The export button fails on Safari."},
		{ID: "item-3", Kind: "sales_request", Text: "Can you send pricing for 50 seats?"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseItems(SampleTask) =\n%+v\nwant\n%+v", got, want)
	}
}
