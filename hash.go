package split

import (
	"math"
)

const fnvOffset64 = uint64(14695981039346656037)
const fnvPrime64 = uint64(1099511628211)

// fnv1a64 computes FNV-1a (64 bit). Reimplemented explicitly instead
// of using hash/fnv so the output is contractually stable: the algorithm
// constants are part of the bucketing semantics and must never change,
// otherwise existing users would silently drift to new buckets.
func fnv1a64(key string) uint64 {
	h := fnvOffset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= fnvPrime64
	}
	return h
}

// hashUint64 returns a deterministic 64-bit hash of key.
func hashUint64(key string) uint64 {
	return fnv1a64(key)
}

// hash100 returns a deterministic bucket index in [0,100) for key.
func hash100(key string) uint32 {
	return uint32(hashUint64(key) % 100)
}

// unitFloat maps a 64-bit hash to a uniform float in (0,1).
//
// It uses the LOW 53 bits on purpose: with FNV-1a, material placed at
// the END of the hashed key mixes strongly into the low bits (early
// bytes mix into the high bits). All key construction in this package
// therefore puts the high-entropy identifier (user id) at the end.
func unitFloat(h uint64) float64 {
	const mask53 = uint64(1<<53) - 1
	const denom = float64(1 << 53)
	v := float64(h&mask53) / denom
	if v <= 0 {
		v = 1.0 / denom
	} else if v >= 1 {
		v = 1.0 - 1.0/denom
	}
	return v
}

// weightedPick deterministically selects a variant for a user using
// weighted rendezvous hashing (highest random weight):
//
//	score_i = u_i^(1/weight_i),  u_i = hash(exp|salt|variant|user) in (0,1)
//
// Key layout matters for FNV mixing: the user id (highest entropy and
// the only per-request variable) is placed LAST, where it drives the
// low bits used by unitFloat; the variant name sits before it, so each
// (variant,user) pair yields an effectively independent u_i.
//
// Properties:
//   - Assignment probability of variant i == weight_i / sum(weights).
//   - Adding/removing/resizing one variant only moves users whose
//     maximum score actually changes owner => existing users do not
//     drift when weights merely shift around them.
//   - Pure function of its inputs: stable across processes and time.
//
// userKey must have the form "exp|salt|"; the user id is concatenated
// last. Returns "" when no variant has positive weight.
func weightedPick(userKey string, userID string, variants []Variant) string {
	best := ""
	bestIdx := -1
	var bestScore float64
	for i := range variants {
		v := &variants[i]
		if v.Weight <= 0 {
			continue
		}
		h := hashUint64(userKey + "v=" + v.Name + "|" + userID)
		u := unitFloat(h)
		score := math.Pow(u, 1.0/float64(v.Weight))
		// Strictly greater keeps the earliest variant on an exact tie,
		// giving deterministic selection.
		if bestIdx == -1 || score > bestScore {
			bestScore = score
			best = v.Name
			bestIdx = i
		}
	}
	return best
}
