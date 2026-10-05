package experiment

import "hash/fnv"

// hashUser deterministically maps (salt, namespace, userID) to a value in
// [0, Scale). It never panics on empty input and is stable across processes
// and versions: it uses FNV-1a over a length-prefixed canonical encoding so
// distinct components cannot collide via boundary ambiguity.
//
// Stability guarantees:
//   - Same inputs -> same bucket, forever, regardless of process or host.
//   - Changing OTHER experiments/variants does not change this bucket,
//     because the namespace is scoped to the experiment (and optional
//     rebucket salt).
func hashUser(salt, namespace, userID string) uint32 {
	h := fnv.New64a()
	writeLenPrefixed := func(s string) {
		var buf [8]byte
		n := len(s)
		i := 0
		for n >= 0x80 {
			buf[i] = byte(n) | 0x80
			n >>= 7
			i++
		}
		buf[i] = byte(n)
		_, _ = h.Write(buf[:i+1])
		_, _ = h.Write([]byte(s))
	}
	writeLenPrefixed(salt)
	writeLenPrefixed(namespace)
	writeLenPrefixed(userID)
	return uint32(h.Sum64() % uint64(Scale))
}

// hashPercent hashes a gate key into [0,100). It is used for percentage
// rollout gating so that "is user in rollout" is itself stable per user (and
// independent of the variant bucket).
func hashPercent(key string) uint32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return uint32(h.Sum64() % 100)
}
