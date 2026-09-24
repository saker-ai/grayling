package model

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"time"

	"github.com/blang/semver/v4"
)

// isNumber reports whether v carries a numeric value across the encodings
// GoLab sees (JSON float64, YAML int, SDK int/int64/float64/json.Number).
func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return true
	}
	return false
}

// Number coerces v to float64 when it is numeric.
func Number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// isScalar reports whether v is comparable with eq/ne/in semantics.
func isScalar(v any) bool {
	switch v.(type) {
	case bool, string:
		return true
	}
	return isNumber(v)
}

// EqualScalars compares two scalars with type-aware equality: numbers
// compare numerically across encodings; strings and bools compare exactly;
// different kinds are never equal (string "1" != number 1).
func EqualScalars(a, b any) bool {
	if an, ok := Number(a); ok {
		bn, ok := Number(b)
		return ok && an == bn
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}

func validCIDR(s string) bool {
	_, err := netip.ParsePrefix(s)
	return err == nil
}

// MatchCIDR reports whether ip is inside cidr. Invalid inputs never match.
func MatchCIDR(ip, cidr string) bool {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	// Normalize IPv4-in-IPv6 so "10.0.0.1" matches "10.0.0.0/8" regardless
	// of how either side was encoded.
	addr = addr.Unmap()
	return prefix.Masked().Contains(addr)
}

func validSemver(s string) bool {
	_, err := ParseSemver(s)
	return err == nil
}

// ParseSemver accepts strict semver plus the common v-prefixed form.
func ParseSemver(s string) (semver.Version, error) {
	if len(s) > 0 && s[0] == 'v' {
		s = s[1:]
	}
	return semver.Parse(s)
}

// validTimeOperand accepts RFC3339 strings or numeric epoch seconds.
func validTimeOperand(v any) bool {
	if isNumber(v) {
		return true
	}
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// ParseTimeOperand converts a time operand (RFC3339 or epoch seconds) to
// time.Time.
func ParseTimeOperand(v any) (time.Time, bool) {
	if n, ok := Number(v); ok {
		sec := int64(n)
		nsec := int64((n - float64(sec)) * float64(time.Second))
		return time.Unix(sec, nsec).UTC(), true
	}
	if s, ok := v.(string); ok {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

// FormatRevision renders an int64 revision the way it appears in dataIds and
// flag Version fields ("r<revision>" / version string).
func FormatRevision(rev int64) string {
	return strconv.FormatInt(rev, 10)
}
