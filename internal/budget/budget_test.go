package budget

import (
	"errors"
	"sync"
	"testing"
)

func TestLedgerEnforcesTokenAndTaskLimits(t *testing.T) {
	ledger, err := NewLedger(Limits{
		MaxTotalTokens:       1000,
		MaxTaskContextTokens: 400,
		MaxMicroTasks:        2,
		MaxStepsPerTask:      2,
	})
	if err != nil {
		t.Fatalf("NewLedger failed: %v", err)
	}

	t1, err := ledger.StartTask("t1")
	if err != nil {
		t.Fatalf("StartTask(t1) failed: %v", err)
	}

	// Single-task context window cap
	if err := t1.CheckPrompt(450); !errors.Is(err, ErrTaskContextExceeded) {
		t.Fatalf("expected ErrTaskContextExceeded, got %v", err)
	}

	// Step cap inside micro-task
	if step, err := t1.NextStep(); err != nil || step != 1 {
		t.Fatalf("NextStep 1 = (%d, %v)", step, err)
	}
	if step, err := t1.NextStep(); err != nil || step != 2 {
		t.Fatalf("NextStep 2 = (%d, %v)", step, err)
	}
	if _, err := t1.NextStep(); !errors.Is(err, ErrMaxTaskStepsExceeded) {
		t.Fatalf("expected ErrMaxTaskStepsExceeded, got %v", err)
	}

	if err := t1.RecordTokens(300, 200); err != nil {
		t.Fatalf("RecordTokens failed: %v", err)
	}
	t1.RecordToolCall()
	t1.Complete()
	t1.Complete() // idempotent

	t2, err := ledger.StartTask("t2")
	if err != nil {
		t.Fatalf("StartTask(t2) failed: %v", err)
	}
	if err := t2.CheckPrompt(350); err != nil {
		t.Fatalf("expected 350 prompt tokens to fit in 500 remaining, got %v", err)
	}
	if err := t2.RecordTokens(350, 200); !errors.Is(err, ErrRunTokenBudgetExceeded) {
		t.Fatalf("expected ErrRunTokenBudgetExceeded, got %v", err)
	}

	// Max micro-task cap
	if _, err := ledger.StartTask("t3"); !errors.Is(err, ErrMaxMicroTasksExceeded) {
		t.Fatalf("expected ErrMaxMicroTasksExceeded, got %v", err)
	}

	stats := ledger.Stats()
	if stats.TasksStarted != 2 || stats.TasksCompleted != 1 || stats.ToolCalls != 1 || stats.RemainingTokens != 0 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestLedgerConcurrentSafety(t *testing.T) {
	ledger, err := NewLedger(Limits{
		MaxTotalTokens:       10000,
		MaxTaskContextTokens: 1000,
		MaxMicroTasks:        20,
		MaxStepsPerTask:      5,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tb, err := ledger.StartTask("worker")
			if err != nil {
				t.Errorf("StartTask: %v", err)
				return
			}
			_, _ = tb.NextStep()
			_ = tb.RecordTokens(50, 25)
			tb.RecordToolCall()
			tb.Complete()
		}(i)
	}
	wg.Wait()

	stats := ledger.Stats()
	if stats.TotalTokens != 750 || stats.TasksCompleted != 10 || stats.ToolCalls != 10 {
		t.Errorf("unexpected concurrent stats: %+v", stats)
	}
}
