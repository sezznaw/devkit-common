package moneyx

import (
	"encoding/json"
	"errors"
	"testing"
)

type fakeThrift struct {
	A int64
	C string
}

func (f fakeThrift) GetAmount() int64    { return f.A }
func (f fakeThrift) GetCurrency() string { return f.C }

func TestParseAndString(t *testing.T) {
	cases := []struct{ in, cur, want string }{
		{"12.34", "USD", "12.34 USD"},
		{"12", "usd", "12.00 USD"},
		{"-0.5", "USD", "-0.50 USD"},
		{"100", "JPY", "100 JPY"},
		{"0.001", "BHD", "0.001 BHD"},
		{"0.00000001", "BTC", "0.00000001 BTC"},
	}
	for _, c := range cases {
		m, err := Parse(c.in, c.cur)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if m.String() != c.want {
			t.Errorf("%s %s: got %s want %s", c.in, c.cur, m, c.want)
		}
	}
	for _, bad := range []string{"12.345", "abc", "", "1,000"} {
		if _, err := Parse(bad, "USD"); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if New(5, "jpy").Decimal() != "5" || New(-5, "USD").Decimal() != "-0.05" || New(123456, "USD").Decimal() != "1234.56" {
		t.Error("decimal rendering")
	}
}

func TestArithmetic(t *testing.T) {
	a := MustParse("10.00", "USD")
	b := MustParse("0.05", "USD")
	if s, _ := a.Add(b); s.Amount != 1005 {
		t.Errorf("add %v", s)
	}
	if d, _ := a.Sub(b); d.Amount != 995 {
		t.Errorf("sub %v", d)
	}
	if _, err := a.Add(New(1, "EUR")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Error("mismatch")
	}
	if c, _ := a.Cmp(b); c != 1 {
		t.Error("cmp")
	}
	// stake 10.00 at odds 1.85 → 18.50
	if p, _ := a.MulDecimal("1.85"); p.Amount != 1850 {
		t.Errorf("odds %v", p)
	}
	// 2.5% fee of 10.00 = 0.25
	if f, _ := a.MulRatio(25, 1000); f.Amount != 25 {
		t.Errorf("fee %v", f)
	}
	// rounding half up: 0.05 * 1.85 = 0.0925 → 0.09; 0.05 * 1.9 = 0.095 → 0.10
	if r, _ := b.MulDecimal("1.85"); r.Amount != 9 {
		t.Errorf("round down %v", r)
	}
	if r, _ := b.MulDecimal("1.9"); r.Amount != 10 {
		t.Errorf("round half up %v", r)
	}
	if r, _ := b.Neg().MulDecimal("1.9"); r.Amount != -10 {
		t.Errorf("round half away from zero %v", r)
	}
	parts, _ := MustParse("1.00", "USD").Split(3)
	if parts[0].Amount != 34 || parts[1].Amount != 33 || parts[2].Amount != 33 {
		t.Errorf("split %v", parts)
	}
	if _, err := New(1<<62, "USD").Add(New(1<<62, "USD")); !errors.Is(err, ErrOverflow) {
		t.Error("overflow")
	}
}

func TestJSONAndThrift(t *testing.T) {
	m := Of(fakeThrift{A: 1234, C: "usd"})
	if m.Amount != 1234 || m.Currency != "USD" {
		t.Errorf("of %v", m)
	}
	b, _ := json.Marshal(m)
	if string(b) != `{"amount":1234,"currency":"USD"}` {
		t.Errorf("json %s", b)
	}
	var back Money
	if err := json.Unmarshal(b, &back); err != nil || back != m {
		t.Errorf("unmarshal %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"amount":1}`), &back); err == nil {
		t.Error("currency required")
	}
	if Of(nil) != (Money{}) {
		t.Error("nil")
	}
}
