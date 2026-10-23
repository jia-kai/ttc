package messagestream

import "testing"

func TestHashPreservesExactBytes(t *testing.T) {
	if Hash([]byte("x")) == Hash([]byte(" x ")) {
		t.Fatal("hash normalized bytes")
	}
}
