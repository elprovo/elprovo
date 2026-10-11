package aws

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationTokenIsANewPasswordForTheUserAWSThatExpiresIn12Hours(t *testing.T) {
	e := newEnv(t)

	status, out := e.ecr("GetAuthorizationToken", map[string]any{})

	data := out["authorizationData"].([]any)[0].(map[string]any)
	expires := time.Unix(int64(data["expiresAt"].(float64)), 0)
	account := e.callerAccount()
	user, password := e.authorizationToken(out)
	if status != http.StatusOK || user != "AWS" || password == "" || data["proxyEndpoint"] != "https://"+account+".dkr.ecr.us-east-1.amazonaws.com" {
		t.Fatalf("authorization data = %d %+v", status, data)
	}
	if d := expires.Sub(e.clock.Now()); d < 12*time.Hour-time.Second || d > 12*time.Hour+time.Second {
		t.Fatalf("token expires in %v, want 12h", d)
	}
	if _, again := e.authorizationToken(e.mustECR("GetAuthorizationToken")); again == password {
		t.Fatal("a second GetAuthorizationToken returned the same password; want a new one for each call")
	}
}

// mustECR calls an ECR operation without input that must succeed.
func (e *testEnv) mustECR(operation string) map[string]any {
	e.t.Helper()
	status, out := e.ecr(operation, map[string]any{})
	if status != http.StatusOK {
		e.t.Fatalf("%s = %d %+v", operation, status, out)
	}
	return out
}

// authorizationToken returns the user and password that the
// GetAuthorizationToken answer carries.
func (e *testEnv) authorizationToken(out map[string]any) (string, string) {
	e.t.Helper()
	data, ok := out["authorizationData"].([]any)
	if !ok || len(data) != 1 {
		e.t.Fatalf("GetAuthorizationToken answered %+v, want one authorization", out)
	}
	decoded, err := base64.StdEncoding.DecodeString(data[0].(map[string]any)["authorizationToken"].(string))
	if err != nil {
		e.t.Fatalf("authorization token: %v", err)
	}
	user, password, _ := strings.Cut(string(decoded), ":")
	return user, password
}

// callerAccount returns the account id that STS GetCallerIdentity reports.
func (e *testEnv) callerAccount() string {
	e.t.Helper()
	header := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	status, body := e.do(e.signedRequest("sts", testRegion, header, "Action=GetCallerIdentity&Version=2011-06-15"))
	var got struct {
		Account string `xml:"GetCallerIdentityResult>Account"`
		Arn     string `xml:"GetCallerIdentityResult>Arn"`
	}
	if err := xml.Unmarshal(body, &got); status != http.StatusOK || err != nil || len(got.Account) != 12 || got.Arn != "arn:aws:iam::"+got.Account+":user/stagely-sim" {
		e.t.Fatalf("GetCallerIdentity = %d %s", status, body)
	}
	return got.Account
}

func TestRepositoryLifecycle(t *testing.T) {
	e := newEnv(t)
	account := e.callerAccount()

	status, created := e.ecr("CreateRepository", map[string]any{"repositoryName": "acme/web"})
	repo := created["repository"].(map[string]any)
	if status != http.StatusOK || repo["repositoryUri"] != account+".dkr.ecr.us-east-1.amazonaws.com/acme/web" || repo["imageTagMutability"] != "MUTABLE" {
		t.Fatalf("CreateRepository = %d %+v", status, created)
	}
	if status, out := e.ecr("CreateRepository", map[string]any{"repositoryName": "acme/web"}); status != http.StatusBadRequest || out["__type"] != "RepositoryAlreadyExistsException" {
		t.Fatalf("duplicate repository = %d %+v", status, out)
	}
	if status, out := e.ecr("CreateRepository", map[string]any{"repositoryName": "Acme"}); status != http.StatusBadRequest || out["__type"] != "InvalidParameterException" {
		t.Fatalf("invalid name = %d %+v", status, out)
	}
	if _, out := e.ecr("DescribeRepositories", map[string]any{"repositoryNames": []string{"acme/web"}}); len(out["repositories"].([]any)) != 1 {
		t.Fatalf("DescribeRepositories = %+v", out)
	}
	e.ecr("DeleteRepository", map[string]any{"repositoryName": "acme/web", "force": true})
	if status, out := e.ecr("DescribeRepositories", map[string]any{"repositoryNames": []string{"acme/web"}}); status != http.StatusBadRequest || out["__type"] != "RepositoryNotFoundException" {
		t.Fatalf("deleted repository = %d %+v", status, out)
	}
}

func TestRepositoryCreationTemplateIsStoredAndReportedAsTerraformReadsIt(t *testing.T) {
	e := newEnv(t)

	status, created := e.ecr("CreateRepositoryCreationTemplate", map[string]any{"prefix": "stagelets", "appliedFor": []string{"CREATE_ON_PUSH"}, "description": "images"})
	if status != http.StatusOK || created["registryId"] != e.callerAccount() {
		t.Fatalf("CreateRepositoryCreationTemplate = %d %+v", status, created)
	}
	_, described := e.ecr("DescribeRepositoryCreationTemplates", map[string]any{"prefixes": []string{"stagelets"}})
	templates := described["repositoryCreationTemplates"].([]any)
	got := templates[0].(map[string]any)
	if len(templates) != 1 || got["description"] != "images" || got["imageTagMutability"] != "MUTABLE" || got["appliedFor"].([]any)[0] != "CREATE_ON_PUSH" ||
		got["encryptionConfiguration"].(map[string]any)["encryptionType"] != "AES256" {
		t.Fatalf("template = %+v", got)
	}
	e.ecr("DeleteRepositoryCreationTemplate", map[string]any{"prefix": "stagelets"})
	if status, out := e.ecr("DescribeRepositoryCreationTemplates", map[string]any{"prefixes": []string{"stagelets"}}); status != http.StatusBadRequest || out["__type"] != "TemplateNotFoundException" {
		t.Fatalf("deleted template = %d %+v", status, out)
	}
}

func TestECRRefusesFieldsAndOperationsItDoesNotSupport(t *testing.T) {
	e := newEnv(t)

	if status, out := e.ecr("CreateRepository", map[string]any{"repositoryName": "acme", "kmsKeyMagic": true}); status != http.StatusBadRequest || out["__type"] != "SerializationException" {
		t.Errorf("unknown field = %d %+v", status, out)
	}
	if status, out := e.ecr("PutLifecyclePolicy", map[string]any{}); status != http.StatusBadRequest || out["__type"] != "UnknownOperationException" {
		t.Errorf("unknown operation = %d %+v", status, out)
	}
}
