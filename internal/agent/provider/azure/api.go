package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	computeAPIVersion  = "2024-07-01"
	networkAPIVersion  = "2024-05-01"
	metadataAPIVersion = "2022-09-01"
	// tokenMargin is how long before expiry a token is renewed.
	tokenMargin = 5 * time.Minute
)

var errNotFound = errors.New("not found on Azure")

// api calls the parts of Azure Resource Manager the provider uses, with a
// token from Microsoft Entra ID's client credentials grant.
type api struct {
	armURL       string
	tenant       string
	clientID     string
	clientSecret string
	http         *http.Client
	now          func() time.Time

	tokenMu  sync.Mutex
	token    string
	expires  time.Time
	login    string
	audience string
}

func newAPI(armURL, tenant, clientID, clientSecret string) *api {
	return &api{
		armURL: strings.TrimRight(armURL, "/"), tenant: tenant, clientID: clientID, clientSecret: clientSecret,
		http: &http.Client{Timeout: 30 * time.Second}, now: time.Now,
	}
}

// accessToken returns a Resource Manager token, getting a new one when the
// cached one is close to expiry. The cloud's metadata document names the
// login endpoint and the token audience, as for any Azure cloud.
func (a *api) accessToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token != "" && a.now().Before(a.expires.Add(-tokenMargin)) {
		return a.token, nil
	}
	if a.login == "" {
		if err := a.discover(ctx); err != nil {
			return "", err
		}
	}
	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {a.clientID}, "client_secret": {a.clientSecret},
		"scope": {a.audience + "/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.login+"/"+url.PathEscape(a.tenant)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get an Azure token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("get an Azure token: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		return "", fmt.Errorf("get an Azure token: %s: %s: %s", resp.Status, body.Error, body.Description)
	}
	a.token, a.expires = body.AccessToken, a.now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return a.token, nil
}

func (a *api) discover(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.armURL+"/metadata/endpoints?api-version="+metadataAPIVersion, nil)
	if err != nil {
		return err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("read the Azure metadata document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		ResourceManager string `json:"resourceManager"`
		Authentication  struct {
			LoginEndpoint string `json:"loginEndpoint"`
		} `json:"authentication"`
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("read the Azure metadata document: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return fmt.Errorf("read the Azure metadata document: %w", err)
	}
	if doc.Authentication.LoginEndpoint == "" || doc.ResourceManager == "" {
		return errors.New("the Azure metadata document names no login endpoint or Resource Manager")
	}
	login, err := url.Parse(doc.Authentication.LoginEndpoint)
	if err != nil {
		return fmt.Errorf("the Azure metadata document's login endpoint: %w", err)
	}
	if arm, _ := url.Parse(a.armURL); login.Scheme != "https" && (arm == nil || login.Scheme != arm.Scheme || login.Host != arm.Host) {
		return fmt.Errorf("the Azure metadata document names the login endpoint %s, which is neither HTTPS nor the Resource Manager endpoint, so the client secret would travel in the clear", login.Redacted())
	}
	a.login = strings.TrimRight(doc.Authentication.LoginEndpoint, "/")
	a.audience = strings.TrimRight(doc.ResourceManager, "/")
	return nil
}

// response is what a Resource Manager call answered.
type response struct {
	status int
	// operation is the Azure-AsyncOperation link of a long-running
	// operation, or "".
	operation string
	// location is the Location link of an accepted request, or "".
	location string
}

// do calls Resource Manager at path, a resource id or operation link path,
// with apiVersion. It decodes a JSON answer into out. A 404 is errNotFound.
func (a *api) do(ctx context.Context, method, path, apiVersion string, in, out any) (response, error) {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return response{}, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return response{}, fmt.Errorf("resource path %q must start with a single /", path)
	}
	u, err := url.Parse(a.armURL + path)
	if err != nil {
		return response{}, err
	}
	q := u.Query()
	if apiVersion != "" {
		q.Set("api-version", apiVersion)
	}
	u.RawQuery = q.Encode()
	token, err := a.accessToken(ctx)
	if err != nil {
		return response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	got := response{status: resp.StatusCode, operation: resp.Header.Get("Azure-AsyncOperation"), location: resp.Header.Get("Location")}
	if resp.StatusCode == http.StatusNotFound {
		return got, errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var armErr struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&armErr)
		return got, fmt.Errorf("the Azure API answered %s: %s: %s", resp.Status, armErr.Error.Code, armErr.Error.Message)
	}
	if out == nil {
		return got, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return got, fmt.Errorf("read response: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return got, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return got, fmt.Errorf("decode response: %w", err)
	}
	return got, nil
}

// wait polls a long-running operation until it ends. It follows only the
// link's path, on the configured endpoint, so the token never goes to
// another host.
func (a *api) wait(ctx context.Context, link string, every time.Duration) error {
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Errorf("operation link %q: %w", link, err)
	}
	for {
		var op struct {
			Status string `json:"status"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if _, err := a.do(ctx, http.MethodGet, u.Path, u.Query().Get("api-version"), nil, &op); err != nil {
			return fmt.Errorf("poll operation: %w", err)
		}
		switch op.Status {
		case "Succeeded":
			return nil
		case "Failed", "Canceled":
			return fmt.Errorf("operation %s: %s: %s", strings.ToLower(op.Status), op.Error.Code, op.Error.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// list follows a list's nextLink until the last page, decoding each page's
// value items with add.
func (a *api) list(ctx context.Context, path, apiVersion string, add func(json.RawMessage) error) error {
	for path != "" {
		var page struct {
			Value    []json.RawMessage `json:"value"`
			NextLink string            `json:"nextLink"`
		}
		if _, err := a.do(ctx, http.MethodGet, path, apiVersion, nil, &page); err != nil {
			return err
		}
		for _, item := range page.Value {
			if err := add(item); err != nil {
				return err
			}
		}
		path, apiVersion = "", ""
		if page.NextLink != "" {
			next, err := url.Parse(page.NextLink)
			if err != nil {
				return fmt.Errorf("nextLink %q: %w", page.NextLink, err)
			}
			path = next.Path + "?" + next.RawQuery
		}
	}
	return nil
}
