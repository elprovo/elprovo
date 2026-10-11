package aws

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	ecrTarget = "AmazonEC2ContainerRegistry_V20150921."
	// describePageSize is the most instances DescribeInstances returns in
	// one page.
	describePageSize = 1000
	// rootDevice is the root device of Ubuntu AMIs and of the golden images
	// built from them.
	rootDevice = "/dev/sda1"
)

var errNotFound = errors.New("not found on AWS")

// queryVersions are the API versions of the services that the agent calls
// with Query API requests.
var queryVersions = map[string]string{"ec2": "2016-11-15", "sts": "2011-06-15"}

// apiError is an error that an AWS API answered with.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("AWS answered %d %s: %s", e.status, e.code, e.message)
}

// Is reports a NotFound error code, such as InvalidInstanceID.NotFound, as
// errNotFound.
func (e *apiError) Is(target error) bool {
	return target == errNotFound && strings.HasSuffix(e.code, ".NotFound")
}

// client calls one AWS service at baseURL with signed requests.
type client struct {
	baseURL string
	signer  signer
	http    *http.Client
}

func newClient(baseURL, region, service string, creds Credentials) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/") + "/",
		signer:  signer{creds: creds, region: region, service: service, now: time.Now},
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// queryError is one error in a Query API answer.
type queryError struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// query calls a Query API action of EC2 or STS and decodes its XML answer
// into out.
func (c *client) query(ctx context.Context, action string, params url.Values, out any) error {
	params.Set("Action", action)
	params.Set("Version", queryVersions[c.signer.service])
	body := []byte(params.Encode())
	resp, err := c.send(ctx, body, map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			EC2 []queryError `xml:"Errors>Error"`
			STS []queryError `xml:"Error"`
		}
		_ = xml.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&failure)
		apiErr := &apiError{status: resp.StatusCode}
		if errs := append(failure.EC2, failure.STS...); len(errs) > 0 {
			apiErr.code, apiErr.message = errs[0].Code, errs[0].Message
		}
		return apiErr
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", action, err)
	}
	return nil
}

// callJSON calls a JSON 1.1 API operation, such as Amazon ECR's, and
// decodes its answer into out.
func (c *client) callJSON(ctx context.Context, operation string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", operation, err)
	}
	resp, err := c.send(ctx, body, map[string]string{"Content-Type": "application/x-amz-json-1.1", "X-Amz-Target": ecrTarget + operation})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&failure)
		code := failure.Type
		if i := strings.LastIndex(code, "#"); i >= 0 {
			code = code[i+1:]
		}
		return &apiError{status: resp.StatusCode, code: code, message: failure.Message}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", operation, err)
	}
	return nil
}

// signedAs returns a copy of the client that signs with creds.
func (c *client) signedAs(creds Credentials) *client {
	copied := *c
	copied.signer.creds = creds
	return &copied
}

func (c *client) send(ctx context.Context, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.signer.sign(req, body)
	return c.http.Do(req)
}

type instance struct {
	ID    string `xml:"instanceId"`
	State struct {
		Name string `xml:"name"`
	} `xml:"instanceState"`
	PrivateIP string `xml:"privateIpAddress"`
	PublicIP  string `xml:"ipAddress"`
	Tags      []tag  `xml:"tagSet>item"`
}

type tag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// tag returns the value of the instance's tag with key, or "".
func (i instance) tag(key string) string {
	for _, t := range i.Tags {
		if t.Key == key {
			return t.Value
		}
	}
	return ""
}

// filter is one DescribeInstances filter: an instance matches when its
// field Name has any of Values.
type filter struct {
	Name   string
	Values []string
}

func (c *client) describeInstances(ctx context.Context, filters []filter, ids ...string) ([]instance, error) {
	var all []instance
	next := ""
	for {
		params := url.Values{}
		for i, f := range filters {
			prefix := "Filter." + strconv.Itoa(i+1)
			params.Set(prefix+".Name", f.Name)
			for j, v := range f.Values {
				params.Set(prefix+".Value."+strconv.Itoa(j+1), v)
			}
		}
		for i, id := range ids {
			params.Set("InstanceId."+strconv.Itoa(i+1), id)
		}
		if len(ids) == 0 {
			params.Set("MaxResults", strconv.Itoa(describePageSize))
		}
		if next != "" {
			params.Set("NextToken", next)
		}
		var body struct {
			Reservations []struct {
				Instances []instance `xml:"instancesSet>item"`
			} `xml:"reservationSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := c.query(ctx, "DescribeInstances", params, &body); err != nil {
			return nil, fmt.Errorf("describe instances: %w", err)
		}
		for _, r := range body.Reservations {
			all = append(all, r.Instances...)
		}
		if next = body.NextToken; next == "" {
			return all, nil
		}
	}
}

// runInstance is the launch of one instance.
type runInstance struct {
	ImageID         string
	InstanceType    string
	KeyName         string
	UserData        string
	SubnetID        string
	SecurityGroupID string
	// VolumeSize is the root volume's size in GiB; 0 keeps the image's.
	VolumeSize int
	Tags       []tag
}

func (c *client) runInstance(ctx context.Context, req runInstance) (instance, error) {
	params := url.Values{
		"ImageId":      {req.ImageID},
		"InstanceType": {req.InstanceType},
		"MinCount":     {"1"},
		"MaxCount":     {"1"},
		"KeyName":      {req.KeyName},
		"UserData":     {base64.StdEncoding.EncodeToString([]byte(req.UserData))},
		// IMDSv2 with a hop limit of 1: containers, one hop further, get no
		// metadata token, so they can't read the user data and its host key.
		"MetadataOptions.HttpTokens":              {"required"},
		"MetadataOptions.HttpPutResponseHopLimit": {"1"},
	}
	if req.SubnetID != "" {
		params.Set("SubnetId", req.SubnetID)
	}
	if req.VolumeSize > 0 {
		params.Set("BlockDeviceMapping.1.DeviceName", rootDevice)
		params.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.Itoa(req.VolumeSize))
		params.Set("BlockDeviceMapping.1.Ebs.VolumeType", "gp3")
		params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")
	}
	if req.SecurityGroupID != "" {
		params.Set("SecurityGroupId.1", req.SecurityGroupID)
	}
	if len(req.Tags) > 0 {
		params.Set("TagSpecification.1.ResourceType", "instance")
		for i, t := range req.Tags {
			prefix := "TagSpecification.1.Tag." + strconv.Itoa(i+1)
			params.Set(prefix+".Key", t.Key)
			params.Set(prefix+".Value", t.Value)
		}
	}
	var body struct {
		Instances []instance `xml:"instancesSet>item"`
	}
	if err := c.query(ctx, "RunInstances", params, &body); err != nil {
		return instance{}, fmt.Errorf("run instance: %w", err)
	}
	if len(body.Instances) != 1 {
		return instance{}, fmt.Errorf("run instance: AWS answered with %d instances, want 1", len(body.Instances))
	}
	return body.Instances[0], nil
}

// terminateInstance terminates the instance. Terminating a missing
// instance succeeds.
func (c *client) terminateInstance(ctx context.Context, id string) error {
	var body struct{}
	err := c.query(ctx, "TerminateInstances", url.Values{"InstanceId.1": {id}}, &body)
	if err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("terminate instance %s: %w", id, err)
	}
	return nil
}

func (c *client) hasKeyPair(ctx context.Context, name string) (bool, error) {
	var body struct {
		Keys []struct {
			Name string `xml:"keyName"`
		} `xml:"keySet>item"`
	}
	err := c.query(ctx, "DescribeKeyPairs", url.Values{"KeyName.1": {name}}, &body)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up key pair %s: %w", name, err)
	}
	return len(body.Keys) > 0, nil
}

func (c *client) importKeyPair(ctx context.Context, name, publicKey string) error {
	params := url.Values{"KeyName": {name}, "PublicKeyMaterial": {base64.StdEncoding.EncodeToString([]byte(publicKey))}}
	var body struct{}
	if err := c.query(ctx, "ImportKeyPair", params, &body); err != nil {
		return fmt.Errorf("import key pair %s: %w", name, err)
	}
	return nil
}

// ecrAuthorization is an Amazon ECR authorization token of the account's
// default registry, base64 of "AWS:<password>", and the registry's URL.
type ecrAuthorization struct {
	Token         string `json:"authorizationToken"`
	ProxyEndpoint string `json:"proxyEndpoint"`
}

func (c *client) authorizationToken(ctx context.Context) (ecrAuthorization, error) {
	var body struct {
		AuthorizationData []ecrAuthorization `json:"authorizationData"`
	}
	if err := c.callJSON(ctx, "GetAuthorizationToken", struct{}{}, &body); err != nil {
		return ecrAuthorization{}, fmt.Errorf("get ECR authorization token: %w", err)
	}
	if len(body.AuthorizationData) == 0 || body.AuthorizationData[0].Token == "" {
		return ecrAuthorization{}, errors.New("get ECR authorization token: AWS answered without a token")
	}
	return body.AuthorizationData[0], nil
}

// federationToken returns temporary credentials of the federated user name,
// valid for duration, whose permissions are the intersection of the
// caller's and the session policy. Only the long-term access key of an IAM
// user may ask for them.
func (c *client) federationToken(ctx context.Context, name, policy string, duration time.Duration) (Credentials, error) {
	params := url.Values{"Name": {name}, "Policy": {policy}, "DurationSeconds": {strconv.Itoa(int(duration.Seconds()))}}
	var body struct {
		AccessKeyID     string `xml:"GetFederationTokenResult>Credentials>AccessKeyId"`
		SecretAccessKey string `xml:"GetFederationTokenResult>Credentials>SecretAccessKey"`
		SessionToken    string `xml:"GetFederationTokenResult>Credentials>SessionToken"`
	}
	if err := c.query(ctx, "GetFederationToken", params, &body); err != nil {
		return Credentials{}, fmt.Errorf("get federation token: %w", err)
	}
	creds := Credentials{
		AccessKeyID: strings.TrimSpace(body.AccessKeyID), SecretAccessKey: strings.TrimSpace(body.SecretAccessKey), SessionToken: strings.TrimSpace(body.SessionToken),
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" || creds.SessionToken == "" {
		return Credentials{}, errors.New("get federation token: AWS answered without temporary credentials")
	}
	return creds, nil
}
