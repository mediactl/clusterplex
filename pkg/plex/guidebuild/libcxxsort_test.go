package guidebuild

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLibcxxSortSortsEveryLength(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	type e struct{ k, i int }
	for range 5000 {
		n, keys := r.IntN(80), 1+r.IntN(20)
		a := make([]e, n)
		for i := range a {
			a[i] = e{r.IntN(keys), i}
		}
		libcxxSort(a, func(x, y e) bool { return x.k < y.k })
		require.True(t, slices.IsSortedFunc(a, func(x, y e) int { return x.k - y.k }), "%v", a)
		seen := make([]bool, n)
		for _, x := range a {
			require.False(t, seen[x.i], "an element twice")
			seen[x.i] = true
		}
	}
}

func TestLibcxxSortMovesEqualElementsAsLibcxxDid(t *testing.T) {
	// Ten elements, five pairs of equals: the partition sends the second
	// copies of some ahead of their firsts. The order is the one libc++'s
	// __sort leaves (checked against an emulation that kept the copies
	// PMS kept for all 155 movies on the rig).
	type e struct {
		k string
		i int
	}
	names := []string{"Action", "Adventure", "Fantasy", "Movies", "Adrenaline + Sci-Fi"}
	var a []e
	for _, l := range [][]string{names, names} {
		for _, n := range l {
			a = append(a, e{n, len(a)})
		}
	}
	libcxxSort(a, func(x, y e) bool { return x.k < y.k })
	got := make([]int, len(a))
	for i, x := range a {
		got[i] = x.i
	}
	assert.Equal(t, []int{0, 5, 9, 4, 6, 1, 7, 2, 3, 8}, got)
}
