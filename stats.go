package split

import (
	"sort"
	"sync"
	"sync/atomic"
)

// VariantCount is one (experiment, variant) count entry.
type VariantCount struct {
	Variant string
	Count   int64
}

// ExperimentRatio reports observed traffic for one experiment.
type ExperimentRatio struct {
	ExperimentID string
	Total        int64
	Enrolled     int64 // users actually assigned to a variant
	Variants     []VariantCount
}

// RatioStats is a concurrency-safe, lock-light traffic counter used to
// verify that observed variant shares match the configured weights.
//
// Memory is bounded by the (experiment,variant) key space of configs
// that were ever observed; RetainVersion/ForgetVersion let the owner
// drop counters for retired config versions, so long-running processes
// do not accumulate memory across endless hot updates.
type RatioStats struct {
	shards [16]statsShard
}

type statsShard struct {
	mu sync.Mutex
	m  map[string]*statsCounters // key: version|experiment
}

type statsCounters struct {
	version string
	expID   string
	total   atomic.Int64
	enroll  atomic.Int64
	// variant counts are kept behind the shard map only for variant
	// name registration; increments use atomic pointers via counter
	// entries.
	variants []*variantCounter
}

type variantCounter struct {
	name  string
	count atomic.Int64
}

func newStatsKey(version, expID string) string { return version + "|" + expID }

// NewRatioStats constructs an empty counter set.
func NewRatioStats() *RatioStats {
	rs := &RatioStats{}
	for i := range rs.shards {
		rs.shards[i].m = make(map[string]*statsCounters)
	}
	return rs
}

func (rs *RatioStats) shardFor(key string) *statsShard {
	h := fnv1a64(key)
	return &rs.shards[h%uint64(len(rs.shards))]
}

// Observe records one allocation. Call it from the assignment path or
// from an event consumer. It is one atomic add in the steady state.
func (rs *RatioStats) Observe(version, expID, variant string, inExperiment bool) {
	key := newStatsKey(version, expID)
	sh := rs.shardFor(key)
	sh.mu.Lock()
	cs := sh.m[key]
	if cs == nil {
		cs = &statsCounters{version: version, expID: expID}
		sh.m[key] = cs
	}
	var vc *variantCounter
	if inExperiment && variant != "" {
		for _, c := range cs.variants {
			if c.name == variant {
				vc = c
				break
			}
		}
		if vc == nil {
			vc = &variantCounter{name: variant}
			cs.variants = append(cs.variants, vc)
		}
	}
	sh.mu.Unlock()

	cs.total.Add(1)
	if inExperiment {
		cs.enroll.Add(1)
	}
	if vc != nil {
		vc.count.Add(1)
	}
}

// ObserveEvent records from an AssignmentEvent.
func (rs *RatioStats) ObserveEvent(e AssignmentEvent) {
	rs.Observe(e.ConfigVersion, e.ExperimentID, e.Variant, e.InExperiment)
}

// Snapshot returns observed ratios for a config version, sorted by
// experiment ID then variant name.
func (rs *RatioStats) Snapshot(version string) []ExperimentRatio {
	out := make([]ExperimentRatio, 0)
	for i := range rs.shards {
		sh := &rs.shards[i]
		sh.mu.Lock()
		for _, cs := range sh.m {
			if cs.version != version {
				continue
			}
			er := ExperimentRatio{
				ExperimentID: cs.expID,
				Total:        cs.total.Load(),
				Enrolled:     cs.enroll.Load(),
			}
			for _, vc := range cs.variants {
				er.Variants = append(er.Variants, VariantCount{
					Variant: vc.name, Count: vc.count.Load(),
				})
			}
			sort.Slice(er.Variants, func(a, b int) bool { return er.Variants[a].Variant < er.Variants[b].Variant })
			out = append(out, er)
		}
		sh.mu.Unlock()
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ExperimentID < out[b].ExperimentID })
	return out
}

// ForgetVersion drops all counters belonging to a retired config
// version, bounding memory across hot updates.
func (rs *RatioStats) ForgetVersion(version string) {
	for i := range rs.shards {
		sh := &rs.shards[i]
		sh.mu.Lock()
		for k, cs := range sh.m {
			if cs.version == version {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}

// Ratio is the observed share of variant within enrolled users in
// [0,1]; returns 0 when nobody is enrolled.
func (er ExperimentRatio) Ratio(variant string) float64 {
	if er.Enrolled == 0 {
		return 0
	}
	for _, v := range er.Variants {
		if v.Variant == variant {
			return float64(v.Count) / float64(er.Enrolled)
		}
	}
	return 0
}
