package dbosrun

import (
	"context"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

// deadGap is how long a pause between two steps has to be before we call the
// process dead rather than slow.
//
// Two seconds is generous. In a live run consecutive steps are MILLISECONDS
// apart, because everything slow — the model call included — is itself a step
// and so is inside the window rather than between two. Any gap you can see is
// a gap where the program was not running.
const deadGap = 2 * time.Second

// Inspect prints the engine's own receipts: every workflow it is holding, or —
// given an id — every step of one, with how long each took.
//
// This is the DBOS answer to -audit and -list, and the interesting difference
// is that we wrote neither of them. Our version needed an event log and a
// script over it; here the engine already recorded each step's name, output
// and duration, because it had to in order to replay them.
func Inspect(ctx context.Context, opt Options, workflowID string) error {
	dctx, closeDBOS, err := Connect(ctx, opt)
	if err != nil {
		return err
	}
	defer closeDBOS()

	if workflowID == "" {
		return listWorkflows(dctx)
	}
	return listSteps(dctx, workflowID)
}

func listWorkflows(dctx dbos.Context) error {
	runs, err := dbos.ListWorkflows(dctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("No workflows in Postgres yet.")
		return nil
	}

	fmt.Printf("%-38s %-10s %-18s %s\n", "ID", "STATUS", "CREATED", "NAME")
	for _, r := range runs {
		fmt.Printf("%-38s %-10s %-18s %s\n",
			r.ID, r.Status, r.CreatedAt.Format("2006-01-02 15:04:05"), r.Name)
	}
	fmt.Println("\nAdd a workflow id to see its steps.")
	return nil
}

func listSteps(dctx dbos.Context, workflowID string) error {
	steps, err := dbos.GetWorkflowSteps(dctx, workflowID)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		fmt.Printf("No steps recorded for %s.\n", workflowID)
		return nil
	}

	fmt.Printf("  %-4s %-40s %-8s %s\n", "#", "STEP", "MS", "OUTPUT")
	var last time.Time
	for _, s := range steps {
		// The gap line is the whole lesson in one row: the wall clock moved,
		// nothing ran, and everything above it came back out of Postgres
		// rather than being done again.
		if !last.IsZero() && !s.StartedAt.IsZero() {
			if gap := s.StartedAt.Sub(last); gap > deadGap {
				fmt.Printf("  ---- %s gap: the process was dead here; everything above was replayed from Postgres ----\n",
					gap.Round(time.Second))
			}
		}
		if !s.CompletedAt.IsZero() {
			last = s.CompletedAt
		}

		var ms int64
		if !s.StartedAt.IsZero() && !s.CompletedAt.IsZero() {
			ms = s.CompletedAt.Sub(s.StartedAt).Milliseconds()
		}
		fmt.Printf("  %-4d %-40s %-8d %s\n", s.StepID, s.StepName, ms, truncate(fmt.Sprint(s.Output), 60))
	}
	return nil
}

func truncate(s string, max int) string {
	flat := []rune(s)
	if len(flat) <= max {
		return s
	}
	return string(flat[:max]) + "…"
}
