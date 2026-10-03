package models

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Validate checks a value given to a variable against the variable's rules,
// the Laravel validation expression of the egg it came from
// ("required|string|max:20", "regex:/^([\w\d._-]+)(\.jar)$/"). Pterodactyl
// and Pelican check every value they are given against it; Quetzal kept the
// rules and checked only required and the choice list, so a value the egg
// forbids -- a jar name with a space in it, a version that is not one --
// was accepted and the server failed at its next start instead.
//
// It covers the rules eggs use. Others are let through, as is a pattern RE2
// cannot read (lookarounds, backreferences): a check that cannot be made is
// not made, rather than refusing a value the egg accepts. An empty value is
// only checked against required: the other rules apply to a value that is
// there, as with nullable in Laravel.
func (v TemplateVariable) Validate(value string) error {
	rules := splitRules(v.Rules)
	if strings.TrimSpace(value) == "" {
		if v.Required || slices.Contains(rules, "required") {
			return fmt.Errorf("variable %q is required", v.EnvVariable)
		}
		return nil
	}
	numericRules := slices.ContainsFunc(rules, func(r string) bool { return r == "integer" || r == "numeric" })
	for _, rule := range rules {
		name, arg, _ := strings.Cut(rule, ":")
		if err := checkRule(strings.TrimSpace(name), arg, value, numericRules); err != nil {
			return fmt.Errorf("variable %q %s", v.EnvVariable, err.Error())
		}
	}
	return nil
}

// splitRules splits a rule expression on its pipes, but not on those inside a
// regex rule's pattern ("regex:/^(a|b)$/"), which Pelican's list form allows
// and which the list arrives joined by.
func splitRules(expr string) []string {
	var out []string
	parts := strings.Split(expr, "|")
	for i := 0; i < len(parts); i++ {
		p := strings.TrimSpace(parts[i])
		if strings.HasPrefix(p, "regex:") || strings.HasPrefix(p, "not_regex:") {
			_, pat, _ := strings.Cut(p, ":")
			for !regexClosed(pat) && i+1 < len(parts) {
				i++
				p += "|" + parts[i]
				_, pat, _ = strings.Cut(p, ":")
			}
		}
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// regexClosed reports whether a PHP pattern ("/…/flags") has its closing
// delimiter.
func regexClosed(pat string) bool {
	if len(pat) < 2 {
		return false
	}
	d := pat[0]
	end := strings.LastIndexByte(pat, d)
	if end <= 0 {
		return false
	}
	return strings.Trim(pat[end+1:], "imsxuADSUXJ") == ""
}

// phpRegexp compiles a PHP pattern ("/…/flags") for RE2, or reports that it
// cannot.
func phpRegexp(pat string) (*regexp.Regexp, bool) {
	if !regexClosed(pat) {
		return nil, false
	}
	d := pat[0]
	end := strings.LastIndexByte(pat, d)
	body, flags := pat[1:end], pat[end+1:]
	prefix := ""
	for _, f := range flags {
		switch f {
		case 'i', 'm', 's':
			prefix += string(f)
		case 'u', 'D':
			// RE2 is UTF-8 throughout, and $ matches at the very end without m.
		default:
			return nil, false
		}
	}
	if prefix != "" {
		body = "(?" + prefix + ")" + body
	}
	re, err := regexp.Compile(body)
	return re, err == nil
}

func checkRule(name, arg, value string, numeric bool) error {
	size := func() (float64, bool) {
		if numeric {
			f, err := strconv.ParseFloat(value, 64)
			return f, err == nil
		}
		return float64(utf8.RuneCountInString(value)), true
	}
	unit := func() string {
		if numeric {
			return ""
		}
		return " characters"
	}
	switch name {
	case "integer":
		if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
			return fmt.Errorf("must be a whole number")
		}
	case "numeric":
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
			return fmt.Errorf("must be a number")
		}
	case "boolean":
		if !slices.Contains([]string{"true", "false", "1", "0"}, strings.ToLower(value)) {
			return fmt.Errorf("must be true or false (or 1 or 0)")
		}
	case "in":
		if opts := ruleList(arg); !slices.Contains(opts, value) {
			return fmt.Errorf("must be one of %s", strings.Join(opts, ", "))
		}
	case "not_in":
		if slices.Contains(ruleList(arg), value) {
			return fmt.Errorf("cannot be %q", value)
		}
	case "regex", "not_regex":
		re, ok := phpRegexp(arg)
		if !ok {
			return nil
		}
		if re.MatchString(value) != (name == "regex") {
			if name == "regex" {
				return fmt.Errorf("must match %s", arg)
			}
			return fmt.Errorf("must not match %s", arg)
		}
	case "max", "min", "size":
		limit, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return nil
		}
		n, ok := size()
		if !ok {
			return fmt.Errorf("must be a number")
		}
		switch {
		case name == "max" && n > limit:
			return fmt.Errorf("must be at most %s%s", arg, unit())
		case name == "min" && n < limit:
			return fmt.Errorf("must be at least %s%s", arg, unit())
		case name == "size" && n != limit:
			return fmt.Errorf("must be exactly %s%s", arg, unit())
		}
	case "between":
		bounds := ruleList(arg)
		if len(bounds) != 2 {
			return nil
		}
		lo, err1 := strconv.ParseFloat(bounds[0], 64)
		hi, err2 := strconv.ParseFloat(bounds[1], 64)
		if err1 != nil || err2 != nil {
			return nil
		}
		n, ok := size()
		if !ok {
			return fmt.Errorf("must be a number")
		}
		if n < lo || n > hi {
			return fmt.Errorf("must be between %s and %s%s", bounds[0], bounds[1], unit())
		}
	case "digits", "digits_between":
		if strings.TrimLeft(value, "0123456789") != "" {
			return fmt.Errorf("must be digits only")
		}
		bounds := ruleList(arg)
		if len(bounds) == 0 {
			return nil
		}
		lo, err := strconv.Atoi(bounds[0])
		if err != nil {
			return nil
		}
		hi := lo
		if name == "digits_between" && len(bounds) == 2 {
			if hi, err = strconv.Atoi(bounds[1]); err != nil {
				return nil
			}
		}
		if len(value) < lo || len(value) > hi {
			if lo == hi {
				return fmt.Errorf("must be %d digits", lo)
			}
			return fmt.Errorf("must be %d to %d digits", lo, hi)
		}
	case "alpha":
		if strings.IndexFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) }) >= 0 {
			return fmt.Errorf("must be letters only")
		}
	case "alpha_num":
		if strings.IndexFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r) }) >= 0 {
			return fmt.Errorf("must be letters and digits only")
		}
	case "alpha_dash":
		if strings.IndexFunc(value, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsNumber(r) && r != '-' && r != '_'
		}) >= 0 {
			return fmt.Errorf("must be letters, digits, dashes and underscores only")
		}
	case "url":
		if u, err := url.Parse(value); err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("must be a URL")
		}
	case "email":
		if _, err := mail.ParseAddress(value); err != nil {
			return fmt.Errorf("must be an email address")
		}
	case "ip":
		if net.ParseIP(value) == nil {
			return fmt.Errorf("must be an IP address")
		}
	case "ipv4":
		if ip := net.ParseIP(value); ip == nil || ip.To4() == nil {
			return fmt.Errorf("must be an IPv4 address")
		}
	case "ipv6":
		if ip := net.ParseIP(value); ip == nil || ip.To4() != nil {
			return fmt.Errorf("must be an IPv6 address")
		}
	}
	return nil
}

func ruleList(arg string) []string {
	var out []string
	for _, o := range strings.Split(arg, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}
