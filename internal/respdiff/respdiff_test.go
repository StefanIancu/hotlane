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

// Masking must hide volatility WITHOUT hiding the values a regression
// would change. The old \d{10,19} rule swallowed order numbers,
// balances and phone numbers; the old 16-char hex rule swallowed ETags
// and short digests.
func TestNormalizeDoesNotMaskRealData(t *testing.T) {
	mustDiffer := []struct{ a, b, what string }{
		{`{"balance":1234567890}`, `{"balance":1999999999}`, "account balance"},
		{`{"order":4815162342}`, `{"order":4815162300}`, "order number"},
		{`{"phone":"+12025550142"}`, `{"phone":"+19995550000"}`, "phone number"},
		{`{"etag":"abc123abc123abc1"}`, `{"etag":"fff999fff999fff9"}`, "short etag"},
	}
	for _, c := range mustDiffer {
		if Normalize(c.a) == Normalize(c.b) {
			t.Errorf("%s masked away: %q and %q normalize alike", c.what, c.a, c.b)
		}
	}
	mustMatch := []struct{ a, b, what string }{
		{`{"at":1784551402}`, `{"at":1784551999}`, "unix seconds"},
		{`{"ms":1784551402123}`, `{"ms":1784551999456}`, "unix millis"},
		{`{"trace":"4bf92f3577b34da6a3ce929d0e0e4736"}`, `{"trace":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "trace id"},
		{`{"t":"2026-07-19T14:03:22Z"}`, `{"t":"2026-07-20T09:00:00Z"}`, "iso timestamp"},
	}
	for _, c := range mustMatch {
		if Normalize(c.a) != Normalize(c.b) {
			t.Errorf("%s should be masked but differs: %q vs %q", c.what, Normalize(c.a), Normalize(c.b))
		}
	}
}
