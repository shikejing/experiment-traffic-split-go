package experiment

import "errors"

// VariantRange is one variant's half-open interval [From, To) over Scale.
type VariantRange struct {
	Key  string
	From uint32
	To   uint32
}

var (
	errNoVariants       = errors.New("experiment has no variants")
	errNegativeWeight   = errors.New("variant weight must be >= 0")
	errEmptyVariantKey  = errors.New("variant key must not be empty")
	errZeroTotalWeight  = errors.New("sum of variant weights must be > 0")
	errDuplicateVariant = errors.New("duplicate variant key")
)

// buildRanges converts relative weights into cumulative half-open ranges.
//
// Ranges are built in declared order using cumulative (not per-variant)
// rounding: end_i = floor(Scale * sum(weight_0..weight_i) / total). This gives
// two stability properties when proportions are adjusted:
//
//  1. Growing or shrinking the LAST variant only moves users at the boundary;
//     users hashed into earlier, unchanged variants stay put.
//  2. Inserting weight at the end never moves any existing user.
//
// In general, moving a boundary can only move users that cross that boundary;
// hashes are not re-mapped. The final interval end is pinned to Scale so the
// last variant absorbs at most one unit of rounding error and coverage is
// exact.
func buildRanges(variants []VariantConfig) ([]VariantRange, int, error) {
	if len(variants) == 0 {
		return nil, 0, errNoVariants
	}
	total := 0
	seen := make(map[string]struct{}, len(variants))
	for _, v := range variants {
		if v.Key == "" {
			return nil, 0, errEmptyVariantKey
		}
		if v.Weight < 0 {
			return nil, 0, errNegativeWeight
		}
		if _, dup := seen[v.Key]; dup {
			return nil, 0, errDuplicateVariant
		}
		seen[v.Key] = struct{}{}
		total += v.Weight
	}
	if total == 0 {
		return nil, 0, errZeroTotalWeight
	}

	ranges := make([]VariantRange, len(variants))
	var cum int
	var from uint32
	for i, v := range variants {
		cum += v.Weight
		var to uint32
		if i == len(variants)-1 {
			to = Scale // pin the final boundary; exact coverage
		} else {
			to = uint32(uint64(Scale) * uint64(cum) / uint64(total))
		}
		ranges[i] = VariantRange{Key: v.Key, From: from, To: to}
		from = to
	}
	return ranges, total, nil
}

// pickVariant returns the variant key whose range contains bucket, or "" when
// no range contains it (only possible for a malformed table).
func pickVariant(ranges []VariantRange, bucket uint32) string {
	// Linear scan; variant counts are tiny (typically 2-5). A loop keeps the
	// mapping trivially auditable.
	for _, r := range ranges {
		if bucket >= r.From && bucket < r.To {
			return r.Key
		}
	}
	return ""
}
