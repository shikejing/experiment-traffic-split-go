package split_test

import (
	"context"
	"fmt"
	"log"

	split "github.com/shikejing/experiment-traffic-split-go"
)

func Example() {
	cfg := &split.Config{
		Version: "2026-10-05",
		Experiments: []split.Experiment{{
			ID:             "checkout-redesign",
			TrafficPercent: 50,
			Variants: []split.Variant{
				{Name: "control", Weight: 1},
				{Name: "redesign", Weight: 1},
			},
			Rules: []split.TargetingRule{{
				Name: "ios_and_android",
				Conditions: [][]split.Condition{
					{{Attribute: "os", Operator: split.OpIn, Values: []string{"ios", "android"}}},
				},
			}},
		}},
	}
	s, err := split.New(cfg, split.Options{})
	if err != nil {
		log.Fatal(err)
	}
	al, err := s.Allocate(context.Background(), "checkout-redesign", "user-42",
		split.Attributes{"os": "ios"})
	if err != nil {
		log.Fatal(err)
	}
	if al.InExperiment {
		fmt.Println("variant:", al.Variant)
	} else {
		fmt.Println("not enrolled:", al.Reason)
	}
}
