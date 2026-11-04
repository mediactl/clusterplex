package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWindowReadsUTCHoursAndRefusesNonsense(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [2]int
	}{
		{"8-11", [2]int{8, 11}},
		{" 23 - 2 ", [2]int{23, 2}},
		{"", [2]int{}},
		{"none", [2]int{}},
		{"OFF", [2]int{}},
	} {
		got, err := parseWindow(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	for _, in := range []string{"8", "8-24", "-1-5", "8-8", "a-b", "8-11-12"} {
		_, err := parseWindow(in)
		assert.Error(t, err, in)
	}
}
