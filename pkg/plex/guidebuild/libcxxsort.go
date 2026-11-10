package guidebuild

// libcxxSort sorts a by less exactly as libc++'s std::sort did until LLVM
// 15 (its __sort in <algorithm>) for a value type that is not trivially
// copyable: sorting networks for 2 to 5 elements, insertion sort up to 6,
// above that a median-of-three quicksort whose partitions fall back to an
// incomplete insertion sort. It keeps no order among equal elements, and
// which of them comes first is the whole point: PMS 1.43 merges an item's
// genre lists with it (mergeTags), so the copy of a genre it keeps follows
// this algorithm's moves (155 of 155 movies on the kind rig, 2026-11-10;
// every newer libc++, which insertion-sorts up to 23 elements, kept the
// first copy of every genre and matched 54).
func libcxxSort[T any](a []T, less func(x, y T) bool) {
	s := sorter[T]{a: a, less: less}
	s.sort(0, len(a))
}

// libcxxLimit is __sort's insertion-sort limit for a value type that is not
// trivially copyable (30 for one that is).
const libcxxLimit = 6

type sorter[T any] struct {
	a    []T
	less func(x, y T) bool
}

func (s sorter[T]) lt(i, j int) bool { return s.less(s.a[i], s.a[j]) }
func (s sorter[T]) swap(i, j int)    { s.a[i], s.a[j] = s.a[j], s.a[i] }

// sort3 is __sort3: x, y, z sorted, returning the swaps made.
func (s sorter[T]) sort3(x, y, z int) int {
	if !s.lt(y, x) {
		if !s.lt(z, y) {
			return 0
		}
		s.swap(y, z)
		if s.lt(y, x) {
			s.swap(x, y)
			return 2
		}
		return 1
	}
	if s.lt(z, y) {
		s.swap(x, z)
		return 1
	}
	s.swap(x, y)
	if s.lt(z, y) {
		s.swap(y, z)
		return 2
	}
	return 1
}

// sort4 is __sort4.
func (s sorter[T]) sort4(x1, x2, x3, x4 int) int {
	r := s.sort3(x1, x2, x3)
	if s.lt(x4, x3) {
		s.swap(x3, x4)
		r++
		if s.lt(x3, x2) {
			s.swap(x2, x3)
			r++
			if s.lt(x2, x1) {
				s.swap(x1, x2)
				r++
			}
		}
	}
	return r
}

// sort5 is __sort5.
func (s sorter[T]) sort5(x1, x2, x3, x4, x5 int) int {
	r := s.sort4(x1, x2, x3, x4)
	if s.lt(x5, x4) {
		s.swap(x4, x5)
		r++
		if s.lt(x4, x3) {
			s.swap(x3, x4)
			r++
			if s.lt(x3, x2) {
				s.swap(x2, x3)
				r++
				if s.lt(x2, x1) {
					s.swap(x1, x2)
					r++
				}
			}
		}
	}
	return r
}

// insert moves a[i] down into the sorted run before it, as the insertion
// sorts' inner loop does: j is the last element of the run, and the element
// moves while it is less than the one before it.
func (s sorter[T]) insert(first, i, j int) {
	t := s.a[i]
	k := j
	j = i
	for {
		s.a[j] = s.a[k]
		j = k
		if j == first {
			break
		}
		k--
		if !s.less(t, s.a[k]) {
			break
		}
	}
	s.a[j] = t
}

// insertionSort3 is __insertion_sort_3.
func (s sorter[T]) insertionSort3(first, last int) {
	j := first + 2
	s.sort3(first, first+1, j)
	for i := j + 1; i != last; i++ {
		if s.lt(i, j) {
			s.insert(first, i, j)
		}
		j = i
	}
}

// insertionSortIncomplete is __insertion_sort_incomplete: it gives up after
// moving eight elements, reporting whether the range is sorted.
func (s sorter[T]) insertionSortIncomplete(first, last int) bool {
	switch last - first {
	case 0, 1:
		return true
	case 2:
		if s.lt(last-1, first) {
			s.swap(first, last-1)
		}
		return true
	case 3:
		s.sort3(first, first+1, last-1)
		return true
	case 4:
		s.sort4(first, first+1, first+2, last-1)
		return true
	case 5:
		s.sort5(first, first+1, first+2, first+3, last-1)
		return true
	}
	j := first + 2
	s.sort3(first, first+1, j)
	count := 0
	for i := j + 1; i != last; i++ {
		if s.lt(i, j) {
			s.insert(first, i, j)
			count++
			if count == 8 {
				return i+1 == last
			}
		}
		j = i
	}
	return true
}

// sort is __sort on [first, last).
func (s sorter[T]) sort(first, last int) {
	for {
		n := last - first
		switch n {
		case 0, 1:
			return
		case 2:
			if s.lt(last-1, first) {
				s.swap(first, last-1)
			}
			return
		case 3:
			s.sort3(first, first+1, last-1)
			return
		case 4:
			s.sort4(first, first+1, first+2, last-1)
			return
		case 5:
			s.sort5(first, first+1, first+2, first+3, last-1)
			return
		}
		if n <= libcxxLimit {
			s.insertionSort3(first, last)
			return
		}
		m, lm1 := first, last-1
		var swaps int
		if n >= 1000 {
			d := n / 2
			m += d
			d /= 2
			swaps = s.sort5(first, first+d, m, m+d, lm1)
		} else {
			m += n / 2
			swaps = s.sort3(first, m, lm1)
		}
		// Partition [first, m) < a[m] <= [m, last).
		i, j := first, lm1
		if !s.lt(i, m) {
			// a[first] == a[m]: guard the downward search by hand.
			restart := false
			for {
				j--
				if i == j {
					// a[first] == a[m] <= every other element: partition
					// into [first, i) == a[first] < [i, last) instead.
					i++
					j = last - 1
					if !s.lt(first, j) {
						for {
							if i == j {
								return // every element is equivalent
							}
							if s.lt(first, i) {
								s.swap(i, j)
								swaps++
								i++
								break
							}
							i++
						}
					}
					if i == j {
						return
					}
					for {
						for !s.lt(first, i) {
							i++
						}
						j--
						for s.lt(first, j) {
							j--
						}
						if i >= j {
							break
						}
						s.swap(i, j)
						swaps++
						i++
					}
					first = i
					restart = true
					break
				}
				if s.lt(j, m) {
					s.swap(i, j)
					swaps++
					break
				}
			}
			if restart {
				continue
			}
		}
		i++
		if i < j {
			for {
				for s.lt(i, m) {
					i++
				}
				j--
				for !s.lt(j, m) {
					j--
				}
				if i > j {
					break
				}
				s.swap(i, j)
				swaps++
				if m == i {
					m = j
				}
				i++
			}
		}
		if i != m && s.lt(m, i) {
			s.swap(i, m)
			swaps++
		}
		if swaps == 0 {
			fs := s.insertionSortIncomplete(first, i)
			if s.insertionSortIncomplete(i+1, last) {
				if fs {
					return
				}
				last = i
				continue
			}
			if fs {
				first = i + 1
				continue
			}
		}
		if i-first < last-i {
			s.sort(first, i)
			first = i + 1
		} else {
			s.sort(i+1, last)
			last = i
		}
	}
}
