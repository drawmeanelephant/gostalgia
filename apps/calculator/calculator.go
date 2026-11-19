// Package calculator is the pack's simplest utility: real arithmetic behind a
// version-aware presentation. Version 1 keeps the original sixteen flat
// actions; version 2 and later expose the same operations on a four-column
// grid pad that also affords decimal and sign keys.
package calculator

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"

	"gostalgia/sdk"
)

const (
	// ID is the reverse-DNS identifier for the calculator.
	ID = "com.gostalgia.calculator"

	// maxDigits bounds the arithmetic core to an honest 16-digit display.
	maxDigits = 16

	// displayPrec is the number of fractional digits used when rendering a
	// result. It matches maxDigits so the display does not silently round
	// non-zero digits away.
	displayPrec = 16

	// padColumns lays the pad out four keys wide like a desktop calculator.
	padColumns = 4

	// pressAction is the shared action every grid cell declares; cell_id
	// carries which key was pressed.
	pressAction = "press"
)

// keyLabels pairs each semantic key id with its plain arithmetic label.
var keyLabels = map[string]string{
	"digit_0": "0",
	"digit_1": "1",
	"digit_2": "2",
	"digit_3": "3",
	"digit_4": "4",
	"digit_5": "5",
	"digit_6": "6",
	"digit_7": "7",
	"digit_8": "8",
	"digit_9": "9",
	"add":     "+",
	"sub":     "-",
	"mul":     "x",
	"div":     "/",
	"eq":      "=",
	"clear":   "C",
	"dot":     ".",
	"neg":     "±",
}

// padOrder is the row-major key order of the grid pad, four columns wide:
// utility keys up top, digits in calculator order, equals at the right edge,
// and a short final row of zero and decimal.
var padOrder = []string{
	"clear", "neg", "div", "mul",
	"digit_7", "digit_8", "digit_9", "sub",
	"digit_4", "digit_5", "digit_6", "add",
	"digit_1", "digit_2", "digit_3", "eq",
	"digit_0", "dot",
}

// v1Order lists keys for the contract-v1 action fallback, bounded by the
// 16-action limit — exactly the original Calculator key set.
var v1Order = []string{
	"digit_0", "digit_1", "digit_2", "digit_3", "digit_4",
	"digit_5", "digit_6", "digit_7", "digit_8", "digit_9",
	"add", "sub", "mul", "div", "eq", "clear",
}

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid compiled-in data is a programming error
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte { return append([]byte(nil), manifestJSON...) }

// Evaluate computes a left-to-right arithmetic expression with no operator
// precedence. It is the pure arithmetic core used by both the presentation
// action handler and the headless calc route.
func Evaluate(expr string) (string, error) {
	i := 0
	n := len(expr)
	for i < n && expr[i] == ' ' {
		i++
	}
	if i >= n {
		return "", fmt.Errorf("malformed expression")
	}

	left, consumed, err := parseNextNumber(expr[i:])
	if err != nil {
		return "", err
	}
	i += consumed

	for i < n {
		for i < n && expr[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		if !isOpChar(expr[i]) {
			return "", fmt.Errorf("malformed expression")
		}
		op := expr[i]
		i++
		for i < n && expr[i] == ' ' {
			i++
		}
		if i >= n {
			return "", fmt.Errorf("malformed expression")
		}
		right, consumed, err := parseNextNumber(expr[i:])
		if err != nil {
			return "", err
		}
		i += consumed
		res, err := compute(left, right, string(op))
		if err != nil {
			return "", err
		}
		left = res
	}

	return formatResult(left)
}

// parseNextNumber parses a decimal literal with an optional leading sign.
// It returns the parsed value and the number of bytes consumed.
func parseNextNumber(s string) (*big.Rat, int, error) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}

	dot := false
	hasDigit := false
	for i < len(s) {
		c := s[i]
		if c == '.' {
			if dot {
				break
			}
			dot = true
			i++
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		hasDigit = true
		i++
	}

	if !hasDigit {
		return nil, 0, fmt.Errorf("malformed number")
	}

	r, err := parseDecimal(s[:i])
	if err != nil {
		return nil, 0, err
	}
	return r, i, nil
}

// parseDecimal converts a signed decimal string into a *big.Rat, reducing the
// fraction and rejecting values that exceed the 16-digit display budget.
func parseDecimal(s string) (*big.Rat, error) {
	if s == "" {
		return nil, fmt.Errorf("malformed number")
	}

	sign := 1
	i := 0
	if s[0] == '+' {
		i++
	} else if s[0] == '-' {
		sign = -1
		i++
	}
	if i == len(s) {
		return nil, fmt.Errorf("malformed number")
	}

	var sb strings.Builder
	frac := 0
	dot := false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if dot {
				return nil, fmt.Errorf("malformed number")
			}
			dot = true
		case c >= '0' && c <= '9':
			sb.WriteByte(c)
			if dot {
				frac++
			}
		default:
			return nil, fmt.Errorf("malformed number")
		}
	}

	digits := sb.String()
	if digits == "" {
		return nil, fmt.Errorf("malformed number")
	}
	if len(digits) > maxDigits {
		return nil, fmt.Errorf("overflow")
	}

	num, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("malformed number")
	}
	if sign < 0 {
		num.Neg(num)
	}

	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(frac)), nil)
	r := new(big.Rat).SetFrac(num, den)
	if err := checkOverflow(r); err != nil {
		return nil, err
	}
	return r, nil
}

// compute returns a op b, handling divide-by-zero and overflow.
func compute(a, b *big.Rat, op string) (*big.Rat, error) {
	if a == nil || b == nil {
		return nil, fmt.Errorf("malformed input")
	}

	r := new(big.Rat)
	switch op {
	case "+":
		r.Add(a, b)
	case "-":
		r.Sub(a, b)
	case "*", "x":
		r.Mul(a, b)
	case "/":
		if b.Sign() == 0 {
			return nil, fmt.Errorf("cannot divide by zero")
		}
		r.Quo(a, b)
	default:
		return nil, fmt.Errorf("unknown operator %q", op)
	}

	if err := checkOverflow(r); err != nil {
		return nil, err
	}
	return r, nil
}

// checkOverflow rejects results whose numerator or denominator exceeds the
// 16-digit display budget.
func checkOverflow(r *big.Rat) error {
	num := new(big.Int).Abs(r.Num())
	den := r.Denom()
	if len(num.String()) > maxDigits || len(den.String()) > maxDigits {
		return fmt.Errorf("overflow")
	}
	return nil
}

// formatResult renders a *big.Rat as a trimmed decimal string.
func formatResult(r *big.Rat) (string, error) {
	if r == nil {
		return "", fmt.Errorf("no result")
	}
	if r.Sign() == 0 {
		return "0", nil
	}
	if err := checkOverflow(r); err != nil {
		return "", err
	}

	s := r.FloatString(displayPrec)
	if !strings.Contains(s, ".") {
		return s, nil
	}
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		s = "0"
	}
	return s, nil
}

func isOpChar(c byte) bool { return c == '+' || c == '-' || c == '*' || c == 'x' || c == '/' }

// Pad is the calculator's interactive keypad state machine: digit entry,
// chained left-to-right operators, equals, clear, plus the decimal and
// sign-flip keys the version-2 grid pad affords. The version-1 Calculator
// and Dogcalc share it so every pad does identical math. A Pad is not
// concurrency-safe; the owning app serializes presses. A sticky error resets
// the pad on the next press, like a hardware calculator's error state.
type Pad struct {
	left        *big.Rat
	leftInput   string
	leftIsInput bool
	rightInput  string
	op          string
	err         string
}

// NewPad returns a cleared pad ready for input.
func NewPad() *Pad {
	p := new(Pad)
	p.Clear()
	return p
}

// Err reports the sticky error text, or "" when the pad is clean.
func (p *Pad) Err() string { return p.err }

// HasDot reports whether the operand currently being edited already contains
// a decimal point, so a pad may disable its dot key. A computed result or an
// operand not yet started counts as dot-free.
func (p *Pad) HasDot() bool {
	if p.op != "" {
		return strings.Contains(p.rightInput, ".")
	}
	return p.leftIsInput && strings.Contains(p.leftInput, ".")
}

// Display renders the current entry or the pending expression.
func (p *Pad) Display() string {
	var leftStr string
	if p.leftIsInput {
		leftStr = p.leftInput
	} else {
		var err error
		leftStr, err = formatResult(p.left)
		if err != nil {
			leftStr = "0"
		}
	}
	if p.op == "" {
		return leftStr
	}
	if p.rightInput == "" {
		return leftStr + " " + opLabel(p.op)
	}
	return leftStr + " " + opLabel(p.op) + " " + p.rightInput
}

// Digit appends the digit n to the operand being edited.
func (p *Pad) Digit(n int) {
	p.clearErr()
	digit := strconv.Itoa(n)
	if p.op == "" {
		if !p.leftIsInput {
			p.leftInput = digit
			p.left = big.NewRat(int64(n), 1)
			p.leftIsInput = true
			p.rightInput = ""
			p.op = ""
			return
		}
		if p.leftInput == "0" {
			p.leftInput = digit
		} else if p.leftInput == "-0" {
			p.leftInput = "-" + digit
		} else if len(p.leftInput) < maxDigits {
			p.leftInput += digit
		} else {
			p.err = "overflow"
			return
		}
		left, err := parseDecimal(p.leftInput)
		if err != nil {
			p.err = err.Error()
			return
		}
		p.left = left
		return
	}
	if p.rightInput == "" || p.rightInput == "0" {
		p.rightInput = digit
	} else if p.rightInput == "-0" {
		p.rightInput = "-" + digit
	} else if len(p.rightInput) < maxDigits {
		p.rightInput += digit
	} else {
		p.err = "overflow"
		return
	}
}

// Dot appends a decimal point to the operand being edited, or starts a fresh
// fractional entry over a computed result.
func (p *Pad) Dot() {
	p.clearErr()
	if p.op == "" {
		if !p.leftIsInput {
			p.leftInput = "0."
			p.left = big.NewRat(0, 1)
			p.leftIsInput = true
			p.rightInput = ""
			p.op = ""
			return
		}
		if p.HasDot() {
			return
		}
		if !p.appendDot(&p.leftInput) {
			return
		}
		left, err := parseDecimal(p.leftInput)
		if err != nil {
			p.err = err.Error()
			return
		}
		p.left = left
		return
	}
	if p.rightInput == "" {
		p.rightInput = "0."
		return
	}
	p.appendDot(&p.rightInput)
}

// appendDot adds a decimal point to in unless one is present or the input is
// at its length budget; it reports whether the input changed.
func (p *Pad) appendDot(in *string) bool {
	if strings.Contains(*in, ".") {
		return false
	}
	if len(*in) >= maxDigits {
		p.err = "overflow"
		return false
	}
	*in += "."
	return true
}

// Neg flips the sign of the operand being edited, or of a computed result.
func (p *Pad) Neg() {
	p.clearErr()
	if p.op == "" {
		if !p.leftIsInput {
			p.left.Neg(p.left)
			return
		}
		if !p.flip(&p.leftInput) {
			return
		}
		left, err := parseDecimal(p.leftInput)
		if err != nil {
			p.err = err.Error()
			return
		}
		p.left = left
		return
	}
	if p.rightInput == "" {
		p.rightInput = "-0"
		return
	}
	p.flip(&p.rightInput)
}

// flip toggles in's leading minus sign within the input length budget; it
// reports whether the input changed.
func (p *Pad) flip(in *string) bool {
	if strings.HasPrefix(*in, "-") {
		*in = (*in)[1:]
		return true
	}
	if len(*in) >= maxDigits {
		p.err = "overflow"
		return false
	}
	*in = "-" + *in
	return true
}

// Op presses a binary operator, chaining a pending operation first.
func (p *Pad) Op(op string) {
	p.clearErr()
	if p.leftIsInput {
		p.leftIsInput = false
	}
	if p.op == "" {
		p.op = op
		p.rightInput = ""
		return
	}
	if p.rightInput != "" {
		right, err := parseDecimal(p.rightInput)
		if err != nil {
			p.err = err.Error()
			p.rightInput = ""
			p.op = ""
			return
		}
		res, err := compute(p.left, right, p.op)
		if err != nil {
			p.err = err.Error()
			p.rightInput = ""
			p.op = ""
			return
		}
		p.left = res
		p.leftIsInput = false
		p.rightInput = ""
	}
	p.op = op
}

// Eq completes the pending operation.
func (p *Pad) Eq() {
	p.clearErr()
	if p.op == "" || p.rightInput == "" {
		return
	}
	if p.leftIsInput {
		p.leftIsInput = false
	}
	right, err := parseDecimal(p.rightInput)
	if err != nil {
		p.err = err.Error()
		p.rightInput = ""
		p.op = ""
		return
	}
	res, err := compute(p.left, right, p.op)
	if err != nil {
		p.err = err.Error()
		p.rightInput = ""
		p.op = ""
		return
	}
	p.left = res
	p.leftIsInput = false
	p.rightInput = ""
	p.op = ""
}

// Clear resets the pad to a zeroed, ready state.
func (p *Pad) Clear() {
	p.left = big.NewRat(0, 1)
	p.leftInput = "0"
	p.leftIsInput = true
	p.rightInput = ""
	p.op = ""
	p.err = ""
}

// clearErr resets a sticky error before the next press; Clear itself already
// is that reset, so callers simply run before applying their own edit.
func (p *Pad) clearErr() {
	if p.err != "" {
		p.Clear()
	}
	p.err = ""
}

func opLabel(op string) string {
	switch op {
	case "*":
		return "x"
	case "/":
		return "/"
	case "+":
		return "+"
	case "-":
		return "-"
	}
	return op
}

// Calc is the running in-process instance of the calculator.
type Calc struct {
	app *sdk.Context
	mu  sync.Mutex
	pad *Pad
}

// Factory returns a fresh, uninitialized Calc instance.
func Factory() (sdk.Instance, error) {
	return &Calc{pad: NewPad()}, nil
}

// Init registers the presentation surface and the headless calc route.
func (c *Calc) Init(app *sdk.Context) error {
	c.app = app
	if err := app.Present(c.view, c.act); err != nil {
		return err
	}
	return app.Handle("calc", c.calc)
}

// Run blocks until the application is cancelled.
func (c *Calc) Run(ctx context.Context) error {
	c.app.Log.Info("calculator running")
	<-ctx.Done()
	return nil
}

// Stop is called once on exit, including after a failed Init.
func (c *Calc) Stop(context.Context) error { return nil }

func (c *Calc) view(ctx context.Context) (sdk.View, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// viewLocked renders the pad the negotiated version can express: a modern
// grid, or the original flat sixteen actions for version 1.
func (c *Calc) viewLocked(version int) sdk.View {
	v := sdk.View{
		Title:  "Calculator",
		State:  sdk.ViewReady,
		Items:  []sdk.Item{{ID: "display", Label: "Display", Detail: c.pad.Display()}},
		Status: "Enter a calculation",
	}
	if version >= 2 {
		v.Grid = c.gridLocked()
	} else {
		v.Actions = v1Actions()
	}
	if err := c.pad.Err(); err != "" {
		v.State = sdk.ViewError
		v.Error = err
		v.Status = err
	}
	return v
}

// gridLocked builds the pad snapshot, dimming the decimal key once the operand
// being edited already has its point.
func (c *Calc) gridLocked() *sdk.Grid {
	cells := make([]sdk.Cell, 0, len(padOrder))
	for _, id := range padOrder {
		cells = append(cells, sdk.Cell{
			ID:       id,
			Label:    keyLabels[id],
			Action:   pressAction,
			Disabled: id == "dot" && c.pad.HasDot(),
		})
	}
	return &sdk.Grid{Label: "Keypad", Columns: padColumns, Cells: cells}
}

// v1Actions builds the sixteen-key fallback the version-1 contract bounds.
func v1Actions() []sdk.Action {
	actions := make([]sdk.Action, 0, len(v1Order))
	for _, id := range v1Order {
		actions = append(actions, sdk.Action{ID: id, Label: keyLabels[id]})
	}
	return actions
}

func (c *Calc) act(ctx context.Context, p sdk.ActionRequest) (sdk.View, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := p.Action
	if p.CellID != "" {
		// A grid press names its key in cell_id.
		key = p.CellID
	}
	switch {
	case strings.HasPrefix(key, "digit_"):
		n, err := strconv.Atoi(strings.TrimPrefix(key, "digit_"))
		if err != nil || n < 0 || n > 9 {
			return sdk.View{}, fmt.Errorf("unknown action %q", p.Action)
		}
		c.pad.Digit(n)
	case key == "add":
		c.pad.Op("+")
	case key == "sub":
		c.pad.Op("-")
	case key == "mul":
		c.pad.Op("*")
	case key == "div":
		c.pad.Op("/")
	case key == "eq":
		c.pad.Eq()
	case key == "clear":
		c.pad.Clear()
	case key == "dot":
		c.pad.Dot()
	case key == "neg":
		c.pad.Neg()
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", p.Action)
	}

	return c.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// calc is the headless route that evaluates a full expression string.
func (c *Calc) calc(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Expr string `json:"expr"`
	}
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Expr == "" {
		return nil, fmt.Errorf("expr is required")
	}
	res, err := Evaluate(p.Expr)
	if err != nil {
		return nil, err
	}
	return map[string]string{"result": res}, nil
}
