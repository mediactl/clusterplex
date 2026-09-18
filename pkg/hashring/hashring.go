// Package hashring assigns keys to members consistently.
//
// Two things need this, and for the same reason. Background maintenance has to
// be handed to exactly one pod per item, or every pod analyses the same media
// at once. Client sessions have to keep landing on the same pod, because Plex
// caches state in memory and there is no bus to invalidate it.
//
// Consistent hashing rather than a modulo because pods come and go: losing one
// should move only the keys it owned, not reshuffle everything.
package hashring

import (
	"hash/crc32"
	"slices"
	"sort"
	"strconv"
)

// replicas is how many points on the ring each member occupies. More points
// spread keys more evenly at the cost of a larger ring; 160 is the usual
// default and gives a good spread for the handful of pods we expect.
const replicas = 160

// Ring maps keys to members. It is immutable once built, so it is safe to read
// from several goroutines and cheap to rebuild when membership changes.
type Ring struct {
	members []string
	points  []uint32
	owner   map[uint32]string
}

// New builds a ring over members. Duplicates and empty names are ignored, and
// the result does not depend on the order members are given: every pod builds
// its own ring and they must agree.
func New(members ...string) *Ring {
	r := &Ring{owner: map[uint32]string{}}
	for _, m := range members {
		if m == "" || slices.Contains(r.members, m) {
			continue
		}
		r.members = append(r.members, m)
	}
	sort.Strings(r.members)

	for _, m := range r.members {
		for i := range replicas {
			p := point(m + "#" + strconv.Itoa(i))
			if _, taken := r.owner[p]; taken {
				// Two members landing on one point would make the ring depend
				// on insertion order. Sorted members make this deterministic.
				continue
			}
			r.owner[p] = m
			r.points = append(r.points, p)
		}
	}
	slices.Sort(r.points)
	return r
}

// Locate returns the member that owns key, or "" when the ring is empty.
func (r *Ring) Locate(key string) string {
	if len(r.points) == 0 {
		return ""
	}
	p := point(key)
	// The first point at or after the key, wrapping around the ring.
	i, _ := slices.BinarySearch(r.points, p)
	if i == len(r.points) {
		i = 0
	}
	return r.owner[r.points[i]]
}

// Members returns the ring's members, sorted.
func (r *Ring) Members() []string { return slices.Clone(r.members) }

func point(s string) uint32 { return crc32.ChecksumIEEE([]byte(s)) }
