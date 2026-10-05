package split

import (
	"regexp"
	"strconv"
	"strings"
)

// Attributes is the user-attribute map used by targeting rules.
type Attributes map[string]string

// RuleResult explains why an experiment did or did not match a user.
type RuleResult struct {
	Matched bool
	Reason  string
}

// compiledCondition is a validated Condition with its regex (if any)
// precompiled, so the allocation hot path performs no parsing work.
type compiledCondition struct {
	cond *Condition
	re   *regexp.Regexp
}

type compiledRule struct {
	name   string
	groups [][]*compiledCondition
}

func compileRules(expID string, in []TargetingRule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(in))
	for ri := range in {
		src := &in[ri]
		cr := compiledRule{name: src.Name}
		if cr.name == "" {
			cr.name = strconv.Itoa(ri)
		}
		for _, group := range src.Conditions {
			cg := make([]*compiledCondition, 0, len(group))
			for ci := range group {
				cc := &compiledCondition{cond: &group[ci]}
				if group[ci].Operator == OpRegex {
					re, err := regexp.Compile(group[ci].Values[0])
					if err != nil {
						return nil, err
					}
					cc.re = re
				}
				cg = append(cg, cc)
			}
			cr.groups = append(cr.groups, cg)
		}
		out = append(out, cr)
	}
	return out, nil
}

// matchCompiledRules evaluates OR-of-ANDs compiled targeting rules.
//
// Determinism and fallback semantics:
//   - Empty rule list => matched ("no targeting restriction").
//   - A missing attribute, a non-numeric value compared numerically,
//     or any malformed input makes the *predicate* fail: it never
//     panics and never fails allocation. The safe fallback for a user
//     that cannot be proven eligible is "not targeted".
//   - Reason is populated on every non-match so hit decisions stay
//     explainable.
func matchCompiledRules(rules []compiledRule, attrs Attributes) RuleResult {
	if len(rules) == 0 {
		return RuleResult{Matched: true, Reason: "no_targeting_rules"}
	}
	if attrs == nil {
		return RuleResult{Matched: false, Reason: "attributes_missing"}
	}
	for ri := range rules {
		r := &rules[ri]
		allGroupsMiss := true
		var firstWhy string
		for _, group := range r.groups {
			groupOK := true
			for _, cc := range group {
				ok, why := evalCondition(cc, attrs)
				if !ok {
					groupOK = false
					if firstWhy == "" {
						firstWhy = why
					}
					break
				}
			}
			if groupOK {
				allGroupsMiss = false
				break
			}
		}
		if !allGroupsMiss {
			return RuleResult{Matched: true, Reason: "matched_rule:" + r.name}
		}
		if ri == len(rules)-1 {
			return RuleResult{Matched: false, Reason: "rule:" + r.name + ":" + firstWhy}
		}
	}
	return RuleResult{Matched: false, Reason: "no_rule_matched"}
}

func evalCondition(c *compiledCondition, attrs Attributes) (bool, string) {
	cond := c.cond
	switch cond.Operator {
	case OpExists:
		if _, ok := attrs[cond.Attribute]; ok {
			return true, ""
		}
		return false, "missing:" + cond.Attribute
	case OpNotExists:
		if _, ok := attrs[cond.Attribute]; !ok {
			return true, ""
		}
		return false, "present:" + cond.Attribute
	}

	val, ok := attrs[cond.Attribute]
	if !ok {
		return false, "missing:" + cond.Attribute
	}

	switch cond.Operator {
	case OpEq:
		if val == cond.Values[0] {
			return true, ""
		}
		return false, "neq:" + cond.Attribute
	case OpNe:
		if val != cond.Values[0] {
			return true, ""
		}
		return false, "eq:" + cond.Attribute
	case OpIn:
		for _, want := range cond.Values {
			if val == want {
				return true, ""
			}
		}
		return false, "not_in:" + cond.Attribute
	case OpNotIn:
		for _, want := range cond.Values {
			if val == want {
				return false, "in:" + cond.Attribute
			}
		}
		return true, ""
	case OpPrefix:
		if strings.HasPrefix(val, cond.Values[0]) {
			return true, ""
		}
		return false, "prefix_mismatch:" + cond.Attribute
	case OpSuffix:
		if strings.HasSuffix(val, cond.Values[0]) {
			return true, ""
		}
		return false, "suffix_mismatch:" + cond.Attribute
	case OpContains:
		if strings.Contains(val, cond.Values[0]) {
			return true, ""
		}
		return false, "contains_mismatch:" + cond.Attribute
	case OpGT, OpGTE, OpLT, OpLTE:
		return compareNumericOrLex(cond, val)
	case OpRegex:
		if c.re == nil {
			return false, "bad_regex:" + cond.Attribute
		}
		if c.re.MatchString(val) {
			return true, ""
		}
		return false, "regex_mismatch:" + cond.Attribute
	case OpMod:
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return false, "non_numeric:" + cond.Attribute
		}
		if mod(n, cond.ModDivisor) == cond.ModRemainder {
			return true, ""
		}
		return false, "mod_mismatch:" + cond.Attribute
	default:
		return false, "unknown_operator:" + string(cond.Operator)
	}
}
func mod(n, d int64) int64 {
	r := n % d
	if r < 0 {
		r += d
	}
	return r
}

func compareNumericOrLex(cond *Condition, val string) (bool, string) {
	want := cond.Values[0]
	nf, nerr := strconv.ParseFloat(val, 64)
	wf, werr := strconv.ParseFloat(want, 64)
	if nerr != nil || werr != nil {
		// Ordered (numeric) comparisons require numeric operands. A
		// malformed value never errors and never guesses a match: the
		// predicate simply does not hold, so the user falls back to
		// "not targeted" deterministically.
		return false, "non_numeric:" + cond.Attribute
	}
	var cmp int
	switch {
	case nf < wf:
		cmp = -1
	case nf > wf:
		cmp = 1
	}
	ok := false
	switch cond.Operator {
	case OpGT:
		ok = cmp > 0
	case OpGTE:
		ok = cmp >= 0
	case OpLT:
		ok = cmp < 0
	case OpLTE:
		ok = cmp <= 0
	}
	if ok {
		return true, ""
	}
	return false, "compare_mismatch:" + cond.Attribute
}
