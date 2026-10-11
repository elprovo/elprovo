package aws

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// tokenOnlyPolicy lets a session get ECR authorization tokens and nothing
// else.
const tokenOnlyPolicy = `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"ecr:GetAuthorizationToken","Resource":"*"}}`

var stsHeader = http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}

// sts calls an STS action with creds and the form parameters, given as
// alternating names and values.
func (e *testEnv) sts(creds testCredentials, action string, params ...string) (int, []byte) {
	e.t.Helper()
	form := url.Values{"Action": {action}, "Version": {"2011-06-15"}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Add(params[i], params[i+1])
	}
	return e.do(e.signedRequestAs(creds, "sts", testRegion, stsHeader, form.Encode()))
}

type federationTokenResponse struct {
	AccessKeyID     string `xml:"GetFederationTokenResult>Credentials>AccessKeyId"`
	SecretAccessKey string `xml:"GetFederationTokenResult>Credentials>SecretAccessKey"`
	SessionToken    string `xml:"GetFederationTokenResult>Credentials>SessionToken"`
	Expiration      string `xml:"GetFederationTokenResult>Credentials>Expiration"`
	Arn             string `xml:"GetFederationTokenResult>FederatedUser>Arn"`
}

// federationToken gets temporary credentials of the federated user name
// with the session policy, as the IAM user.
func (e *testEnv) federationToken(name, policy string, params ...string) (testCredentials, federationTokenResponse) {
	e.t.Helper()
	status, body := e.sts(userCredentials, "GetFederationToken", append([]string{"Name", name, "Policy", policy}, params...)...)
	var got federationTokenResponse
	if err := xml.Unmarshal(body, &got); status != http.StatusOK || err != nil {
		e.t.Fatalf("GetFederationToken = %d %s", status, body)
	}
	return testCredentials{got.AccessKeyID, got.SecretAccessKey, got.SessionToken}, got
}

func stsErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		Code string `xml:"Error>Code"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("STS error %s: %v", body, err)
	}
	return got.Code
}

func TestFederationTokenGivesCredentialsThatExpireAfterTheRequestedDuration(t *testing.T) {
	e := newEnv(t)
	account := e.callerAccount()

	creds, got := e.federationToken("stagely-registry-pull", tokenOnlyPolicy, "DurationSeconds", "900")

	expires, err := time.Parse(time.RFC3339, got.Expiration)
	if err != nil || expires.Sub(e.clock.Now()).Round(time.Second) != 15*time.Minute {
		t.Fatalf("Expiration = %q, want 15 minutes from now", got.Expiration)
	}
	if !strings.HasPrefix(creds.accessKeyID, "ASIA") || creds.secretAccessKey == "" || creds.sessionToken == "" ||
		got.Arn != "arn:aws:sts::"+account+":federated-user/stagely-registry-pull" {
		t.Fatalf("GetFederationToken = %+v", got)
	}
	status, body := e.sts(creds, "GetCallerIdentity")
	var identity struct {
		Arn string `xml:"GetCallerIdentityResult>Arn"`
	}
	if err := xml.Unmarshal(body, &identity); status != http.StatusOK || err != nil || identity.Arn != got.Arn {
		t.Fatalf("GetCallerIdentity with the session = %d %s, want the federated user", status, body)
	}
	e.clock.advance(15 * time.Minute)
	if status, out := e.ecrAs(creds, "GetAuthorizationToken", map[string]any{}); status != http.StatusBadRequest || out["__type"] != "ExpiredTokenException" {
		t.Fatalf("GetAuthorizationToken after the session expired = %d %+v, want ExpiredTokenException", status, out)
	}
}

func TestSessionCredentialsMayDoOnlyWhatTheSessionPolicyAllows(t *testing.T) {
	e := newEnv(t)
	creds, _ := e.federationToken("stagely-registry-pull", tokenOnlyPolicy)

	if status, out := e.ecrAs(creds, "GetAuthorizationToken", map[string]any{}); status != http.StatusOK {
		t.Fatalf("GetAuthorizationToken, which the policy allows = %d %+v", status, out)
	}
	if status, out := e.ecrAs(creds, "CreateRepository", map[string]any{"repositoryName": "acme/web"}); status != http.StatusBadRequest || out["__type"] != "AccessDeniedException" {
		t.Errorf("CreateRepository, which the policy does not allow = %d %+v, want AccessDeniedException", status, out)
	}
	form := url.Values{"Action": {"DescribeVpcs"}, "Version": {"2016-11-15"}}.Encode()
	status, body := e.do(e.signedRequestAs(creds, "ec2", testRegion, stsHeader, form))
	if status != http.StatusForbidden || !strings.Contains(string(body), "<Code>UnauthorizedOperation</Code>") {
		t.Errorf("DescribeVpcs, which the policy does not allow = %d %s, want UnauthorizedOperation", status, body)
	}
	if status, body := e.sts(creds, "GetFederationToken", "Name", "nested", "Policy", tokenOnlyPolicy); status != http.StatusForbidden || stsErrorCode(t, body) != "AccessDenied" {
		t.Errorf("GetFederationToken with session credentials = %d %s, want AccessDenied", status, body)
	}
}

func TestSessionCredentialsNeedTheirOwnSessionToken(t *testing.T) {
	e := newEnv(t)
	creds, _ := e.federationToken("stagely-registry-pull", tokenOnlyPolicy)
	other, _ := e.federationToken("stagely-registry-push", tokenOnlyPolicy)

	for name, token := range map[string]string{"no token": "", "another session's token": other.sessionToken} {
		creds.sessionToken = token
		if status, out := e.ecrAs(creds, "GetAuthorizationToken", map[string]any{}); status != http.StatusBadRequest || out["__type"] != "UnrecognizedClientException" {
			t.Errorf("%s: GetAuthorizationToken = %d %+v, want UnrecognizedClientException", name, status, out)
		}
	}
}

func TestFederationTokenRefusesInputOutsideItsLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		params []string
		want   string
	}{
		"name too short":          {[]string{"Name", "a", "Policy", tokenOnlyPolicy}, "ValidationError"},
		"name with a space":       {[]string{"Name", "stagely pull", "Policy", tokenOnlyPolicy}, "ValidationError"},
		"shorter than 15 minutes": {[]string{"Name", "stagely", "Policy", tokenOnlyPolicy, "DurationSeconds", "899"}, "ValidationError"},
		"longer than 36 hours":    {[]string{"Name", "stagely", "Policy", tokenOnlyPolicy, "DurationSeconds", "129601"}, "ValidationError"},
		"policy over 2048 chars":  {[]string{"Name", "stagely", "Policy", tokenOnlyPolicy + strings.Repeat(" ", 2049-len(tokenOnlyPolicy))}, "ValidationError"},
		"policy with a condition": {[]string{"Name", "stagely", "Policy", `{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"*","Resource":"*","Condition":{}}}`}, "MalformedPolicyDocument"},
		"policy that is not JSON": {[]string{"Name", "stagely", "Policy", "allow everything"}, "MalformedPolicyDocument"},
		"managed session policy":  {[]string{"Name", "stagely", "PolicyArns.member.1.arn", "arn:aws:iam::aws:policy/AdministratorAccess"}, "UnknownParameter"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)

			status, body := e.sts(userCredentials, "GetFederationToken", tc.params...)

			if status != http.StatusBadRequest || stsErrorCode(t, body) != tc.want {
				t.Fatalf("GetFederationToken = %d %s, want %s", status, body, tc.want)
			}
		})
	}
}
