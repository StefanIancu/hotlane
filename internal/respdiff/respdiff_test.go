package respdiff

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"started":"2026-07-19T14:03:22Z"}`, `{"started":"<ts>"}`},
		{`{"started":"2026-07-19 14:03:22.123+02:00"}`, `{"started":"<ts>"}`},
		{`Date: Sat, 19 Jul 2026 14:03:22 GMT`, `Date: <ts>`},
		{`{"req":"7f9c2ba4-e88f-11e9-a1b2-9cb6d0d493a1"}`, `{"req":"<uuid>"}`},
		{`trace 4bf92f3577b34da6a3ce929d0e0e4736 done`, `trace <hex> done`},
		{`{"ts":1784551402,"ms":1784551402123}`, `{"ts":<num>,"ms":<num>}`},
		// stays put: short numbers, version strings, plain words
		{`{"version":"1.2.3","port":3000,"items":42}`, `{"version":"1.2.3","port":3000,"items":42}`},
		{`hello from demo-app v2`, `hello from demo-app v2`},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("abcdef", 4); got != "abcd..." {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("ab", 4); got != "ab" {
		t.Errorf("Truncate short = %q", got)
	}
}
