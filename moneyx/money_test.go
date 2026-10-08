package moneyx

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

type fakeThrift struct {
	A string
	C string
}

func (f fakeThrift) GetAmount() string   { return f.A }
func (f fakeThrift) GetCurrency() string { return f.C }

func TestParseAndString(t *testing.T) {
	cases := []struct{ in, cur, want string }{
		{"12.34", "USD", "12.34"},
		{"12", "usd", "12.00"},
		{"-0.5", "USD", "-0.50"},
		{"100", "JPY", "100"},
		{"0.001", "BHD", "0.001"},
		{"0.00000001", "BTC", "0.00000001"},
		{"12.3400", "USD", "12.34"}, // trailing zeros are not extra decimals
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
	if _, err := Parse("12.345", "USD"); !errors.Is(err, ErrScale) {
		t.Error("too many decimals must be ErrScale, never rounded")
	}
	for _, bad := range []string{"abc", "", "1,000"} {
		if _, err := Parse(bad, "USD"); !errors.Is(err, ErrBadAmount) {
			t.Errorf("%q should be ErrBadAmount", bad)
		}
	}
	if _, err := Parse("1", "X"); !errors.Is(err, ErrCurrency) {
		t.Error("bad currency")
	}
	if MustParse("12.34", "usd").Display() != "12.34 USD" {
		t.Error("display")
	}
}

func TestArithmetic(t *testing.T) {
	a := MustParse("10.00", "USD")
	b := MustParse("0.05", "USD")
	if s, _ := a.Add(b); s.String() != "10.05" {
		t.Errorf("add %v", s)
	}
	if d, _ := a.Sub(b); d.String() != "9.95" {
		t.Errorf("sub %v", d)
	}
	if _, err := a.Add(MustParse("1", "EUR")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Error("mismatch")
	}
	if c, _ := a.Cmp(b); c != 1 {
		t.Error("cmp")
	}
	if !a.Equal(MustParse("10", "USD")) || a.Equal(b) {
		t.Error("equal compares values")
	}
	// stake 10.00 at odds 1.85 → 18.50
	if p, _ := a.MulString("1.85", RoundHalfUp); p.String() != "18.50" {
		t.Errorf("odds %v", p)
	}
	// 2.5% fee of 10.00 = 0.25
	if f := a.Mul(decimal.RequireFromString("0.025"), RoundHalfUp); f.String() != "0.25" {
		t.Errorf("fee %v", f)
	}
	// rounding modes on 0.05 * 1.85 = 0.0925 and 0.05 * 1.9 = 0.095
	if r, _ := b.MulString("1.85", RoundHalfUp); r.String() != "0.09" {
		t.Errorf("half up down %v", r)
	}
	if r, _ := b.MulString("1.9", RoundHalfUp); r.String() != "0.10" {
		t.Errorf("half up %v", r)
	}
	if r, _ := b.MulString("1.9", RoundDown); r.String() != "0.09" {
		t.Errorf("down %v", r)
	}
	if r, _ := b.MulString("1.81", RoundUp); r.String() != "0.10" {
		t.Errorf("up %v", r)
	}
	if r, _ := b.MulString("1.9", RoundHalfEven); r.String() != "0.10" {
		t.Errorf("half even %v", r)
	}
	if r, _ := b.MulString("1.7", RoundHalfEven); r.String() != "0.08" { // 0.085 → 0.08
		t.Errorf("half even to even %v", r)
	}
	if r, _ := b.Neg().MulString("1.9", RoundHalfUp); r.String() != "-0.10" {
		t.Errorf("half away from zero %v", r)
	}
	// JPY has no decimals
	if r, _ := MustParse("100", "JPY").MulString("1.85", RoundDown); r.String() != "185" {
		t.Errorf("jpy %v", r)
	}
	// a multi-step formula keeps decimals in between and rounds once at the end
	raw := a.Amount.Mul(decimal.RequireFromString("1.85")).Mul(decimal.RequireFromString("0.995"))
	if m, _ := FromDecimal(raw, "USD", RoundDown); m.String() != "18.40" { // 18.4075 → 18.40
		t.Errorf("from decimal %v", m)
	}
	parts, _ := MustParse("1.00", "USD").Split(3)
	if parts[0].String() != "0.34" || parts[1].String() != "0.33" || parts[2].String() != "0.33" {
		t.Errorf("split %v", parts)
	}
	alloc, _ := MustParse("100.00", "USD").Allocate(70, 20, 10)
	if alloc[0].String() != "70.00" || alloc[1].String() != "20.00" || alloc[2].String() != "10.00" {
		t.Errorf("allocate %v", alloc)
	}
	alloc, _ = MustParse("0.10", "USD").Allocate(1, 1, 1)
	sum := decimal.Zero
	for _, p := range alloc {
		sum = sum.Add(p.Amount)
	}
	if !sum.Equal(decimal.RequireFromString("0.10")) || alloc[0].String() != "0.04" {
		t.Errorf("allocate remainder %v", alloc)
	}
}

func TestJSONAndThrift(t *testing.T) {
	m, err := Of(fakeThrift{A: "12.34", C: "usd"})
	if err != nil || m.String() != "12.34" || m.Currency != "USD" {
		t.Errorf("of %v %v", m, err)
	}
	if _, err := Of(fakeThrift{A: "12.345", C: "USD"}); !errors.Is(err, ErrScale) {
		t.Error("scale at the edge")
	}
	if _, err := Of(nil); err == nil {
		t.Error("nil")
	}
	a, c := m.Parts()
	if a != "12.34" || c != "USD" {
		t.Error("parts")
	}
	b, _ := json.Marshal(m)
	if string(b) != `{"amount":"12.34","currency":"USD"}` {
		t.Errorf("json %s", b)
	}
	var back Money
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(m) {
		t.Errorf("unmarshal %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"amount":12.5,"currency":"USD"}`), &back); err != nil || back.String() != "12.50" {
		t.Errorf("number amount %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"amount":"1"}`), &back); err == nil {
		t.Error("currency required")
	}
	if err := json.Unmarshal([]byte(`{"amount":"1.005","currency":"USD"}`), &back); !errors.Is(err, ErrScale) {
		t.Error("json scale")
	}
}

func TestScale(t *testing.T) {
	if Scale("usd") != 2 || Scale("JPY") != 0 || Scale("KWD") != 3 || Scale("BTC") != 8 || Scale("XYZ") != 2 {
		t.Error("scales")
	}
	RegisterCurrency("CHIP", 0)
	if Scale("chip") != 0 {
		t.Error("register")
	}
}
