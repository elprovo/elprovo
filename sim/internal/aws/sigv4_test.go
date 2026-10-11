package aws

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The request of the AWS Signature Version 4 test suite case
// post-x-www-form-urlencoded, as botocore ships it in
// tests/unit/auth/aws4_testsuite, with the signature AWS computed.
const (
	suiteAccessKey     = "AKIDEXAMPLE"
	suiteSecretKey     = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteDate          = "20150830T123600Z"
	suiteAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"
)

func suiteSimulation(t *testing.T, secret string, now time.Time) *Simulation {
	t.Helper()
	cfg := testConfig()
	cfg.accessKeyID, cfg.secretAccessKey = suiteAccessKey, secret
	s, err := newSimulation(cfg, "", newFakeMachines(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	return s
}

func suiteRequest(t *testing.T, authorization string) *http.Request {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.amazonaws.com/", strings.NewReader("Param1=value1"))
	r.Host = "example.amazonaws.com"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Amz-Date", suiteDate)
	r.Header.Set("Authorization", authorization)
	return r
}

func TestSignatureFromTheAWSTestSuiteIsAccepted(t *testing.T) {
	signedAt, _ := time.Parse(amzDateFormat, suiteDate)
	s := suiteSimulation(t, suiteSecretKey, signedAt.Add(time.Minute))

	sc, failure := s.verifySignature(suiteRequest(t, suiteAuthorization), []byte("Param1=value1"))

	if failure != 0 || sc != (scope{region: "us-east-1", service: "service"}) {
		t.Fatalf("verify = %+v, failure %d; want the scope us-east-1/service", sc, failure)
	}
}

func TestSignatureIsRefusedWhenAnythingSignedChanges(t *testing.T) {
	signedAt, _ := time.Parse(amzDateFormat, suiteDate)
	for name, tc := range map[string]struct {
		secret        string
		now           time.Time
		body          string
		authorization string
		want          authFailure
	}{
		"other secret":      {"wrong-secret", signedAt, "Param1=value1", suiteAuthorization, authBadSignature},
		"changed body":      {suiteSecretKey, signedAt, "Param1=value2", suiteAuthorization, authBadSignature},
		"unknown key":       {suiteSecretKey, signedAt, "Param1=value1", strings.Replace(suiteAuthorization, "AKIDEXAMPLE", "AKIDOTHER", 1), authUnknownKey},
		"16 minutes later":  {suiteSecretKey, signedAt.Add(16 * time.Minute), "Param1=value1", suiteAuthorization, authExpired},
		"no signature":      {suiteSecretKey, signedAt, "Param1=value1", "", authMissing},
		"other algorithm":   {suiteSecretKey, signedAt, "Param1=value1", strings.Replace(suiteAuthorization, "AWS4-HMAC-SHA256", "AWS4-HMAC-SHA1", 1), authIncomplete},
		"host not signed":   {suiteSecretKey, signedAt, "Param1=value1", strings.Replace(suiteAuthorization, "content-type;host;x-amz-date", "content-type;x-amz-date", 1), authIncomplete},
		"scope of past day": {suiteSecretKey, signedAt, "Param1=value1", strings.Replace(suiteAuthorization, "20150830/", "20150829/", 1), authIncomplete},
	} {
		t.Run(name, func(t *testing.T) {
			s := suiteSimulation(t, tc.secret, tc.now)
			r := suiteRequest(t, tc.authorization)
			if tc.authorization == "" {
				r.Header.Del("Authorization")
			}

			_, failure := s.verifySignature(r, []byte(tc.body))

			if failure != tc.want {
				t.Fatalf("failure = %d, want %d", failure, tc.want)
			}
		})
	}
}

func TestEachServiceReportsAWrongSecretInItsOwnFormat(t *testing.T) {
	e := newEnv(t)
	tamper := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", r.Header.Get("Authorization")[:len(r.Header.Get("Authorization"))-4]+"0000")
		return r
	}

	status, body := e.do(tamper(e.signedRequest("ec2", testRegion, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, "Action=DescribeVpcs&Version=2016-11-15")))
	var ec2Err struct {
		Code string `xml:"Errors>Error>Code"`
	}
	if err := xml.Unmarshal(body, &ec2Err); err != nil || status != http.StatusForbidden || ec2Err.Code != "SignatureDoesNotMatch" {
		t.Errorf("EC2 = %d %s", status, body)
	}

	status, body = e.do(tamper(e.signedRequest("sts", testRegion, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, "Action=GetCallerIdentity&Version=2011-06-15")))
	var stsErr struct {
		Type string `xml:"Error>Type"`
		Code string `xml:"Error>Code"`
	}
	if err := xml.Unmarshal(body, &stsErr); err != nil || status != http.StatusForbidden || stsErr.Code != "SignatureDoesNotMatch" || stsErr.Type != "Sender" {
		t.Errorf("STS = %d %s", status, body)
	}

	status, body = e.do(tamper(e.signedRequest("ecr", testRegion, http.Header{"Content-Type": {"application/x-amz-json-1.1"}, "X-Amz-Target": {ecrTargetPrefix + "DescribeRepositories"}}, "{}")))
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"__type":"InvalidSignatureException"`) {
		t.Errorf("ECR = %d %s", status, body)
	}
}

func TestRequestSignedForARegionTheSimulationDoesNotServeIsRefused(t *testing.T) {
	e := newEnv(t)

	status, body := e.ec2In("ap-south-1", "DescribeVpcs")

	if status != http.StatusUnauthorized || !strings.Contains(string(body), "<Code>AuthFailure</Code>") {
		t.Fatalf("DescribeVpcs in ap-south-1 = %d %s, want AuthFailure", status, body)
	}
}
