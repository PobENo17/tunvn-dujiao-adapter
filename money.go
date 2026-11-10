package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// micros is a fixed-point money value: 1 unit = 1_000_000 micros.
type micros int64

const microScale = 1_000_000

// parseMicros converts a decimal string or JSON number into micros without
// floating point drift. Accepts an optional leading sign and up to 6 decimals.
func parseMicros(raw string) (micros, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		fracPart = s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, err
	}
	if len(fracPart) > 6 {
		fracPart = fracPart[:6]
	}
	for len(fracPart) < 6 {
		fracPart += "0"
	}
	frac := int64(0)
	if fracPart != "" {
		frac, err = strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return 0, err
		}
	}
	value := whole*microScale + frac
	if neg {
		value = -value
	}
	return micros(value), nil
}

// parseMicrosAny accepts a json.Number, string or float64.
func parseMicrosAny(v any) (micros, error) {
	switch value := v.(type) {
	case json.Number:
		return parseMicros(value.String())
	case string:
		return parseMicros(value)
	case float64:
		return parseMicros(strconv.FormatFloat(value, 'f', 6, 64))
	case int64:
		return micros(value) * microScale, nil
	case int:
		return micros(value) * microScale, nil
	default:
		return 0, errors.New("unsupported amount type")
	}
}

// StringFixed renders the value with two decimals, rounding half away from zero.
func (m micros) StringFixed() string {
	neg := m < 0
	v := m
	if neg {
		v = -v
	}
	cents := (int64(v) + microScale/20000) / (microScale / 100)
	if cents == 0 && neg {
		neg = false
	}
	whole := cents / 100
	frac := cents % 100
	out := strconv.FormatInt(whole, 10) + "." + pad2(frac)
	if neg {
		out = "-" + out
	}
	return out
}

func pad2(v int64) string {
	if v < 10 {
		return "0" + strconv.FormatInt(v, 10)
	}
	return strconv.FormatInt(v, 10)
}

func (m micros) Mul(n int) micros { return micros(int64(m) * int64(n)) }

// CeilCents rounds up to the next cent and never returns 0.00 for a positive
// amount. Downstream (Dujiao-Next) stores prices at two decimals, so a product
// costing 0.004 USDT would otherwise be exported as "0.00" — a free product.
func (m micros) CeilCents() micros {
	const cent = microScale / 100
	if m <= 0 {
		return 0
	}
	value := (int64(m) + cent - 1) / cent * cent
	if value < cent {
		value = cent
	}
	return micros(value)
}
