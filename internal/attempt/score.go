package attempt

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// score is integer hundredths. Binary floating point is not used, matching
// the server exact.Score contract that AcceptReport recomputes.
type score struct {
	hundredths int64
}

var zeroScore = score{}

func scoreFromInt(n int64) score { return score{hundredths: n * 100} }

func (s score) String() string {
	sign := ""
	h := s.hundredths
	if h < 0 {
		sign = "-"
		h = -h
	}
	return fmt.Sprintf("%s%d.%02d", sign, h/100, h%100)
}

func (s score) add(o score) score {
	return score{hundredths: s.hundredths + o.hundredths}
}

func (s *score) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("score must be a decimal string")
	}
	parsed, err := parseScore(raw)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

func parseScore(raw string) (score, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return score{}, fmt.Errorf("empty score")
	}
	if strings.ContainsAny(s, "eE") {
		return score{}, fmt.Errorf("scientific notation is not allowed")
	}
	neg := false
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return score{}, fmt.Errorf("invalid score")
	}
	wholeStr, fracStr, ok := strings.Cut(s, ".")
	if !ok {
		n, err := parseDigits(wholeStr)
		if err != nil {
			return score{}, err
		}
		if neg {
			n = -n
		}
		return scoreFromInt(n), nil
	}
	if fracStr == "" {
		return score{}, fmt.Errorf("trailing decimal point is not allowed")
	}
	if len(fracStr) > 2 {
		return score{}, fmt.Errorf("score has more than 2 decimal places")
	}
	if wholeStr == "" {
		return score{}, fmt.Errorf("invalid score")
	}
	whole, err := parseDigits(wholeStr)
	if err != nil {
		return score{}, err
	}
	frac, err := parseFraction(fracStr)
	if err != nil {
		return score{}, err
	}
	h := whole*100 + frac
	if neg {
		h = -h
	}
	return score{hundredths: h}, nil
}

func parseDigits(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("invalid number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid number %q", s)
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("leading zeros are not allowed")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number")
	}
	return n, nil
}

func parseFraction(s string) (int64, error) {
	if len(s) == 0 || len(s) > 2 {
		return 0, fmt.Errorf("invalid fraction")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid fraction")
		}
	}
	if len(s) == 1 {
		s += "0"
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid fraction")
	}
	return n, nil
}

type caseOutcome struct {
	id, verdict string
}

type subtaskOutcome struct {
	id, verdict string
	score       score
}

type computedScore struct {
	total    score
	verdict  string
	subtasks []subtaskOutcome
}

// computeSubmission mirrors server scoring.ComputeSubmissionScore so AcceptReport
// cannot reject the summary as a contradiction.
func computeSubmission(cfg normalizedConfig, cases []caseOutcome, compilerFailed bool) (computedScore, error) {
	if compilerFailed {
		subs := make([]subtaskOutcome, 0, len(cfg.Subtasks))
		for _, st := range cfg.Subtasks {
			subs = append(subs, subtaskOutcome{id: st.ID, score: zeroScore, verdict: verdictCompilationError})
		}
		return computedScore{total: zeroScore, verdict: verdictCompilationError, subtasks: subs}, nil
	}
	byID := make(map[string]caseOutcome, len(cases))
	for _, c := range cases {
		if _, ok := byID[c.id]; ok {
			return computedScore{}, fmt.Errorf("duplicate case %s", c.id)
		}
		if !isNormalTerminal(c.verdict) {
			return computedScore{}, fmt.Errorf("case %s is not a normal terminal verdict", c.id)
		}
		byID[c.id] = c
	}
	if len(byID) != len(cfg.Cases) {
		return computedScore{}, fmt.Errorf("result has %d cases, config has %d", len(byID), len(cfg.Cases))
	}
	for _, c := range cfg.Cases {
		if _, ok := byID[c.ID]; !ok {
			return computedScore{}, fmt.Errorf("missing case %s", c.ID)
		}
	}

	var total score
	subs := make([]subtaskOutcome, 0, len(cfg.Subtasks))
	allAC := true
	firstFail := verdictAccepted
	for _, st := range cfg.Subtasks {
		ok := true
		stFail := verdictAccepted
		for _, id := range st.CaseIDs {
			cr := byID[id]
			if cr.verdict != verdictAccepted {
				ok = false
				allAC = false
				if stFail == verdictAccepted {
					stFail = cr.verdict
				}
				if firstFail == verdictAccepted {
					firstFail = cr.verdict
				}
			}
		}
		sc := zeroScore
		ver := stFail
		if ok {
			sc = st.Score
			ver = verdictAccepted
		}
		total = total.add(sc)
		subs = append(subs, subtaskOutcome{id: st.ID, score: sc, verdict: ver})
	}
	v := verdictAccepted
	if !allAC {
		v = firstFail
	}
	return computedScore{total: total, verdict: v, subtasks: subs}, nil
}

func isNormalTerminal(v string) bool {
	switch v {
	case verdictAccepted, verdictWrongAnswer, verdictTLE, verdictMLE, verdictRE, verdictCompilationError:
		return true
	default:
		return false
	}
}
