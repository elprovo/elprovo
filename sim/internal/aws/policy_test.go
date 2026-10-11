package aws

import "testing"

func TestSessionPolicyEvaluation(t *testing.T) {
	p, err := parsePolicy(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"ecr:*","Resource":"arn:aws:ecr:us-east-1:123456789012:repository/acme/*"},
		{"Effect":"Deny","Action":"ecr:PutImage","Resource":"arn:aws:ecr:us-east-1:123456789012:repository/acme/locked/*"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		action, resource string
		want             bool
	}{
		"allowed by a wildcard":                       {"ecr:BatchGetImage", "arn:aws:ecr:us-east-1:123456789012:repository/acme/web", true},
		"action in another case":                      {"ECR:batchgetimage", "arn:aws:ecr:us-east-1:123456789012:repository/acme/web", true},
		"repository outside the prefix":               {"ecr:BatchGetImage", "arn:aws:ecr:us-east-1:123456789012:repository/acmeco/web", false},
		"denied although allowed":                     {"ecr:PutImage", "arn:aws:ecr:us-east-1:123456789012:repository/acme/locked/web", false},
		"action of another service":                   {"ec2:DescribeVpcs", "arn:aws:ecr:us-east-1:123456789012:repository/acme/web", false},
		"every resource, where it names repositories": {"ecr:GetAuthorizationToken", "*", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.allows(tc.action, tc.resource); got != tc.want {
				t.Fatalf("allows(%s, %s) = %v, want %v", tc.action, tc.resource, got, tc.want)
			}
		})
	}
}
