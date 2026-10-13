package hetzner

import (
	"errors"
	"testing"
)

func TestLabelSelectorMatchesAsDocumented(t *testing.T) {
	labels := map[string]string{"env": "production", "tier": "web", "elprovo.com/role": "preview"}
	cases := []struct {
		selector string
		want     bool
	}{
		{"env=production", true},
		{"env==production", true},
		{"env!=production", false},
		{"env", true},
		{"!env", false},
		{"!type", true},
		{"env in (testing,staging)", false},
		{"env in (production, staging)", true},
		{"env notin (testing,staging)", true},
		{"env=production,tier!=db", true},
		{"env=production,tier in (db,cache)", false},
		{"elprovo.com/role=preview", true},
		{"missing=value", false},
		{"tier=", false},
	}
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			sel, err := parseSelector(tc.selector)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if got := sel.matches(labels); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMalformedLabelSelectorIsInvalidInput(t *testing.T) {
	for _, selector := range []string{"=value", "bad key=x", "env,,tier", "env in (a,-b)"} {
		t.Run(selector, func(t *testing.T) {
			_, err := parseSelector(selector)

			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.code != "invalid_input" {
				t.Fatalf("parse %q = %v, want invalid_input", selector, err)
			}
		})
	}
}

func TestLabelsFollowTheKeyAndValueRules(t *testing.T) {
	valid := map[string]string{"env": "", "example.com/my": "label", "a_b.c-d": "x.y_z-1"}
	if err := checkLabels(valid); err != nil {
		t.Fatalf("valid labels refused: %v", err)
	}
	for _, labels := range []map[string]string{
		{"hetzner.cloud/x": "y"},
		{"-leading": "x"},
		{"env": "-leading"},
		{"Bad_Prefix/key": "x"},
	} {
		if err := checkLabels(labels); err == nil {
			t.Fatalf("labels %v accepted, want them refused", labels)
		}
	}
}
