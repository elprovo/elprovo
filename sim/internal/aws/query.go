package aws

import (
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
)

// params reads the EC2 Query API's form parameters. Lists are flattened as
// Name.1, Name.2, ...; structures as Name.Field; a params value with a prefix
// reads the parameters under one list member or structure.
type params struct {
	values url.Values
	prefix string
}

func (p params) get(name string) string { return p.values.Get(p.prefix + name) }

func (p params) has(name string) bool {
	key := p.prefix + name
	for k := range p.values {
		if k == key || strings.HasPrefix(k, key+".") {
			return true
		}
	}
	return false
}

// members reads the list name.N, in index order, as one params per member.
func (p params) members(name string) []params {
	key := p.prefix + name + "."
	var indexes []int
	for k := range p.values {
		rest, ok := strings.CutPrefix(k, key)
		if !ok {
			continue
		}
		digits, _, _ := strings.Cut(rest, ".")
		if n, err := strconv.Atoi(digits); err == nil && n > 0 && !slices.Contains(indexes, n) {
			indexes = append(indexes, n)
		}
	}
	slices.Sort(indexes)
	out := make([]params, len(indexes))
	for i, n := range indexes {
		out[i] = params{values: p.values, prefix: key + strconv.Itoa(n) + "."}
	}
	return out
}

// list reads the list of strings name.N, in index order.
func (p params) list(name string) []string {
	var out []string
	for _, m := range p.members(name) {
		out = append(out, m.values.Get(strings.TrimSuffix(m.prefix, ".")))
	}
	return out
}

// boolean reads a boolean parameter; ok is false when it is absent.
func (p params) boolean(name string) (value, ok bool) {
	v := p.get(name)
	return v == "true", v != ""
}

// integer reads an integer parameter, or def when it is absent.
func (p params) integer(name string, def int) (int, error) {
	v := p.get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errInvalidParameter("Invalid value '%s' for %s", v, name)
	}
	return n, nil
}

// unknown returns a top-level parameter that is not in allowed, or "".
func (p params) unknown(allowed []string) string {
	for k := range p.values {
		top, _, _ := strings.Cut(k, ".")
		if !slices.Contains(allowed, top) {
			return k
		}
	}
	return ""
}

// filter is one Filter.N of a Describe action: it matches when any of the
// resource's values for Name matches any of Values, where * and ? are
// wildcards.
type filter struct {
	name   string
	values []string
}

func (p params) filters() []filter {
	var out []filter
	for _, m := range p.members("Filter") {
		out = append(out, filter{name: m.get("Name"), values: m.list("Value")})
	}
	return out
}

// fieldFunc returns a resource's values for a filter name, and whether the
// resource supports the filter.
type fieldFunc func(name string) ([]string, bool)

// matchFilters reports whether a resource passes every filter. A filter the
// resource does not support is an error, as in EC2.
func matchFilters(filters []filter, fields fieldFunc, tags []tag) (bool, error) {
	for _, f := range filters {
		values, supported := tagField(f.name, tags)
		if !supported {
			values, supported = fields(f.name)
		}
		if !supported {
			return false, errInvalidParameter("The filter '%s' is invalid", f.name)
		}
		if !matchAny(values, f.values) {
			return false, nil
		}
	}
	return true, nil
}

func matchAny(values, patterns []string) bool {
	for _, v := range values {
		for _, pattern := range patterns {
			if ok, _ := path.Match(pattern, v); ok || pattern == v {
				return true
			}
		}
	}
	return false
}

func tagField(name string, tags []tag) ([]string, bool) {
	var out []string
	switch {
	case strings.HasPrefix(name, "tag:"):
		for _, t := range tags {
			if t.Key == strings.TrimPrefix(name, "tag:") {
				out = append(out, t.Value)
			}
		}
	case name == "tag-key":
		for _, t := range tags {
			out = append(out, t.Key)
		}
	case name == "tag-value":
		for _, t := range tags {
			out = append(out, t.Value)
		}
	default:
		return nil, false
	}
	return out, true
}

// selectIDs keeps the resources whose ids are in ids; an id that names no
// resource is an error with notFound's code.
func selectIDs[T any](items []T, id func(T) string, ids []string, notFound func(id string) *apiError) ([]T, error) {
	if len(ids) == 0 {
		return items, nil
	}
	var out []T
	for _, want := range ids {
		i := slices.IndexFunc(items, func(item T) bool { return id(item) == want })
		if i < 0 {
			return nil, notFound(want)
		}
		out = append(out, items[i])
	}
	return out, nil
}

func values(v ...string) []string { return v }

func boolString(b bool) []string { return []string{strconv.FormatBool(b)} }
