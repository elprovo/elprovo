package stripe

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type params struct {
	value    string
	hasValue bool
	children map[string]*params
}

func parseParams(form url.Values) *params {
	root := &params{}
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		path := splitKey(k)
		for _, v := range form[k] {
			root.set(path, v)
		}
	}
	return root
}

func splitKey(key string) []string {
	open := strings.IndexByte(key, '[')
	if open <= 0 || !strings.HasSuffix(key, "]") {
		return []string{key}
	}
	parts := []string{key[:open]}
	for rest := key[open:]; rest != ""; {
		end := strings.IndexByte(rest, ']')
		if rest[0] != '[' || end < 0 {
			return []string{key}
		}
		parts = append(parts, rest[1:end])
		rest = rest[end+1:]
	}
	return parts
}

func (p *params) set(path []string, value string) {
	node := p
	for _, seg := range path {
		if node.children == nil {
			node.children = map[string]*params{}
		}
		if seg == "" {
			seg = strconv.Itoa(len(node.children))
		}
		child := node.children[seg]
		if child == nil {
			child = &params{}
			node.children[seg] = child
		}
		node = child
	}
	if !node.hasValue {
		node.value, node.hasValue = value, true
	}
}

func (p *params) get(key string) *params {
	if p == nil {
		return nil
	}
	return p.children[key]
}

func (p *params) str(key string) string {
	return p.get(key).scalar()
}

func (p *params) scalar() string {
	if p == nil {
		return ""
	}
	return p.value
}

func (p *params) optional(key string) *string {
	c := p.get(key)
	if c == nil || !c.hasValue {
		return nil
	}
	v := c.value
	return &v
}

func (p *params) list() []*params {
	if p == nil {
		return nil
	}
	type entry struct {
		index int
		node  *params
	}
	var entries []entry
	for k, c := range p.children {
		if i, err := strconv.Atoi(k); err == nil {
			entries = append(entries, entry{i, c})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.index - b.index })
	out := make([]*params, len(entries))
	for i, e := range entries {
		out[i] = e.node
	}
	return out
}

func (p *params) strings(key string) []string {
	c := p.get(key)
	if c == nil {
		return nil
	}
	if len(c.children) == 0 {
		if c.value == "" {
			return nil
		}
		return []string{c.value}
	}
	var out []string
	for _, item := range c.list() {
		if item.value != "" {
			out = append(out, item.value)
		}
	}
	return out
}

func (p *params) integer(key string) (int64, bool, error) {
	c := p.get(key)
	if c == nil {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(c.value, 10, 64)
	if err != nil {
		return 0, false, invalidInteger(key, c.value)
	}
	return n, true, nil
}

func (p *params) boolean(key string) (bool, bool, error) {
	c := p.get(key)
	if c == nil {
		return false, false, nil
	}
	b, err := strconv.ParseBool(c.value)
	if err != nil {
		return false, false, invalidRequest(key, "Invalid boolean: %s", c.value)
	}
	return b, true, nil
}

type metadataUpdate struct {
	clear bool
	set   map[string]string
}

func (p *params) metadata(key string) metadataUpdate {
	c := p.get(key)
	if c == nil {
		return metadataUpdate{}
	}
	u := metadataUpdate{clear: c.hasValue && c.value == "", set: map[string]string{}}
	for k, v := range c.children {
		u.set[k] = v.value
	}
	return u
}

func (u metadataUpdate) apply(current map[string]string) map[string]string {
	out := map[string]string{}
	if !u.clear {
		for k, v := range current {
			out[k] = v
		}
	}
	for k, v := range u.set {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
