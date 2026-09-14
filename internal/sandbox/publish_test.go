package sandbox

import (
	"strings"
	"testing"
)

// TestNewSubdomain pins the hostname shape: 128 bits of hex for the first
// published port, and the port number appended for a later one.
func TestNewSubdomain(t *testing.T) {
	first, err := newSubdomain(true, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || strings.Contains(first, "-") {
		t.Fatalf("first label %q, want 32 hex characters", first)
	}
	for i := 0; i < len(first); i++ {
		c := first[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			t.Fatalf("first label %q is not hex", first)
		}
	}
	later, err := newSubdomain(false, 8001)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(later, "-8001") || len(later) != 32+1+4 {
		t.Fatalf("later label %q, want <32 hex>-8001", later)
	}
	again, err := newSubdomain(true, 8000)
	if err != nil {
		t.Fatal(err)
	}
	if again == first {
		t.Fatalf("two draws both produced %q", first)
	}
}
