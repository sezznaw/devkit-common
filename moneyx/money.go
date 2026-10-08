// Package moneyx is the one representation of money in a service: an
// integer amount in the currency's smallest unit (cents for USD, yen for
// JPY) plus the ISO 4217 code. It never holds a float. Arithmetic that
// can produce fractions (a stake times decimal odds, a percentage fee)
// rounds half up to the smallest unit, once, at the end.
//
// The IDL side is `common.Money` (idl/common/common.thrift), two fields
// with the same names; Of converts a generated struct in, Amount and
// Currency go out. GORM stores a Money as two columns: embed it with a
// prefix (`gorm:"embedded;embeddedPrefix:balance_"`), which gives
// balance_amount BIGINT and balance_currency CHAR(3).
//
// moneyx 是服务里金额的唯一表示：最小货币单位的整数金额（美元是分、日元是元）加
// ISO 4217 货币码，永远不是浮点。会出现小数的运算（本金乘赔率、按比例收费）只在最后
// 四舍五入到最小单位一次。IDL 里对应 `common.Money`；GORM 里作为嵌入结构存两列。
package moneyx

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Money is an amount in the smallest unit of a currency.
type Money struct {
	Amount   int64  `json:"amount" gorm:"column:amount"`
	Currency string `json:"currency" gorm:"column:currency;type:char(3)"`
}

// ErrCurrencyMismatch: two amounts of different currencies were combined.
var ErrCurrencyMismatch = errors.New("moneyx: currency mismatch")

// ErrBadDecimal: a decimal string could not be parsed.
var ErrBadDecimal = errors.New("moneyx: bad decimal")

// ErrOverflow: the result does not fit in int64.
var ErrOverflow = errors.New("moneyx: overflow")

// exponents are the minor-unit digits of the currencies that differ from
// the default 2 (ISO 4217). Unknown currencies use 2.
var exponents = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "IDR": 0, "CLP": 0, "ISK": 0, "UGX": 0, "XOF": 0, "XAF": 0,
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3, "IQD": 3, "LYD": 3,
	// crypto, as the platform settles them
	"USDT": 2, "BTC": 8, "ETH": 8,
}

// Exponent is the number of decimal digits of the currency's smallest
// unit: 2 for USD (cents), 0 for JPY, 8 for BTC.
func Exponent(currency string) int {
	if e, ok := exponents[strings.ToUpper(currency)]; ok {
		return e
	}
	return 2
}

// New makes a Money from an amount already in the smallest unit.
func New(amount int64, currency string) Money {
	return Money{Amount: amount, Currency: strings.ToUpper(currency)}
}

// thriftMoney is what a generated common.Money (kitex_gen, hertz_gen)
// satisfies, whichever service generated it.
type thriftMoney interface {
	GetAmount() int64
	GetCurrency() string
}

// Of converts a generated IDL Money (or anything with the two getters);
// nil gives the zero Money.
func Of(m thriftMoney) Money {
	if m == nil {
		return Money{}
	}
	return New(m.GetAmount(), m.GetCurrency())
}

// Parse reads a decimal string in major units ("12.34", "-0.5", "100")
// into the currency's smallest unit; more decimals than the currency has
// is an error, never a silent rounding.
func Parse(decimal, currency string) (Money, error) {
	s := strings.TrimSpace(decimal)
	if s == "" {
		return Money{}, ErrBadDecimal
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	exp := Exponent(currency)
	if len(frac) > exp {
		return Money{}, fmt.Errorf("%w: %q has more than %d decimals for %s", ErrBadDecimal, decimal, exp, currency)
	}
	frac += strings.Repeat("0", exp-len(frac))
	digits := intPart + frac
	for _, r := range digits {
		if r < '0' || r > '9' {
			return Money{}, fmt.Errorf("%w: %q", ErrBadDecimal, decimal)
		}
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if !ok || !n.IsInt64() {
		return Money{}, ErrOverflow
	}
	a := n.Int64()
	if neg {
		a = -a
	}
	return New(a, currency), nil
}

// MustParse is Parse for constants in code and tests; it panics on error.
func MustParse(decimal, currency string) Money {
	m, err := Parse(decimal, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// String renders the amount in major units with the currency's decimals:
// "12.34 USD", "-5 JPY".
func (m Money) String() string {
	return m.Decimal() + " " + m.Currency
}

// Decimal renders the amount in major units: "12.34", "-0.05", "100".
func (m Money) Decimal() string {
	exp := Exponent(m.Currency)
	a := m.Amount
	neg := a < 0
	if neg {
		a = -a
	}
	s := fmt.Sprintf("%d", a)
	if exp > 0 {
		if len(s) <= exp {
			s = strings.Repeat("0", exp-len(s)+1) + s
		}
		s = s[:len(s)-exp] + "." + s[len(s)-exp:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// IsZero reports an amount of 0.
func (m Money) IsZero() bool { return m.Amount == 0 }

// IsNegative reports an amount below 0.
func (m Money) IsNegative() bool { return m.Amount < 0 }

// Neg is the amount with the opposite sign.
func (m Money) Neg() Money { return Money{Amount: -m.Amount, Currency: m.Currency} }

// Abs is the amount without its sign.
func (m Money) Abs() Money {
	if m.Amount < 0 {
		return m.Neg()
	}
	return m
}

func (m Money) same(o Money) error {
	if !strings.EqualFold(m.Currency, o.Currency) {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return nil
}

// Add returns m + o; the currencies must match.
func (m Money) Add(o Money) (Money, error) {
	if err := m.same(o); err != nil {
		return Money{}, err
	}
	r := m.Amount + o.Amount
	if (o.Amount > 0 && r < m.Amount) || (o.Amount < 0 && r > m.Amount) {
		return Money{}, ErrOverflow
	}
	return Money{Amount: r, Currency: m.Currency}, nil
}

// Sub returns m - o; the currencies must match.
func (m Money) Sub(o Money) (Money, error) {
	return m.Add(o.Neg())
}

// Cmp compares two amounts of the same currency: -1, 0 or 1.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.same(o); err != nil {
		return 0, err
	}
	switch {
	case m.Amount < o.Amount:
		return -1, nil
	case m.Amount > o.Amount:
		return 1, nil
	}
	return 0, nil
}

// MulRatio returns m * num / den, rounded half up (away from zero) to the
// smallest unit: a stake times decimal odds given as 1.85 = 185/100, a 2.5%
// fee as 25/1000. This is the only place a fraction appears.
func (m Money) MulRatio(num, den int64) (Money, error) {
	if den == 0 {
		return Money{}, errors.New("moneyx: zero denominator")
	}
	p := new(big.Int).Mul(big.NewInt(m.Amount), big.NewInt(num))
	q := big.NewInt(den)
	if q.Sign() < 0 {
		q.Neg(q)
		p.Neg(p)
	}
	// round half away from zero: (2p + q) / (2q), with the sign handled
	twice := new(big.Int).Mul(p, big.NewInt(2))
	neg := twice.Sign() < 0
	if neg {
		twice.Neg(twice)
	}
	twice.Add(twice, q)
	r := twice.Div(twice, new(big.Int).Mul(q, big.NewInt(2)))
	if neg {
		r.Neg(r)
	}
	if !r.IsInt64() {
		return Money{}, ErrOverflow
	}
	return Money{Amount: r.Int64(), Currency: m.Currency}, nil
}

// MulDecimal returns m times a decimal factor given as a string ("1.85",
// "0.025"), rounded half up to the smallest unit. Odds and rates arrive
// as strings or fixed-point integers, never as float64.
func (m Money) MulDecimal(factor string) (Money, error) {
	s := strings.TrimSpace(factor)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	digits := intPart + frac
	if digits == "" {
		return Money{}, ErrBadDecimal
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return Money{}, fmt.Errorf("%w: %q", ErrBadDecimal, factor)
		}
	}
	num, ok := new(big.Int).SetString(digits, 10)
	if !ok || !num.IsInt64() {
		return Money{}, ErrOverflow
	}
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
	if !den.IsInt64() {
		return Money{}, ErrOverflow
	}
	n := num.Int64()
	if neg {
		n = -n
	}
	return m.MulRatio(n, den.Int64())
}

// Split divides m into n parts that add up exactly to m, the remainder
// going one unit at a time to the first parts (a payout shared by agents).
func (m Money) Split(n int) ([]Money, error) {
	if n <= 0 {
		return nil, errors.New("moneyx: split into zero parts")
	}
	base := m.Amount / int64(n)
	rem := m.Amount - base*int64(n)
	out := make([]Money, n)
	for i := range out {
		out[i] = Money{Amount: base, Currency: m.Currency}
	}
	step := int64(1)
	if rem < 0 {
		step = -1
		rem = -rem
	}
	for i := int64(0); i < rem; i++ {
		out[i].Amount += step
	}
	return out, nil
}

// MarshalJSON writes {"amount":1234,"currency":"USD"}: the wire form is
// the IDL's, so a gateway passes it through unchanged.
func (m Money) MarshalJSON() ([]byte, error) {
	type plain Money
	return json.Marshal(plain(m))
}

// UnmarshalJSON reads the same form; a missing currency is an error.
func (m *Money) UnmarshalJSON(b []byte) error {
	type plain Money
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Currency == "" {
		return errors.New("moneyx: currency is required")
	}
	*m = New(p.Amount, p.Currency)
	return nil
}
