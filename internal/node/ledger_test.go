package node

import (
	"path/filepath"
	"testing"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

func TestLedgerPromotesRunningToUncertain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	led, err := openLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &judgev1.DispatchAttempt{
		AttemptId: "att-1", TaskId: "task-1", Generation: 4,
		JudgeDataRevisionId: "rev-1", ExecutionProfileId: "cpp23.gcc.linux-amd64.v0",
	}
	if err := led.begin(d, "node-1"); err != nil {
		t.Fatal(err)
	}

	led2, err := openLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := led2.get("att-1")
	if rec == nil || rec.Phase != phaseUncertain || rec.Reported {
		t.Fatalf("orphan = %+v", rec)
	}
	res := unmarshalResult(rec.Result)
	if res == nil || !res.GetSystemFault() || res.GetSystemFaultKind() != faultUncertainRestart {
		t.Fatalf("result %+v", res)
	}
	if res.GetAttemptId() != "att-1" || res.GetGeneration() != 4 {
		t.Fatalf("identity %+v", res)
	}
}

func TestLedgerFinishPersistsUntilReported(t *testing.T) {
	t.Parallel()
	led, err := openLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &judgev1.DispatchAttempt{AttemptId: "att-2", TaskId: "t", Generation: 1}
	if err := led.begin(d, "node-1"); err != nil {
		t.Fatal(err)
	}
	res := &judgev1.AttemptResult{AttemptId: "att-2", Generation: 1, Verdict: "accepted", Score: "100.00"}
	if err := led.finish("att-2", res); err != nil {
		t.Fatal(err)
	}
	pending := led.unreported()
	if len(pending) != 1 || pending[0].AttemptID != "att-2" {
		t.Fatalf("unreported = %+v", pending)
	}
	if err := led.markReported("att-2"); err != nil {
		t.Fatal(err)
	}
	if n := len(led.unreported()); n != 0 {
		t.Fatalf("unreported after ack = %d", n)
	}
}

func TestLedgerRejectsUnsafeID(t *testing.T) {
	t.Parallel()
	if _, err := ledgerPath("dir", "../x"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ledgerPath("dir", `a\b`); err == nil {
		t.Fatal("expected error")
	}
	p, err := ledgerPath("dir", "att-1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "att-1.json" {
		t.Fatalf("path %s", p)
	}
}
