package aws

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// policy is the session policy of a federated session. The simulation's IAM
// user may do everything, so the intersection of its permissions and the
// session policy is the session policy itself.
type policy struct {
	Statements []statement `json:"statements"`
}

type statement struct {
	Effect    string   `json:"effect"`
	Actions   []string `json:"actions"`
	Resources []string `json:"resources"`
}

// policyDocument is an IAM policy as JSON: Statement is one statement or a
// list of them.
type policyDocument struct {
	Version   string          `json:"Version"`
	ID        string          `json:"Id"`
	Statement json.RawMessage `json:"Statement"`
}

type statementDocument struct {
	Sid      string     `json:"Sid"`
	Effect   string     `json:"Effect"`
	Action   stringList `json:"Action"`
	Resource stringList `json:"Resource"`
}

// stringList is a policy element that is a string or a list of strings.
type stringList []string

func (l *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*l = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("must be a string or a list of strings")
	}
	*l = many
	return nil
}

// parsePolicy reads an IAM policy document. It refuses the elements that the
// simulation does not evaluate, such as Condition, NotAction and Principal,
// so that a session never gets more than its policy says.
func parsePolicy(doc string) (policy, error) {
	var d policyDocument
	if err := decodeStrict([]byte(doc), &d); err != nil {
		return policy{}, err
	}
	if d.Version != "2012-10-17" && d.Version != "2008-10-17" {
		return policy{}, fmt.Errorf("the policy version %q is not 2012-10-17", d.Version)
	}
	raw := []json.RawMessage{d.Statement}
	if trimmed := bytes.TrimSpace(d.Statement); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return policy{}, err
		}
	}
	var p policy
	for _, r := range raw {
		var s statementDocument
		if err := decodeStrict(r, &s); err != nil {
			return policy{}, err
		}
		if s.Effect != "Allow" && s.Effect != "Deny" {
			return policy{}, fmt.Errorf("the effect %q is not Allow or Deny", s.Effect)
		}
		if len(s.Action) == 0 || len(s.Resource) == 0 {
			return policy{}, errors.New("every statement needs an Action and a Resource")
		}
		p.Statements = append(p.Statements, statement{Effect: s.Effect, Actions: s.Action, Resources: s.Resource})
	}
	return p, nil
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w (the simulation evaluates only Version, Statement, Sid, Effect, Action and Resource)", err)
	}
	if dec.More() {
		return errors.New("the policy has data after its end")
	}
	return nil
}

// allows reports whether the policy allows the action, such as
// ecr:BatchGetImage, on the resource ARN: a statement allows it and none
// denies it. Actions match without regard to case, resources with it, and
// both may use the wildcards * and ?.
func (p *policy) allows(action, resource string) bool {
	allowed := false
	for _, s := range p.Statements {
		if !slices.ContainsFunc(s.Actions, func(a string) bool { return wildcardMatch(strings.ToLower(a), strings.ToLower(action)) }) ||
			!slices.ContainsFunc(s.Resources, func(r string) bool { return wildcardMatch(r, resource) }) {
			continue
		}
		if s.Effect == "Deny" {
			return false
		}
		allowed = true
	}
	return allowed
}

// allowsAll reports whether the policy allows every one of the actions on
// the resource.
func (p *policy) allowsAll(actions []string, resource string) bool {
	return !slices.ContainsFunc(actions, func(a string) bool { return !p.allows(a, resource) })
}

// wildcardMatch reports whether s matches pattern, in which * matches any
// run of characters and ? any one character.
func wildcardMatch(pattern, s string) bool {
	expr := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
	return regexp.MustCompile("^(?s:" + expr + ")$").MatchString(s)
}
