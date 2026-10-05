// Command demo exercises the experiment package as an embedded component:
// config loading, stable bucketing, targeting, mutually exclusive layers,
// hot reload and gray rollout, plus sampled async event recording.
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/shikejing/experiment-traffic-split-go/experiment"
)

const cfgV1 = `{
  "version": "v1",
  "experiments": [
    {
      "key": "checkout-button",
      "enabled": true,
      "variants": [
        {"key": "control", "weight": 50},
        {"key": "green",   "weight": 50}
      ]
    },
    {
      "key": "home-promo",
      "enabled": true,
      "layer": "homepage",
      "fallback": "control",
      "variants": [
        {"key": "control", "weight": 90},
        {"key": "promo-a", "weight": 10}
      ]
    },
    {
      "key": "home-banner",
      "enabled": true,
      "layer": "homepage",
      "variants": [
        {"key": "control", "weight": 90},
        {"key": "banner-b", "weight": 10}
      ]
    },
    {
      "key": "vip-pricing",
      "enabled": true,
      "fallback": "standard",
      "variants": [
        {"key": "standard", "weight": 50},
        {"key": "discount", "weight": 50}
      ],
      "rules": {"where": {"attribute": "vip", "operator": "eq", "values": [true]}}
    }
  ],
  "layers": [
    {"key": "homepage", "traffic": 100, "experiments": ["home-promo", "home-banner"]}
  ]
}`

type stdoutSink struct{ n int }

func (s *stdoutSink) Write(evs []experiment.AssignmentEvent) error {
	for _, ev := range evs {
		if ev.InExperiment {
			s.n++
			if s.n <= 5 {
				b, _ := json.Marshal(ev)
				fmt.Printf("event: %s\n", b)
			}
		}
	}
	return nil
}

func main() {
	cfg, err := experiment.ParseConfig([]byte(cfgV1))
	if err != nil {
		log.Fatalf("parse: %v", err)
	}

	mgr, err := experiment.NewManager(cfg, experiment.ManagerOptions{
		Sink:           &stdoutSink{},
		EventSampling:  0.5,
		EventQueueSize: 1024,
		DropPolicy:     experiment.DropNewest,
	})
	if err != nil {
		log.Fatalf("manager: %v", err)
	}
	defer mgr.Close()

	// 1) Stability: the same user always lands on the same variant.
	u := experiment.User{ID: "user-1001", Attributes: map[string]experiment.Value{"vip": true}}
	first := mgr.Variant(u, "checkout-button")
	for i := 0; i < 3; i++ {
		if mgr.Variant(u, "checkout-button") != first {
			log.Fatal("assignment drifted")
		}
	}
	fmt.Printf("user-1001 -> checkout-button=%s (stable)\n", first)

	// 2) Targeting with fallback and explainable miss.
	plain := experiment.User{ID: "user-2002"} // no attributes at all: no panic
	d := mgr.Evaluate(plain)
	fmt.Printf("non-vip user-2002 -> vip-pricing variant=%q reason=%q\n",
		d.Assignments["vip-pricing"].Variant, d.Assignments["vip-pricing"].Reason)

	// 3) Mutual exclusion: one user hits at most one homepage experiment.
	both := 0
	for i := 0; i < 5000; i++ {
		dd := mgr.Evaluate(experiment.User{ID: fmt.Sprintf("u-%d", i)})
		if dd.Assignments["home-promo"].InExperiment &&
			dd.Assignments["home-banner"].InExperiment {
			both++
		}
	}
	fmt.Printf("users hitting both homepage experiments: %d (must be 0)\n", both)

	// 4) Gray rollout: stage v2 at 10%, then observe the routed share.
	cfgV2 := *cfg
	cfgV2.Version = "v2"
	cfgV2.Experiments[0].Variants = []experiment.VariantConfig{
		{Key: "control", Weight: 20}, {Key: "green", Weight: 80},
	}
	if err := mgr.StageRollout(&cfgV2); err != nil {
		log.Fatalf("stage: %v", err)
	}
	if err := mgr.SetRollout("v2", 10); err != nil {
		log.Fatalf("rollout: %v", err)
	}
	v2 := 0
	for i := 0; i < 5000; i++ {
		if mgr.Assignment(experiment.User{ID: fmt.Sprintf("r-%d", i)}, "checkout-button").ConfigFrom == "v2" {
			v2++
		}
	}
	fmt.Printf("routed to v2: %.1f%% of users (target 10%%)\n", float64(v2)/50)

	// 5) Rollback candidate and show audit history.
	if err := mgr.Rollback("v2"); err != nil {
		log.Fatalf("rollback: %v", err)
	}
	snap := mgr.Snapshot()
	fmt.Printf("after rollback: stable=%s candidate=%q history=%d entries\n",
		snap.StableVersion, snap.CandidateVersion, len(snap.History))
}
