package attempt

import (
	"encoding/json"
	"fmt"
)

const configVersion = 1

type normalizedConfig struct {
	Version          int                 `json:"version"`
	Kind             string              `json:"kind"`
	Checker          checkerSpec         `json:"checker"`
	Interactor       *interactorSpec     `json:"interactor,omitempty"`
	TimeLimitMS      int64               `json:"time_limit_ms"`
	MemoryLimitBytes int64               `json:"memory_limit_bytes"`
	Subtasks         []normalizedSubtask `json:"subtasks"`
	Cases            []normalizedCase    `json:"cases"`
}

type checkerSpec struct {
	Kind       string `json:"kind"`
	SourceName string `json:"source_name,omitempty"`
}

type interactorSpec struct {
	SourceName string `json:"source_name"`
}

type normalizedSubtask struct {
	ID      string   `json:"id"`
	Score   score    `json:"score"`
	CaseIDs []string `json:"case_ids"`
}

type normalizedCase struct {
	ID               string `json:"id"`
	InputName        string `json:"input_name"`
	OutputName       string `json:"output_name,omitempty"`
	TimeLimitMS      *int64 `json:"time_limit_ms,omitempty"`
	MemoryLimitBytes *int64 `json:"memory_limit_bytes,omitempty"`
}

func parseConfig(raw string) (normalizedConfig, error) {
	var cfg normalizedConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return normalizedConfig{}, err
	}
	if err := cfg.validate(); err != nil {
		return normalizedConfig{}, err
	}
	return cfg, nil
}

func (c normalizedConfig) validate() error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Kind != kindBatch && c.Kind != kindInteractive {
		return fmt.Errorf("unsupported problem kind %q", c.Kind)
	}
	if c.Kind == kindInteractive && (c.Interactor == nil || c.Interactor.SourceName == "") {
		return fmt.Errorf("interactive problems require an interactor")
	}
	switch c.Checker.Kind {
	case checkerExact, checkerToken, checkerTestlib:
	default:
		return fmt.Errorf("unsupported checker kind %q", c.Checker.Kind)
	}
	if c.Checker.Kind == checkerTestlib && c.Checker.SourceName == "" {
		return fmt.Errorf("testlib checker requires a source file")
	}
	if c.TimeLimitMS <= 0 || c.MemoryLimitBytes <= 0 {
		return fmt.Errorf("time and memory limits must be positive")
	}
	if len(c.Cases) == 0 || len(c.Subtasks) == 0 {
		return fmt.Errorf("at least one case and subtask are required")
	}
	seenCase := map[string]struct{}{}
	for _, cs := range c.Cases {
		if cs.ID == "" || cs.InputName == "" {
			return fmt.Errorf("case id and input are required")
		}
		if c.Kind == kindBatch && cs.OutputName == "" && (c.Checker.Kind == checkerExact || c.Checker.Kind == checkerToken) {
			return fmt.Errorf("case %s missing output", cs.ID)
		}
		if _, ok := seenCase[cs.ID]; ok {
			return fmt.Errorf("duplicate case %s", cs.ID)
		}
		seenCase[cs.ID] = struct{}{}
	}
	seenSub := map[string]struct{}{}
	used := map[string]struct{}{}
	for _, st := range c.Subtasks {
		if st.ID == "" {
			return fmt.Errorf("subtask id required")
		}
		if _, ok := seenSub[st.ID]; ok {
			return fmt.Errorf("duplicate subtask %s", st.ID)
		}
		seenSub[st.ID] = struct{}{}
		if len(st.CaseIDs) == 0 {
			return fmt.Errorf("subtask %s has no cases", st.ID)
		}
		for _, id := range st.CaseIDs {
			if _, ok := seenCase[id]; !ok {
				return fmt.Errorf("subtask %s references unknown case %s", st.ID, id)
			}
			used[id] = struct{}{}
		}
	}
	if len(used) != len(seenCase) {
		return fmt.Errorf("every case must belong to a subtask")
	}
	return nil
}
