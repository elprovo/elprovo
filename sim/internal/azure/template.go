package azure

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// templateScope evaluates the expressions of an Azure Resource Manager
// template: the functions that the Packer plugin's templates use, over the
// deployment's parameters and the template's variables. Any other function
// fails, naming it, so a template the simulation cannot run never deploys
// half of its resources.
type templateScope struct {
	sim        *Simulation
	group      *group
	parameters map[string]any
	variables  map[string]any
	evaluated  map[string]any
	evaluating map[string]bool
}

func (t *templateScope) parameter(name string) (any, error) {
	for k, v := range t.parameters {
		if strings.EqualFold(k, name) {
			return v, nil
		}
	}
	return nil, fmt.Errorf("the template parameter '%s' is not found", name)
}

func (t *templateScope) variable(name string) (any, error) {
	key := strings.ToLower(name)
	if v, ok := t.evaluated[key]; ok {
		return v, nil
	}
	for k, raw := range t.variables {
		if !strings.EqualFold(k, name) {
			continue
		}
		if t.evaluating[key] {
			return nil, fmt.Errorf("the template variable '%s' refers to itself", name)
		}
		t.evaluating[key] = true
		v, err := t.eval(raw)
		delete(t.evaluating, key)
		if err != nil {
			return nil, fmt.Errorf("variable '%s': %w", name, err)
		}
		t.evaluated[key] = v
		return v, nil
	}
	return nil, fmt.Errorf("the template variable '%s' is not found", name)
}

// eval evaluates every expression in a template value: strings in brackets,
// inside objects and arrays too.
func (t *templateScope) eval(v any) (any, error) {
	switch x := v.(type) {
	case string:
		if !strings.HasPrefix(x, "[") || !strings.HasSuffix(x, "]") {
			return x, nil
		}
		if strings.HasPrefix(x, "[[") {
			return x[1:], nil
		}
		p := &exprParser{src: x[1 : len(x)-1]}
		e, err := p.parse()
		if err != nil {
			return nil, fmt.Errorf("the template expression '%s' is not valid: %w", x, err)
		}
		return e.eval(t)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			c, err := t.eval(child)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, child := range x {
			c, err := t.eval(child)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	default:
		return v, nil
	}
}

type expr interface {
	eval(t *templateScope) (any, error)
}

type literal struct{ value any }

func (l literal) eval(*templateScope) (any, error) { return l.value, nil }

type call struct {
	name string
	args []expr
}

type member struct {
	target expr
	name   string
}

func (m member) eval(t *templateScope) (any, error) {
	v, err := m.target.eval(t)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cannot read property '%s' of a %T", m.name, v)
	}
	for k, child := range obj {
		if strings.EqualFold(k, m.name) {
			return child, nil
		}
	}
	return nil, fmt.Errorf("the property '%s' does not exist", m.name)
}

func (c call) eval(t *templateScope) (any, error) {
	args := make([]any, 0, len(c.args))
	for _, a := range c.args {
		v, err := a.eval(t)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	str := func(i int) (string, error) {
		if i >= len(args) {
			return "", fmt.Errorf("%s needs at least %d arguments", c.name, i+1)
		}
		s, ok := args[i].(string)
		if !ok {
			return "", fmt.Errorf("argument %d of %s must be a string, not %T", i+1, c.name, args[i])
		}
		return s, nil
	}
	switch strings.ToLower(c.name) {
	case "parameters":
		name, err := str(0)
		if err != nil {
			return nil, err
		}
		return t.parameter(name)
	case "variables":
		name, err := str(0)
		if err != nil {
			return nil, err
		}
		return t.variable(name)
	case "concat":
		return concat(args)
	case "format":
		f, err := str(0)
		if err != nil {
			return nil, err
		}
		for i, a := range args[1:] {
			f = strings.ReplaceAll(f, "{"+strconv.Itoa(i)+"}", fmt.Sprint(a))
		}
		return f, nil
	case "not":
		b, ok := firstArg(args).(bool)
		if !ok {
			return nil, fmt.Errorf("not needs a boolean")
		}
		return !b, nil
	case "empty":
		switch v := firstArg(args).(type) {
		case nil:
			return true, nil
		case string:
			return v == "", nil
		case []any:
			return len(v) == 0, nil
		case map[string]any:
			return len(v) == 0, nil
		}
		return false, nil
	case "equals":
		if len(args) != 2 {
			return nil, fmt.Errorf("equals needs two arguments")
		}
		a, _ := json.Marshal(args[0])
		b, _ := json.Marshal(args[1])
		return string(a) == string(b), nil
	case "if":
		if len(args) != 3 {
			return nil, fmt.Errorf("if needs three arguments")
		}
		cond, ok := args[0].(bool)
		if !ok {
			return nil, fmt.Errorf("the condition of if must be a boolean")
		}
		if cond {
			return args[1], nil
		}
		return args[2], nil
	case "tolower":
		s, err := str(0)
		return strings.ToLower(s), err
	case "toupper":
		s, err := str(0)
		return strings.ToUpper(s), err
	case "resourcegroup":
		return map[string]any{
			"id": t.sim.groupID(t.group.Name), "name": t.group.Name, "type": typeGroup, "location": t.group.Location,
			"tags": t.group.Tags, "properties": map[string]any{"provisioningState": statusSucceeded},
		}, nil
	case "subscription":
		return map[string]any{
			"id": "/subscriptions/" + t.sim.cfg.subscription, "subscriptionId": t.sim.cfg.subscription, "tenantId": t.sim.cfg.tenant, "displayName": "Stagely simulation",
		}, nil
	case "resourceid":
		return t.resourceID(args)
	}
	return nil, fmt.Errorf("the template function '%s' is not supported by the simulation", c.name)
}

func firstArg(args []any) any {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

func concat(args []any) (any, error) {
	if len(args) > 0 {
		if _, ok := args[0].([]any); ok {
			var out []any
			for _, a := range args {
				list, ok := a.([]any)
				if !ok {
					return nil, fmt.Errorf("concat of arrays got a %T", a)
				}
				out = append(out, list...)
			}
			return out, nil
		}
	}
	var b strings.Builder
	for _, a := range args {
		switch v := a.(type) {
		case string:
			b.WriteString(v)
		case float64:
			b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		case bool:
			b.WriteString(strconv.FormatBool(v))
		default:
			return nil, fmt.Errorf("concat of strings got a %T", a)
		}
	}
	return b.String(), nil
}

// resourceID implements resourceId([subscriptionId], [resourceGroupName],
// resourceType, resourceName1, [resourceName2], ...).
func (t *templateScope) resourceID(args []any) (any, error) {
	var parts []string
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("resourceId takes strings, not %T", a)
		}
		parts = append(parts, s)
	}
	typeAt := -1
	for i, p := range parts {
		if strings.Contains(p, "/") {
			typeAt = i
			break
		}
	}
	if typeAt < 0 || typeAt > 2 {
		return nil, fmt.Errorf("resourceId needs a resource type such as Microsoft.Network/virtualNetworks")
	}
	sub, group := t.sim.cfg.subscription, t.group.Name
	switch typeAt {
	case 1:
		group = parts[0]
	case 2:
		sub, group = parts[0], parts[1]
	}
	types := strings.Split(strings.Trim(parts[typeAt], "/"), "/")
	names := parts[typeAt+1:]
	if len(types) < 2 || len(names) != len(types)-1 {
		return nil, fmt.Errorf("resourceId got %d names for the resource type %s", len(names), parts[typeAt])
	}
	id := "/subscriptions/" + sub + "/resourceGroups/" + group + "/providers/" + types[0]
	for i, name := range names {
		id += "/" + types[i+1] + "/" + name
	}
	return id, nil
}

var (
	identPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	numberPattern = regexp.MustCompile(`^-?[0-9]+`)
)

// exprParser parses the expression language of templates: function calls,
// strings in single quotes with ” for a quote, integers, true and false,
// and property access with a dot.
type exprParser struct {
	src string
	pos int
}

func (p *exprParser) parse() (expr, error) {
	e, err := p.expression()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("unexpected %q at %d", p.src[p.pos:], p.pos)
	}
	return e, nil
}

func (p *exprParser) space() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *exprParser) expression() (expr, error) {
	p.space()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("the expression ends early")
	}
	if p.src[p.pos] == '\'' {
		return p.stringLiteral()
	}
	if m := numberPattern.FindString(p.src[p.pos:]); m != "" {
		p.pos += len(m)
		n, _ := strconv.ParseFloat(m, 64)
		return literal{n}, nil
	}
	name := identPattern.FindString(p.src[p.pos:])
	if name == "" {
		return nil, fmt.Errorf("unexpected %q at %d", p.src[p.pos:], p.pos)
	}
	p.pos += len(name)
	switch strings.ToLower(name) {
	case "true":
		return literal{true}, nil
	case "false":
		return literal{false}, nil
	}
	p.space()
	if p.pos >= len(p.src) || p.src[p.pos] != '(' {
		return nil, fmt.Errorf("%s must be called with ()", name)
	}
	p.pos++
	c := call{name: name}
	p.space()
	if p.pos < len(p.src) && p.src[p.pos] == ')' {
		p.pos++
	} else {
		for {
			arg, err := p.expression()
			if err != nil {
				return nil, err
			}
			c.args = append(c.args, arg)
			p.space()
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("%s( is not closed", name)
			}
			if p.src[p.pos] == ')' {
				p.pos++
				break
			}
			if p.src[p.pos] != ',' {
				return nil, fmt.Errorf("unexpected %q in the arguments of %s", p.src[p.pos], name)
			}
			p.pos++
		}
	}
	var e expr = c
	for p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		prop := identPattern.FindString(p.src[p.pos:])
		if prop == "" {
			return nil, fmt.Errorf("a property name must follow '.' at %d", p.pos)
		}
		p.pos += len(prop)
		e = member{target: e, name: prop}
	}
	return e, nil
}

func (p *exprParser) stringLiteral() (expr, error) {
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		ch := p.src[p.pos]
		p.pos++
		if ch != '\'' {
			b.WriteByte(ch)
			continue
		}
		if p.pos < len(p.src) && p.src[p.pos] == '\'' {
			b.WriteByte('\'')
			p.pos++
			continue
		}
		return literal{b.String()}, nil
	}
	return nil, fmt.Errorf("a string is not closed")
}
