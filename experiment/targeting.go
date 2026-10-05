package experiment

import (
	"fmt"
	"strings"
)

// rule is the compiled targeting rule tree.
type rule interface {
	// match must never panic: missing attributes and value type mismatches
	// resolve to a deterministic false with a human-readable explanation.
	match(attrs map[string]Value) (bool, string)
}

// ---- match-all (nil rule) -------------------------------------------------

type matchAll struct{}

func (matchAll) match(map[string]Value) (bool, string) {
	return true, "no targeting rules"
}

// ---- logical nodes --------------------------------------------------------

type andNode struct{ children []rule }

func (n andNode) match(attrs map[string]Value) (bool, string) {
	if len(n.children) == 0 {
		return true, "empty and-group"
	}
	var reasons []string
	for _, c := range n.children {
		ok, why := c.match(attrs)
		if !ok {
			reasons = append(reasons, why)
		}
	}
	if len(reasons) == 0 {
		return true, "all conditions matched"
	}
	return false, "and-group failed: " + strings.Join(reasons, "; ")
}

type orNode struct{ children []rule }

func (n orNode) match(attrs map[string]Value) (bool, string) {
	if len(n.children) == 0 {
		return false, "empty or-group"
	}
	var reasons []string
	for _, c := range n.children {
		if ok, why := c.match(attrs); ok {
			return true, why
		} else {
			reasons = append(reasons, why)
		}
	}
	return false, "or-group failed: " + strings.Join(reasons, "; ")
}

// ---- leaf condition -------------------------------------------------------

type leafRule struct {
	attr string
	op   string
	vals []Value
}

func (l leafRule) match(attrs map[string]Value) (bool, string) {
	raw, present := attrs[l.attr] // nil map read is safe in Go

	switch l.op {
	case opExists:
		if present {
			return true, fmt.Sprintf("%s exists", l.attr)
		}
		return false, fmt.Sprintf("%s missing", l.attr)
	case opNotExists:
		if !present {
			return true, fmt.Sprintf("%s not present", l.attr)
		}
		return false, fmt.Sprintf("%s present", l.attr)
	}

	if !present {
		return false, fmt.Sprintf("%s: attribute missing", l.attr)
	}

	switch l.op {
	case opEq, opNe, opIn, opNotIn:
		hit := false
		var got string
		for _, want := range l.vals {
			eq, comparable := looseEqual(raw, want)
			if !comparable {
				got = fmt.Sprintf("%s: type mismatch (%s vs %s)", l.attr, typeName(raw), typeName(want))
				continue
			}
			if eq {
				hit = true
				break
			}
		}
		switch l.op {
		case opEq:
			if hit {
				return true, fmt.Sprintf("%s == %s", l.attr, formatVal(l.vals[0]))
			}
			if got != "" {
				return false, got
			}
			return false, fmt.Sprintf("%s != %s (got %s)", l.attr, formatVal(l.vals[0]), formatVal(raw))
		case opNe:
			if !hit {
				return true, fmt.Sprintf("%s != %s", l.attr, formatVal(l.vals[0]))
			}
			return false, fmt.Sprintf("%s == %s", l.attr, formatVal(raw))
		case opIn:
			if hit {
				return true, fmt.Sprintf("%s in set", l.attr)
			}
			if got != "" {
				return false, got
			}
			return false, fmt.Sprintf("%s=%s not in set", l.attr, formatVal(raw))
		default: // not_in
			if !hit {
				return true, fmt.Sprintf("%s not in set", l.attr)
			}
			return false, fmt.Sprintf("%s=%s in set", l.attr, formatVal(raw))
		}

	case opPrefix:
		s, ok := raw.(string)
		if !ok {
			return false, fmt.Sprintf("%s: prefix requires string, got %s", l.attr, typeName(raw))
		}
		if len(l.vals) == 0 {
			return false, fmt.Sprintf("%s: prefix has no operand", l.attr)
		}
		want, ok := l.vals[0].(string)
		if !ok {
			return false, fmt.Sprintf("%s: prefix operand must be string", l.attr)
		}
		if strings.HasPrefix(s, want) {
			return true, fmt.Sprintf("%s has prefix %q", l.attr, want)
		}
		return false, fmt.Sprintf("%s=%q lacks prefix %q", l.attr, s, want)

	case opGt, opGte, opLt, opLte:
		x, xok := toFloat(raw)
		if !xok {
			return false, fmt.Sprintf("%s: operator %s requires number, got %s", l.attr, l.op, typeName(raw))
		}
		if len(l.vals) == 0 {
			return false, fmt.Sprintf("%s: %s has no operand", l.attr, l.op)
		}
		y, yok := toFloat(l.vals[0])
		if !yok {
			return false, fmt.Sprintf("%s: %s operand must be number", l.attr, l.op)
		}
		cmp := false
		switch l.op {
		case opGt:
			cmp = x > y
		case opGte:
			cmp = x >= y
		case opLt:
			cmp = x < y
		case opLte:
			cmp = x <= y
		}
		if cmp {
			return true, fmt.Sprintf("%s %s %v", l.attr, l.op, y)
		}
		return false, fmt.Sprintf("%s=%v not %s %v", l.attr, x, l.op, y)
	}

	return false, fmt.Sprintf("%s: unsupported operator %q", l.attr, l.op)
}

// ---- operators ------------------------------------------------------------

const (
	opEq        = "eq"
	opNe        = "ne"
	opIn        = "in"
	opNotIn     = "not_in"
	opGt        = "gt"
	opGte       = "gte"
	opLt        = "lt"
	opLte       = "lte"
	opExists    = "exists"
	opNotExists = "not_exists"
	opPrefix    = "prefix"
)

var validOps = map[string]bool{
	opEq: true, opNe: true, opIn: true, opNotIn: true,
	opGt: true, opGte: true, opLt: true, opLte: true,
	opExists: true, opNotExists: true, opPrefix: true,
}

// compileRule compiles a rule configuration. A nil rule matches every user.
// It never returns a nil rule: callers always get a usable matcher.
func compileRule(r *RuleConfig) (rule, error) {
	if r == nil {
		return matchAll{}, nil
	}
	if r.Where != nil {
		return compileLeaf(r.Where)
	}

	var raw []RuleConfig
	op := strings.ToLower(strings.TrimSpace(r.Op))
	if op == "or" {
		raw = r.Any
	} else {
		raw = r.All
		if op != "" && op != "and" {
			return nil, fmt.Errorf("unknown logical operator %q (want and/or)", r.Op)
		}
	}
	children := make([]rule, 0, len(raw))
	for i := range raw {
		c, err := compileRule(&raw[i])
		if err != nil {
			return nil, err
		}
		children = append(children, c)
	}
	if op == "or" {
		return orNode{children: children}, nil
	}
	return andNode{children: children}, nil
}

func compileLeaf(c *ConditionConfig) (rule, error) {
	if c.Attribute == "" {
		return nil, fmt.Errorf("condition attribute must not be empty")
	}
	op := strings.ToLower(strings.TrimSpace(c.Operator))
	if !validOps[op] {
		return nil, fmt.Errorf("condition on %q: unknown operator %q", c.Attribute, c.Operator)
	}
	switch op {
	case opExists, opNotExists:
		// no operands needed
	case opEq, opNe, opGt, opGte, opLt, opLte, opPrefix:
		if len(c.Values) == 0 {
			return nil, fmt.Errorf("condition on %q: operator %s requires one value", c.Attribute, op)
		}
	default:
		if len(c.Values) == 0 {
			return nil, fmt.Errorf("condition on %q: operator %s requires values", c.Attribute, op)
		}
	}
	return leafRule{attr: c.Attribute, op: op, vals: c.Values}, nil
}

// ---- value coercion -------------------------------------------------------

func toFloat(v Value) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// looseEqual compares two attribute values. The second result is false when
// the types are not comparable (rather than panicking).
func looseEqual(a, b Value) (bool, bool) {
	if x, ok := toFloat(a); ok {
		if y, ok := toFloat(b); ok {
			return x == y, true
		}
		return false, false
	}
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return sa == sb, true
		}
		return false, false
	}
	if ba, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			return ba == bb, true
		}
		return false, false
	}
	return false, false
}

func typeName(v Value) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		if _, ok := toFloat(v); ok {
			return "number"
		}
		return fmt.Sprintf("%T", v)
	}
}

func formatVal(v Value) string {
	switch t := v.(type) {
	case string:
		return strconvQuote(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	default:
		if f, ok := toFloat(v); ok {
			return trimFloat(f)
		}
		return fmt.Sprintf("%v", v)
	}
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

func trimFloat(f float64) string { return fmt.Sprintf("%g", f) }
