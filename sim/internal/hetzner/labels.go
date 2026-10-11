package hetzner

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9]([-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9])?$`)
	labelValuePattern = regexp.MustCompile(`^([a-zA-Z0-9]([-_.a-zA-Z0-9]{0,61}[a-zA-Z0-9])?)?$`)
	dnsLabelPattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	setPattern        = regexp.MustCompile(`^(\S+)\s+(in|notin)\s*\((.*)\)$`)
)

// checkLabels validates labels as Hetzner documents them: a key is an
// optional DNS subdomain prefix and a slash, then a name of up to 63
// characters; a value is empty or up to 63 characters; the hetzner.cloud/
// prefix is reserved.
func checkLabels(labels map[string]string) error {
	for k, v := range labels {
		if err := checkLabelKey(k); err != nil {
			return errInvalidInput("labels", "%v", err)
		}
		if !labelValuePattern.MatchString(v) {
			return errInvalidInput("labels", "label value %q of %q is invalid", v, k)
		}
	}
	return nil
}

func checkLabelKey(key string) error {
	name := key
	if prefix, rest, ok := strings.Cut(key, "/"); ok {
		name = rest
		if prefix == "hetzner.cloud" || strings.HasSuffix(prefix, ".hetzner.cloud") {
			return fmt.Errorf("label key %q uses the reserved prefix hetzner.cloud/", key)
		}
		if len(prefix) > 253 || !allMatch(strings.Split(prefix, "."), dnsLabelPattern) {
			return fmt.Errorf("label key prefix %q is not a DNS subdomain", prefix)
		}
	}
	if !labelNamePattern.MatchString(name) {
		return fmt.Errorf("label key %q is invalid", key)
	}
	return nil
}

func allMatch(parts []string, re *regexp.Regexp) bool {
	for _, p := range parts {
		if !re.MatchString(p) {
			return false
		}
	}
	return true
}

// selector is a parsed label selector: every requirement must hold.
type selector []requirement

type requirement struct {
	key    string
	op     string // exists, !exists, =, !=, in, notin
	values []string
}

// parseSelector parses Hetzner's label selector language: k==v, k=v, k!=v,
// k, !k, k in (v1,v2) and k notin (v1,v2), joined by commas.
func parseSelector(text string) (selector, error) {
	var sel selector
	for _, part := range splitSelector(text) {
		part = strings.TrimSpace(part)
		req, err := parseRequirement(part)
		if err != nil {
			return nil, errInvalidInput("label_selector", "%v", err)
		}
		sel = append(sel, req)
	}
	return sel, nil
}

// splitSelector splits at commas outside parentheses.
func splitSelector(text string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range text {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, text[start:i])
			start = i + 1
		}
	}
	return append(parts, text[start:])
}

func parseRequirement(part string) (requirement, error) {
	var req requirement
	switch {
	case part == "":
		return req, fmt.Errorf("empty requirement")
	case strings.HasPrefix(part, "!") && !strings.Contains(part, "="):
		req = requirement{key: strings.TrimSpace(part[1:]), op: "!exists"}
	case setPattern.MatchString(part):
		m := setPattern.FindStringSubmatch(part)
		req = requirement{key: m[1], op: m[2]}
		for _, v := range strings.Split(m[3], ",") {
			req.values = append(req.values, strings.TrimSpace(v))
		}
	case strings.Contains(part, "!="):
		k, v, _ := strings.Cut(part, "!=")
		req = requirement{key: strings.TrimSpace(k), op: "!=", values: []string{strings.TrimSpace(v)}}
	case strings.Contains(part, "=="):
		k, v, _ := strings.Cut(part, "==")
		req = requirement{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}}
	case strings.Contains(part, "="):
		k, v, _ := strings.Cut(part, "=")
		req = requirement{key: strings.TrimSpace(k), op: "=", values: []string{strings.TrimSpace(v)}}
	default:
		req = requirement{key: part, op: "exists"}
	}
	if err := checkLabelKey(req.key); err != nil {
		return req, err
	}
	for _, v := range req.values {
		if !labelValuePattern.MatchString(v) {
			return req, fmt.Errorf("label value %q is invalid", v)
		}
	}
	return req, nil
}

func (sel selector) matches(labels map[string]string) bool {
	for _, req := range sel {
		v, has := labels[req.key]
		ok := false
		switch req.op {
		case "exists":
			ok = has
		case "!exists":
			ok = !has
		case "=":
			ok = has && v == req.values[0]
		case "!=":
			ok = !has || v != req.values[0]
		case "in":
			ok = has && slices.Contains(req.values, v)
		case "notin":
			ok = !has || !slices.Contains(req.values, v)
		}
		if !ok {
			return false
		}
	}
	return true
}

// selectorFilter returns a filter for the request's label_selector
// parameter; without one, everything matches.
func selectorFilter(query map[string][]string) (func(map[string]string) bool, error) {
	values := query["label_selector"]
	if len(values) == 0 || values[0] == "" {
		return func(map[string]string) bool { return true }, nil
	}
	sel, err := parseSelector(values[0])
	if err != nil {
		return nil, err
	}
	return sel.matches, nil
}
