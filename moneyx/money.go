// Package moneyx is the one representation of money in a service: a
// decimal amount in the currency's major unit ("12.34") plus the ISO 4217
// code, kept exact by shopspring/decimal, with a rounding policy chosen
// at the one place a fraction can appear.
//
// Why a decimal and not an integer of minor units (cents):
//
//   - Money crosses every boundary of this system as a decimal: provider
//     callbacks and payment channels send "12.34", players type 12.34, the
//     ledger, reports and reconciliation files show 12.34. An integer of
//     cents would need a conversion at each boundary, each one knowing the
//     currency's exponent (JPY has none, BHD has three), and a mistaken
//     1234 for 12.34 is the classic money bug. With a decimal, 12.34 is
//     12.34 in the IDL, the JSON, the database and the logs; the exponent
//     only matters when rounding.
//   - The platform has fiat with 0, 2 and 3 decimals and crypto with 8;
//     exact SQL SUMs over DECIMAL columns and exact FX arithmetic are
//     easier with a decimal than with per-currency integer scales.
//   - Integers win only in an engine doing millions of operations a
//     second; nothing here is that, and the odds engines are the vendors'.
//   - This is the mainstream choice: BigDecimal in Java and C# betting and
//     payment systems, shopspring/decimal in Go fintech code.
//
// Why shopspring/decimal and only it: the most used decimal in Go,
// maintained, implements sql.Scanner / driver.Valuer, parses floats and
// strings safely. The library does the arithmetic; this package adds what
// it cannot know: the currency, the currency's scale, the rounding policy
// and the IDL / JSON / GORM shape.
//
// The three mistakes the type prevents, and how:
//
//  1. A fraction of the smallest unit ("12.345 USD") creeping in: every
//     constructor and every operation normalises to the currency's scale;
//     an input with more decimals is an error, never a silent rounding.
//  2. Rounding in the middle of a calculation: Mul rounds once, at the
//     end, with an explicit mode; a multi-step formula keeps decimals in
//     between (Amount()) and comes back through FromDecimal once.
//  3. Comparing with ==: the amount is a decimal.Decimal whose == compares
//     internals, not values. Use Equal and Cmp; the fields stay exported
//     only because GORM's embedding needs them.
//
// Shapes: IDL common.Money {string amount, string currency}; JSON
// {"amount":"12.34","currency":"USD"} (a string, so JavaScript never
// touches it as a float); GORM embedded with a prefix, two columns
// <name>_amount DECIMAL(24,8) and <name>_currency CHAR(3).
//
// 金额的唯一表示：货币主单位的小数（"12.34"）加 ISO 4217 货币码，由 shopspring/decimal
// 保证精确，舍入只在唯一可能出现小数的地方（乘比例）按指定模式做一次。
//
// 为什么用小数而不是"最小单位的整数（分）"：钱在本系统的每个边界上都是小数——厂商回调、
// 支付渠道、玩家输入、流水表、报表、对账文件。整数分要在每个边界换算一次，每次都得知道
// 货币的小数位（日元 0 位、巴林第纳尔 3 位），把 12.34 传成 1234 是经典事故；用小数则
// 12.34 在 IDL、JSON、数据库、日志里都是 12.34，小数位只在舍入时才需要知道。多币种
// （法币 0/2/3 位、加密货币 8 位）、SQL 精确求和、换汇，小数都更顺。整数只在每秒百万次
// 运算的引擎里占优，我们不是，而且赔率引擎是厂商的。Java/C# 的博彩和支付系统用 BigDecimal，
// Go 的金融项目用 shopspring/decimal，这是主流做法。
//
// 只用 shopspring/decimal 一个库：Go 里用得最多、维护活跃、支持 sql Scan/Value、能安全
// 读入浮点和字符串。运算交给它；本包只补它不知道的：货币、货币小数位、舍入策略、IDL/JSON/GORM
// 的形状。
//
// 本类型防的三件事：半分钱混进来（构造和每次运算后按货币小数位规整，多出来的位报错）；
// 中途舍入（Mul 只在最后按指定模式舍一次，多步公式中间用 Amount() 走小数、最后 FromDecimal
// 回来）；用 == 比较（decimal 的 == 比的是内部结构，比较只能用 Equal、Cmp；字段导出仅因
// GORM 嵌入需要）。
package moneyx

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Money is a decimal amount in a currency's major unit. The fields are
// exported for GORM (embed with a prefix) and JSON; never compare two
// Money with ==, use Equal or Cmp.
type Money struct {
	Amount   decimal.Decimal `json:"amount" gorm:"column:amount;type:decimal(24,8)"`
	Currency string          `json:"currency" gorm:"column:currency;type:char(3)"`
}

// Rounding is how a fraction of the smallest unit is resolved. Which one
// applies is a business rule (a payout may be rounded down in the house's
// favour, a refund half up): pass it where money meets a rate.
type Rounding int

const (
	// RoundHalfUp: half away from zero (12.345 → 12.35, -12.345 → -12.35).
	RoundHalfUp Rounding = iota
	// RoundDown: towards zero (12.349 → 12.34). Payouts, bonuses.
	RoundDown
	// RoundUp: away from zero (12.341 → 12.35). Fees the house charges.
	RoundUp
	// RoundHalfEven: banker's rounding, for statistics and settlement sums.
	RoundHalfEven
)

var (
	// ErrCurrencyMismatch: two amounts of different currencies were combined.
	ErrCurrencyMismatch = errors.New("moneyx: currency mismatch")
	// ErrScale: the amount has more decimals than the currency's smallest unit.
	ErrScale = errors.New("moneyx: more decimals than the currency allows")
	// ErrBadAmount: the amount could not be parsed.
	ErrBadAmount = errors.New("moneyx: bad amount")
	// ErrCurrency: the currency code is missing or malformed.
	ErrCurrency = errors.New("moneyx: bad currency")
)

// scales lists the ISO 4217 currencies whose minor unit is not 2 decimals
// (the complete exception list of the standard; everything else is 2),
// plus the crypto assets the platform settles.
var scales = map[string]int32{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	"CLF": 4, "UYW": 4,
	"BTC": 8, "ETH": 8, "USDT": 6, "USDC": 6,
}

// Scale is the number of decimals of the currency's smallest unit: 2 for
// USD, 0 for JPY, 3 for KWD, 8 for BTC. Unknown codes use 2.
func Scale(currency string) int32 {
	if s, ok := scales[strings.ToUpper(currency)]; ok {
		return s
	}
	return 2
}

// RegisterCurrency adds or changes a currency's scale (a new crypto asset,
// a platform-specific unit). Call it at start-up.
func RegisterCurrency(code string, scale int32) {
	scales[strings.ToUpper(code)] = scale
}

func normCurrency(c string) (string, error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) < 3 || len(c) > 8 {
		return "", fmt.Errorf("%w: %q", ErrCurrency, c)
	}
	return c, nil
}

// FromDecimal makes a Money from a decimal, rounding to the currency's
// scale with mode. This is the one way a value with more decimals than
// the currency becomes a Money: the end of a calculation.
func FromDecimal(d decimal.Decimal, currency string, mode Rounding) (Money, error) {
	c, err := normCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: round(d, Scale(c), mode), Currency: c}, nil
}

// Parse reads "12.34" (or "12", "-0.5") in the currency's major unit.
// More decimals than the currency has is ErrScale: input is never rounded
// silently; round on purpose with FromDecimal if that is wanted.
func Parse(amount, currency string) (Money, error) {
	c, err := normCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	d, err := decimal.NewFromString(strings.TrimSpace(amount))
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrBadAmount, amount)
	}
	if -d.Exponent() > Scale(c) && !d.Equal(d.Truncate(Scale(c))) {
		return Money{}, fmt.Errorf("%w: %q has more than %d decimals for %s", ErrScale, amount, Scale(c), c)
	}
	return Money{Amount: d.Truncate(Scale(c)), Currency: c}, nil
}

// MustParse is Parse for constants in code and tests; it panics on error.
func MustParse(amount, currency string) Money {
	m, err := Parse(amount, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero is 0 of a currency.
func Zero(currency string) Money {
	c, _ := normCurrency(currency)
	return Money{Amount: decimal.Zero, Currency: c}
}

// thriftMoney is what a generated common.Money (kitex_gen, hertz_gen)
// satisfies, whichever service generated it.
type thriftMoney interface {
	GetAmount() string
	GetCurrency() string
}

// Of converts a generated IDL Money (two string fields) into a Money;
// nil is an error, as is a bad amount or scale. Validate request money
// with it at the edge of the service.
func Of(m thriftMoney) (Money, error) {
	if m == nil {
		return Money{}, fmt.Errorf("%w: money is required", ErrBadAmount)
	}
	return Parse(m.GetAmount(), m.GetCurrency())
}

// Parts are the two IDL fields, for building the generated struct:
// &common.Money{Amount: a, Currency: c}.
func (m Money) Parts() (amount, currency string) {
	return m.String(), m.Currency
}

// String renders the amount with exactly the currency's decimals: "12.34",
// "-0.50", "100" (JPY), "0.00000001" (BTC).
func (m Money) String() string {
	return m.Amount.StringFixed(Scale(m.Currency))
}

// Display is "12.34 USD".
func (m Money) Display() string {
	return m.String() + " " + m.Currency
}

// IsZero reports an amount of 0.
func (m Money) IsZero() bool { return m.Amount.IsZero() }

// IsNegative reports an amount below 0.
func (m Money) IsNegative() bool { return m.Amount.IsNegative() }

// IsPositive reports an amount above 0.
func (m Money) IsPositive() bool { return m.Amount.IsPositive() }

// Neg is the amount with the opposite sign.
func (m Money) Neg() Money { return Money{Amount: m.Amount.Neg(), Currency: m.Currency} }

// Abs is the amount without its sign.
func (m Money) Abs() Money { return Money{Amount: m.Amount.Abs(), Currency: m.Currency} }

func (m Money) same(o Money) error {
	if m.Currency != o.Currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return nil
}

// Add returns m + o; the currencies must match. Exact: no rounding.
func (m Money) Add(o Money) (Money, error) {
	if err := m.same(o); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Add(o.Amount), Currency: m.Currency}, nil
}

// Sub returns m - o; the currencies must match. Exact.
func (m Money) Sub(o Money) (Money, error) {
	if err := m.same(o); err != nil {
		return Money{}, err
	}
	return Money{Amount: m.Amount.Sub(o.Amount), Currency: m.Currency}, nil
}

// Equal reports the same currency and amount (what == would not do).
func (m Money) Equal(o Money) bool {
	return m.Currency == o.Currency && m.Amount.Equal(o.Amount)
}

// Cmp compares two amounts of the same currency: -1, 0 or 1.
func (m Money) Cmp(o Money) (int, error) {
	if err := m.same(o); err != nil {
		return 0, err
	}
	return m.Amount.Cmp(o.Amount), nil
}

// Mul returns m times a rate (odds 1.85, a fee rate 0.025, an FX rate
// 7.2345), rounded once to the currency's scale with mode. The rate is a
// decimal.Decimal: rates arrive as strings or decimals, never as float64.
func (m Money) Mul(rate decimal.Decimal, mode Rounding) Money {
	return Money{Amount: round(m.Amount.Mul(rate), Scale(m.Currency), mode), Currency: m.Currency}
}

// MulString is Mul with the rate as a string ("1.85"), for rates taken
// from configuration or an IDL common.Decimal field.
func (m Money) MulString(rate string, mode Rounding) (Money, error) {
	r, err := decimal.NewFromString(strings.TrimSpace(rate))
	if err != nil {
		return Money{}, fmt.Errorf("%w: rate %q", ErrBadAmount, rate)
	}
	return m.Mul(r, mode), nil
}

// Split divides m into n parts that add up exactly to m; the remainder
// of the smallest units goes one at a time to the first parts.
func (m Money) Split(n int) ([]Money, error) {
	if n <= 0 {
		return nil, errors.New("moneyx: split into zero parts")
	}
	scale := Scale(m.Currency)
	unit := decimal.New(1, -scale)
	base := m.Amount.Div(decimal.NewFromInt(int64(n))).Truncate(scale)
	rem := m.Amount.Sub(base.Mul(decimal.NewFromInt(int64(n))))
	out := make([]Money, n)
	for i := range out {
		out[i] = Money{Amount: base, Currency: m.Currency}
	}
	step := unit
	if rem.IsNegative() {
		step = unit.Neg()
		rem = rem.Neg()
	}
	units := rem.Div(unit).IntPart()
	for i := int64(0); i < units && int(i) < n; i++ {
		out[i].Amount = out[i].Amount.Add(step)
	}
	return out, nil
}

// Allocate shares m by weights (commission tiers 70/20/10), exact, with
// the rounding remainder going to the first parts.
func (m Money) Allocate(weights ...int64) ([]Money, error) {
	var total int64
	for _, w := range weights {
		if w < 0 {
			return nil, errors.New("moneyx: negative weight")
		}
		total += w
	}
	if total == 0 {
		return nil, errors.New("moneyx: zero weights")
	}
	scale := Scale(m.Currency)
	unit := decimal.New(1, -scale)
	out := make([]Money, len(weights))
	sum := decimal.Zero
	for i, w := range weights {
		part := m.Amount.Mul(decimal.NewFromInt(w)).Div(decimal.NewFromInt(total)).Truncate(scale)
		out[i] = Money{Amount: part, Currency: m.Currency}
		sum = sum.Add(part)
	}
	rem := m.Amount.Sub(sum)
	step := unit
	if rem.IsNegative() {
		step = unit.Neg()
		rem = rem.Neg()
	}
	units := rem.Div(unit).IntPart()
	for i := int64(0); i < units && int(i) < len(out); i++ {
		out[i].Amount = out[i].Amount.Add(step)
	}
	return out, nil
}

func round(d decimal.Decimal, scale int32, mode Rounding) decimal.Decimal {
	switch mode {
	case RoundDown:
		return d.RoundDown(scale)
	case RoundUp:
		return d.RoundUp(scale)
	case RoundHalfEven:
		return d.RoundBank(scale)
	default:
		return d.Round(scale)
	}
}

// MarshalJSON writes {"amount":"12.34","currency":"USD"}: the amount as a
// string with the currency's decimals, the IDL's and the gateway's form.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{m.String(), m.Currency})
}

// UnmarshalJSON reads the same form (the amount may also arrive as a JSON
// number); a missing currency or too many decimals is an error.
func (m *Money) UnmarshalJSON(b []byte) error {
	var p struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	amount := strings.Trim(string(p.Amount), `"`)
	parsed, err := Parse(amount, p.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
