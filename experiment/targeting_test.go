package experiment

import "testing"

func mustRule(t *testing.T, r *RuleConfig) rule {
	t.Helper()
	c, err := compileRule(r)
	if err != nil {
		t.Fatalf("compileRule: %v", err)
	}
	return c
}

func TestNilRuleMatchesAll(t *testing.T) {
	r, err := compileRule(nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.match(nil); !ok {
		t.Fatal("nil rule must match even nil attributes")
	}
}

func TestLeafOperators(t *testing.T) {
	attrs := map[string]Value{
		"city": "beijing",
		"age":  25,
		"vip":  true,
	}
	cases := []struct {
		name string
		cond ConditionConfig
		want bool
	}{
		{"eq hit", ConditionConfig{"city", "eq", []Value{"beijing"}}, true},
		{"eq miss", ConditionConfig{"city", "eq", []Value{"shanghai"}}, false},
		{"ne hit", ConditionConfig{"city", "ne", []Value{"shanghai"}}, true},
		{"in hit", ConditionConfig{"city", "in", []Value{"beijing", "x"}}, true},
		{"in miss", ConditionConfig{"city", "in", []Value{"x", "y"}}, false},
		{"not_in hit", ConditionConfig{"city", "not_in", []Value{"x"}}, true},
		{"gt num", ConditionConfig{"age", "gt", []Value{18}}, true},
		{"gte eq", ConditionConfig{"age", "gte", []Value{25}}, true},
		{"lt miss", ConditionConfig{"age", "lt", []Value{25}}, false},
		{"lte hit", ConditionConfig{"age", "lte", []Value{25}}, true},
		{"exists hit", ConditionConfig{"vip", "exists", nil}, true},
		{"exists miss", ConditionConfig{"unknown", "exists", nil}, false},
		{"not_exists hit", ConditionConfig{"unknown", "not_exists", nil}, true},
		{"bool eq", ConditionConfig{"vip", "eq", []Value{true}}, true},
		{"prefix hit", ConditionConfig{"city", "prefix", []Value{"bei"}}, true},
		{"prefix miss", ConditionConfig{"city", "prefix", []Value{"sh"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := compileLeaf(&c.cond)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := r.match(attrs)
			if got != c.want {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		})
	}
}

func TestMissingAttributeNeverErrors(t *testing.T) {
	r := mustRule(t, &RuleConfig{Where: &ConditionConfig{
		Attribute: "missing", Operator: "eq", Values: []Value{"x"},
	}})
	ok, why := r.match(map[string]Value{})
	if ok {
		t.Fatal("missing attribute must not match")
	}
	if why == "" {
		t.Fatal("expected an explanation for the miss")
	}
	// nil map too.
	if ok2, _ := r.match(nil); ok2 {
		t.Fatal("nil map must not match")
	}
}

func TestTypeAnomalyIsDeterministicFalse(t *testing.T) {
	r := mustRule(t, &RuleConfig{Where: &ConditionConfig{
		Attribute: "city", Operator: "gt", Values: []Value{18},
	}})
	// city is a string while gt expects a number -> false + explanation, no panic.
	ok, why := r.match(map[string]Value{"city": "beijing"})
	if ok {
		t.Fatal("type mismatch must resolve to false")
	}
	if why == "" {
		t.Fatal("expected explanation")
	}
}

func TestAndOrComposition(t *testing.T) {
	attrs := map[string]Value{"city": "beijing", "age": 25}
	andR := mustRule(t, &RuleConfig{Op: "and", All: []RuleConfig{
		{Where: &ConditionConfig{"city", "eq", []Value{"beijing"}}},
		{Where: &ConditionConfig{"age", "gte", []Value{18}}},
	}})
	if ok, _ := andR.match(attrs); !ok {
		t.Fatal("and must hit")
	}
	if ok, _ := andR.match(map[string]Value{"city": "beijing", "age": 5}); ok {
		t.Fatal("and must miss when one child fails")
	}

	orR := mustRule(t, &RuleConfig{Op: "or", Any: []RuleConfig{
		{Where: &ConditionConfig{"city", "eq", []Value{"shanghai"}}},
		{Where: &ConditionConfig{"age", "gte", []Value{18}}},
	}})
	if ok, _ := orR.match(attrs); !ok {
		t.Fatal("or must hit via age")
	}

	if _, err := compileRule(&RuleConfig{Op: "xor"}); err == nil {
		t.Fatal("unknown operator must fail compilation")
	}
}
