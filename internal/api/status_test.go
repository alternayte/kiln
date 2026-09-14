package api

import "testing"

// TestHumanBytes pins the unit prefixes. An off-by-one in the prefix table
// renders "1 iiB" for a mebibyte.
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1 KiB"},
		{1 << 20, "1 MiB"},
		{1 << 30, "1 GiB"},
		{1 << 40, "1 TiB"},
		{3 << 30, "3 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.n); got != tc.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
