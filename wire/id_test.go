package wire

import (
	"regexp"
	"testing"
)

func TestRandomHex(t *testing.T) {
	a, b := RandomHex(4), RandomHex(4)
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(a) || a == b {
		t.Fatal(a, b)
	}
}
