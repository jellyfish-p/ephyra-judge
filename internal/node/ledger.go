package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
	"google.golang.org/protobuf/proto"
)

const (
	phaseRunning     = "running"
	phaseFinished    = "finished"
	phaseUncertain   = "uncertain"
	phaseInvalidated = "invalidated"

	faultUncertainRestart = "uncertain_restart"
)

type record struct {
	AttemptID   string `json:"attempt_id"`
	TaskID      string `json:"task_id"`
	RevisionID  string `json:"revision_id"`
	ProfileID   string `json:"profile_id"`
	NodeID      string `json:"node_id"`
	Generation  int32  `json:"generation"`
	Phase       string `json:"phase"`
	Result      []byte `json:"result,omitempty"`
	Reported    bool   `json:"reported"`
	UpdatedUnix int64  `json:"updated_unix"`
}

type ledger struct {
	dir  string
	mu   sync.Mutex
	recs map[string]*record
}

func openLedger(dir string) (*ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &ledger{dir: dir, recs: make(map[string]*record)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(data, &rec) != nil || rec.AttemptID == "" {
			continue
		}
		l.recs[rec.AttemptID] = &rec
	}
	if err := l.promoteOrphans(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *ledger) promoteOrphans() error {
	for _, rec := range l.recs {
		if rec.Phase != phaseRunning {
			continue
		}
		rec.Phase = phaseUncertain
		rec.Reported = false
		rec.Result = mustMarshalResult(uncertainResult(rec))
		rec.UpdatedUnix = time.Now().Unix()
		if err := l.writeLocked(rec); err != nil {
			return err
		}
	}
	return nil
}

func (l *ledger) get(id string) *record {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.recs[id]
	if rec == nil {
		return nil
	}
	cp := *rec
	return &cp
}

func (l *ledger) begin(d *judgev1.DispatchAttempt, nodeID string) error {
	rec := &record{
		AttemptID:   d.GetAttemptId(),
		TaskID:      d.GetTaskId(),
		RevisionID:  d.GetJudgeDataRevisionId(),
		ProfileID:   d.GetExecutionProfileId(),
		NodeID:      nodeID,
		Generation:  d.GetGeneration(),
		Phase:       phaseRunning,
		UpdatedUnix: time.Now().Unix(),
	}
	return l.put(rec)
}

func (l *ledger) finish(id string, res *judgev1.AttemptResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.recs[id]
	if rec == nil {
		return fmt.Errorf("ledger: unknown attempt %s", id)
	}
	if rec.Phase == phaseInvalidated {
		return nil
	}
	rec.Phase = phaseFinished
	rec.Reported = false
	rec.Result = mustMarshalResult(res)
	rec.UpdatedUnix = time.Now().Unix()
	return l.writeLocked(rec)
}

func (l *ledger) markReported(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.recs[id]
	if rec == nil {
		return nil
	}
	rec.Reported = true
	rec.UpdatedUnix = time.Now().Unix()
	return l.writeLocked(rec)
}

func (l *ledger) markInvalidated(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := l.recs[id]
	if rec == nil {
		return nil
	}
	rec.Phase = phaseInvalidated
	rec.UpdatedUnix = time.Now().Unix()
	return l.writeLocked(rec)
}

func (l *ledger) put(rec *record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := *rec
	l.recs[rec.AttemptID] = &cp
	return l.writeLocked(&cp)
}

func (l *ledger) writeLocked(rec *record) error {
	path, err := ledgerPath(l.dir, rec.AttemptID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := atomicWrite(path, data); err != nil {
		return err
	}
	cp := *rec
	l.recs[rec.AttemptID] = &cp
	return nil
}

func (l *ledger) unreported() []*record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*record, 0)
	for _, rec := range l.recs {
		if rec.Reported || rec.Phase == phaseInvalidated || rec.Phase == phaseRunning {
			continue
		}
		if len(rec.Result) == 0 {
			continue
		}
		cp := *rec
		out = append(out, &cp)
	}
	return out
}

func (l *ledger) occupyingIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0)
	for id, rec := range l.recs {
		if rec.Phase == phaseInvalidated {
			continue
		}
		if rec.Phase == phaseRunning || !rec.Reported {
			out = append(out, id)
		}
	}
	return out
}

func ledgerPath(dir, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("illegal attempt id")
	}
	return filepath.Join(dir, id+".json"), nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(path)
		if err := os.Rename(name, path); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

func mustMarshalResult(res *judgev1.AttemptResult) []byte {
	b, err := proto.Marshal(res)
	if err != nil {
		return nil
	}
	return b
}

func unmarshalResult(b []byte) *judgev1.AttemptResult {
	if len(b) == 0 {
		return nil
	}
	res := &judgev1.AttemptResult{}
	if err := proto.Unmarshal(b, res); err != nil {
		return nil
	}
	return res
}

func uncertainResult(rec *record) *judgev1.AttemptResult {
	return &judgev1.AttemptResult{
		AttemptId:           rec.AttemptID,
		TaskId:              rec.TaskID,
		Generation:          rec.Generation,
		JudgeDataRevisionId: rec.RevisionID,
		ExecutionProfileId:  rec.ProfileID,
		NodeId:              rec.NodeID,
		Verdict:             "system_error",
		Score:               "0.00",
		SystemFault:         true,
		SystemFaultKind:     faultUncertainRestart,
	}
}
