package digitalocean

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

const pageSize = 200

var errNotFound = errors.New("not found on DigitalOcean")

// api calls the parts of the DigitalOcean API the provider uses.
type api struct {
	baseURL string
	token   string
	http    *http.Client
}

type droplet struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Networks struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

// address returns the droplet's IPv4 address of the given type, public or
// private, or "" when it has none yet.
func (d droplet) address(kind string) string {
	for _, n := range d.Networks.V4 {
		if n.Type == kind {
			return n.IPAddress
		}
	}
	return ""
}

type createDroplet struct {
	Name     string   `json:"name"`
	Region   string   `json:"region"`
	Size     string   `json:"size"`
	Image    string   `json:"image"`
	SSHKeys  []string `json:"ssh_keys"`
	Tags     []string `json:"tags"`
	UserData string   `json:"user_data"`
	VPCUUID  string   `json:"vpc_uuid,omitempty"`
}

func newAPI(baseURL, token string) *api {
	return &api{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (a *api) dropletsByTag(ctx context.Context, tag string) ([]droplet, error) {
	var all []droplet
	for page := 1; ; page++ {
		var body struct {
			Droplets []droplet `json:"droplets"`
		}
		q := url.Values{"tag_name": {tag}, "per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
		if err := a.do(ctx, http.MethodGet, "/v2/droplets?"+q.Encode(), nil, &body); err != nil {
			return nil, fmt.Errorf("list droplets: %w", err)
		}
		all = append(all, body.Droplets...)
		if len(body.Droplets) < pageSize {
			return all, nil
		}
	}
}

func (a *api) droplet(ctx context.Context, id int64) (droplet, error) {
	var body struct {
		Droplet droplet `json:"droplet"`
	}
	if err := a.do(ctx, http.MethodGet, "/v2/droplets/"+strconv.FormatInt(id, 10), nil, &body); err != nil {
		return droplet{}, err
	}
	return body.Droplet, nil
}

func (a *api) createDroplet(ctx context.Context, req createDroplet) (droplet, error) {
	var body struct {
		Droplet droplet `json:"droplet"`
	}
	if err := a.do(ctx, http.MethodPost, "/v2/droplets", req, &body); err != nil {
		return droplet{}, fmt.Errorf("create droplet %s: %w", req.Name, err)
	}
	return body.Droplet, nil
}

// deleteDroplet deletes the droplet. Deleting a missing droplet succeeds.
func (a *api) deleteDroplet(ctx context.Context, id int64) error {
	err := a.do(ctx, http.MethodDelete, "/v2/droplets/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("delete droplet %d: %w", id, err)
	}
	return nil
}

func (a *api) hasSSHKey(ctx context.Context, fingerprint string) (bool, error) {
	err := a.do(ctx, http.MethodGet, "/v2/account/keys/"+url.PathEscape(fingerprint), nil, nil)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up SSH key: %w", err)
	}
	return true, nil
}

func (a *api) addSSHKey(ctx context.Context, name, publicKey string) error {
	if err := a.do(ctx, http.MethodPost, "/v2/account/keys", map[string]string{"name": name, "public_key": publicKey}, nil); err != nil {
		return fmt.Errorf("add SSH key: %w", err)
	}
	return nil
}

// dockerCredentials returns credentials for the registry at host that
// expire after expiry and can push only when readWrite is set.
func (a *api) dockerCredentials(ctx context.Context, host string, readWrite bool, expiry time.Duration) (username, password string, err error) {
	var out struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	q := url.Values{"read_write": {strconv.FormatBool(readWrite)}, "expiry_seconds": {strconv.Itoa(int(expiry.Seconds()))}}
	if err := a.do(ctx, http.MethodGet, "/v2/registry/docker-credentials?"+q.Encode(), nil, &out); err != nil {
		return "", "", fmt.Errorf("get registry credentials: %w", err)
	}
	entry, ok := out.Auths[host]
	if !ok {
		return "", "", fmt.Errorf("DigitalOcean gave no credentials for registry %s", host)
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
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&apiErr)
		return fmt.Errorf("DigitalOcean answered %s: %s", resp.Status, apiErr.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
