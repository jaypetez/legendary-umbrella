package signaling

import (
	"strings"
	"testing"
)

func TestRandomUserCode(t *testing.T) {
	const iterations = 500
	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		c := randomUserCode()
		if len(c) != 9 {
			t.Fatalf("user code %q: expected len 9, got %d", c, len(c))
		}
		if c[4] != '-' {
			t.Fatalf("user code %q: expected '-' at index 4", c)
		}
		for idx, r := range c {
			if idx == 4 {
				continue
			}
			if !strings.ContainsRune(crockfordAlphabet, r) {
				t.Fatalf("user code %q contains non-Crockford rune %q", c, r)
			}
		}
		if _, dup := seen[c]; dup {
			// 32^8 keyspace >> 500, a repeat means the RNG is broken.
			t.Fatalf("duplicate user code after %d iterations: %q", i, c)
		}
		seen[c] = struct{}{}
	}
}

func TestRandomToken(t *testing.T) {
	for _, n := range []int{8, 16, 32, 64} {
		tk := randomToken(n)
		if got := len(tk); got != n*2 { // hex-encoded -> 2 chars per byte
			t.Fatalf("randomToken(%d) returned %d hex chars, want %d", n, got, n*2)
		}
	}
	a, b := randomToken(16), randomToken(16)
	if a == b {
		t.Fatalf("two random tokens collided: %q", a)
	}
}

func TestHashToken(t *testing.T) {
	h1 := hashToken("hello")
	h2 := hashToken("hello")
	h3 := hashToken("hello ")
	if h1 != h2 {
		t.Fatalf("hashToken is not deterministic: %q vs %q", h1, h2)
	}
	if h1 == h3 {
		t.Fatalf("hashToken collided across different inputs")
	}
	if len(h1) != 64 {
		t.Fatalf("expected 64-char hex digest, got %d: %q", len(h1), h1)
	}
}
