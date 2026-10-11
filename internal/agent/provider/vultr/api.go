package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const pageSize = 500

var (
	errNotFound = errors.New("not found on Vultr")
	// errPendingDestruction is Vultr's answer about an instance it is
	// deleting.
	errPendingDestruction = errors.New("the instance is being destroyed")
)

// api calls the parts of the Vultr API v2 the provider uses.
type api struct {
	baseURL string
	key     string
	http    *http.Client
}

type instance struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	MainIP      string   `json:"main_ip"`
	InternalIP  string   `json:"internal_ip"`
	Status      string   `json:"status"`
	PowerStatus string   `json:"power_status"`
	Tags        []string `json:"tags"`
}

type createInstance struct {
	Region          string   `json:"region"`
	Plan            string   `json:"plan"`
	OSID            int      `json:"os_id,omitempty"`
	SnapshotID      string   `json:"snapshot_id,omitempty"`
	Label           string   `json:"label"`
	Hostname        string   `json:"hostname"`
	Tags            []string `json:"tags"`
	SSHKeyIDs       []string `json:"sshkey_id"`
	UserData        string   `json:"user_data"`
	FirewallGroupID string   `json:"firewall_group_id,omitempty"`
	AttachVPC       []string `json:"attach_vpc"`
}

type sshKey struct {
	ID     string `json:"id"`
	SSHKey string `json:"ssh_key"`
}

type meta struct {
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}

func newAPI(baseURL, key string) *api {
	return &api{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: &http.Client{Timeout: 30 * time.Second}}
}

// instances lists the account's instances, with the label when it is not
// empty, following the pagination cursors.
func (a *api) instances(ctx context.Context, label string) ([]instance, error) {
	var all []instance
	q := url.Values{"per_page": {strconv.Itoa(pageSize)}}
	if label != "" {
		q.Set("label", label)
	}
	for {
		var body struct {
			Instances []instance `json:"instances"`
			Meta      meta       `json:"meta"`
		}
		if err := a.do(ctx, http.MethodGet, "/v2/instances?"+q.Encode(), nil, &body); err != nil {
			return nil, fmt.Errorf("list instances: %w", err)
		}
		all = append(all, body.Instances...)
		if body.Meta.Links.Next == "" {
			return all, nil
		}
		q.Set("cursor", body.Meta.Links.Next)
	}
}

func (a *api) instance(ctx context.Context, id string) (instance, error) {
	var body struct {
		Instance instance `json:"instance"`
	}
	if err := a.do(ctx, http.MethodGet, "/v2/instances/"+url.PathEscape(id), nil, &body); err != nil {
		return instance{}, err
	}
	return body.Instance, nil
}

func (a *api) createInstance(ctx context.Context, req createInstance) (instance, error) {
	var body struct {
		Instance instance `json:"instance"`
	}
	if err := a.do(ctx, http.MethodPost, "/v2/instances", req, &body); err != nil {
		return instance{}, fmt.Errorf("create instance %s: %w", req.Label, err)
	}
	return body.Instance, nil
}

// deleteInstance deletes the instance. Deleting a missing instance, or one
// that Vultr is deleting already, succeeds.
func (a *api) deleteInstance(ctx context.Context, id string) error {
	err := a.do(ctx, http.MethodDelete, "/v2/instances/"+url.PathEscape(id), nil, nil)
	if err != nil && !errors.Is(err, errNotFound) && !errors.Is(err, errPendingDestruction) {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	return nil
}

func (a *api) sshKeys(ctx context.Context) ([]sshKey, error) {
	var all []sshKey
	q := url.Values{"per_page": {strconv.Itoa(pageSize)}}
	for {
		var body struct {
			SSHKeys []sshKey `json:"ssh_keys"`
			Meta    meta     `json:"meta"`
		}
		if err := a.do(ctx, http.MethodGet, "/v2/ssh-keys?"+q.Encode(), nil, &body); err != nil {
			return nil, fmt.Errorf("list SSH keys: %w", err)
		}
		all = append(all, body.SSHKeys...)
		if body.Meta.Links.Next == "" {
			return all, nil
		}
		q.Set("cursor", body.Meta.Links.Next)
	}
}

func (a *api) addSSHKey(ctx context.Context, name, publicKey string) (string, error) {
	var body struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	if err := a.do(ctx, http.MethodPost, "/v2/ssh-keys", map[string]string{"name": name, "ssh_key": publicKey}, &body); err != nil {
		return "", fmt.Errorf("add SSH key: %w", err)
	}
	return body.SSHKey.ID, nil
}

// dockerCredentials returns credentials for the registry with the id, at
// host, that expire after expiry and can push only when readWrite is set.
// Vultr serves them for OPTIONS only.
func (a *api) dockerCredentials(ctx context.Context, registryID, host string, readWrite bool, expiry time.Duration) (username, password string, err error) {
	var out struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	q := url.Values{"read_write": {strconv.FormatBool(readWrite)}, "expiry_seconds": {strconv.Itoa(int(expiry.Seconds()))}}
	if err := a.do(ctx, http.MethodOptions, "/v2/registry/"+url.PathEscape(registryID)+"/docker-credentials?"+q.Encode(), nil, &out); err != nil {
		return "", "", fmt.Errorf("get registry credentials: %w", err)
	}
	entry, ok := out.Auths[host]
	if !ok {
		return "", "", fmt.Errorf("the Vultr API gave no credentials for registry %s", host)
	}
	raw, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", "", fmt.Errorf("decode credentials for registry %s: %w", host, err)
	}
	username, password, ok = strings.Cut(string(raw), ":")
	if !ok {
		return "", "", fmt.Errorf("credentials for registry %s have no password", host)
	}
	return username, password, nil
}

func (a *api) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// responseError turns Vultr's error body, {"error": message, "status":
// code}, into an error.
func responseError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case strings.Contains(body.Error, "pending destruction"):
		return errPendingDestruction
	}
	return fmt.Errorf("the Vultr API answered %s: %s", resp.Status, body.Error)
}
