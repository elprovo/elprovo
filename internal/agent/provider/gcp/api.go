package gcp

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
	"time"
)

const pageSize = 500

var errNotFound = errors.New("not found on Google Cloud")

// api calls the parts of the Compute Engine API v1 that the provider uses.
type api struct {
	baseURL           string
	iamCredentialsURL string
	project           string
	zone              string
	tokens            *tokenSource
	http              *http.Client
}

type instance struct {
	Name              string            `json:"name"`
	Status            string            `json:"status"`
	Labels            map[string]string `json:"labels"`
	NetworkInterfaces []struct {
		NetworkIP string `json:"networkIP"`
	} `json:"networkInterfaces"`
}

// internalAddress returns the instance's internal IPv4 address, or "" when
// it has none yet.
func (i instance) internalAddress() string {
	if len(i.NetworkInterfaces) == 0 {
		return ""
	}
	return i.NetworkInterfaces[0].NetworkIP
}

type operation struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  *struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// failure returns the operation's error, or nil when it succeeded.
func (o operation) failure() error {
	if o.Error == nil || len(o.Error.Errors) == 0 {
		return nil
	}
	var msgs []string
	for _, e := range o.Error.Errors {
		msgs = append(msgs, e.Code+": "+e.Message)
	}
	return fmt.Errorf("operation %s failed: %s", o.Name, strings.Join(msgs, "; "))
}

type metadataItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type insertInstance struct {
	Name              string            `json:"name"`
	MachineType       string            `json:"machineType"`
	Disks             []insertDisk      `json:"disks"`
	NetworkInterfaces []insertInterface `json:"networkInterfaces"`
	Metadata          metadata          `json:"metadata"`
	Tags              tags              `json:"tags"`
	Labels            map[string]string `json:"labels"`
}

type metadata struct {
	Items []metadataItem `json:"items"`
}

type tags struct {
	Items []string `json:"items"`
}

type insertDisk struct {
	Boot             bool              `json:"boot"`
	AutoDelete       bool              `json:"autoDelete"`
	InitializeParams map[string]string `json:"initializeParams"`
}

type insertInterface struct {
	Network       string              `json:"network,omitempty"`
	Subnetwork    string              `json:"subnetwork,omitempty"`
	AccessConfigs []map[string]string `json:"accessConfigs"`
}

func (a *api) zonePath(parts ...string) string {
	return "projects/" + a.project + "/zones/" + a.zone + "/" + strings.Join(parts, "/")
}

func (a *api) instance(ctx context.Context, name string) (instance, error) {
	var out instance
	err := a.do(ctx, http.MethodGet, a.zonePath("instances", name), nil, &out)
	return out, err
}

// instancesLabeled lists the zone's instances whose label key has value.
func (a *api) instancesLabeled(ctx context.Context, key, value string) ([]instance, error) {
	var all []instance
	token := ""
	for {
		q := url.Values{"filter": {"labels." + key + ` = "` + value + `"`}, "maxResults": {fmt.Sprint(pageSize)}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items         []instance `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := a.do(ctx, http.MethodGet, a.zonePath("instances")+"?"+q.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("list instances: %w", err)
		}
		all = append(all, page.Items...)
		if token = page.NextPageToken; token == "" {
			return all, nil
		}
	}
}

func (a *api) insertInstance(ctx context.Context, req insertInstance) (operation, error) {
	var op operation
	err := a.do(ctx, http.MethodPost, a.zonePath("instances"), req, &op)
	return op, err
}

// deleteInstance deletes the instance. Deleting a missing instance
// succeeds with no operation.
func (a *api) deleteInstance(ctx context.Context, name string) (*operation, error) {
	var op operation
	err := a.do(ctx, http.MethodDelete, a.zonePath("instances", name), nil, &op)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete instance %s: %w", name, err)
	}
	return &op, nil
}

// wait waits until the zone operation is DONE with the operation's wait
// method, which answers after at most two minutes, and returns its error.
// The method may answer early, so the next call waits pause first.
func (a *api) wait(ctx context.Context, op operation, pause time.Duration) error {
	for op.Status != "DONE" {
		if err := a.do(ctx, http.MethodPost, a.zonePath("operations", url.PathEscape(op.Name), "wait"), nil, &op); err != nil {
			return fmt.Errorf("wait for operation %s: %w", op.Name, err)
		}
		if op.Status == "DONE" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for operation %s: %w", op.Name, ctx.Err())
		case <-time.After(pause):
		}
	}
	return op.failure()
}

// accessToken returns an access token of the service account, which the
// provider's service account may impersonate, that expires after lifetime.
func (a *api) accessToken(ctx context.Context, account string, lifetime time.Duration) (string, error) {
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	in := map[string]any{"scope": []string{cloudPlatformScope}, "lifetime": fmt.Sprintf("%ds", int(lifetime.Seconds()))}
	u := a.iamCredentialsURL + "projects/-/serviceAccounts/" + url.PathEscape(account) + ":generateAccessToken"
	if err := a.doURL(ctx, http.MethodPost, u, in, &out); err != nil {
		return "", fmt.Errorf("get an access token for %s: %w", account, err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("get an access token for %s: the answer holds no token", account)
	}
	return out.AccessToken, nil
}

func (a *api) do(ctx context.Context, method, path string, in, out any) error {
	return a.doURL(ctx, method, a.baseURL+path, in, out)
}

func (a *api) doURL(ctx context.Context, method, u string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	token, err := a.tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		a.tokens.forget()
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&apiErr)
		return fmt.Errorf("google cloud answered %s: %s", resp.Status, apiErr.Error.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
