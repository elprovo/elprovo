package hetzner

import (
	"bytes"
	"context"
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

const pageSize = 50

var (
	errNotFound = errors.New("not found on Hetzner Cloud")
	// errLocked reports a resource that an action of its own still holds.
	errLocked = errors.New("locked by a running action")
)

// api calls the parts of the Hetzner Cloud API the provider uses.
type api struct {
	baseURL string
	token   string
	http    *http.Client
}

type server struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Status     string       `json:"status"`
	PrivateNet []privateNet `json:"private_net"`
}

// privateNet is a server's attachment to a private network.
type privateNet struct {
	Network int64  `json:"network"`
	IP      string `json:"ip"`
}

// privateIP returns the server's address on the network, or "" when it has
// none.
func (s server) privateIP(network int64) string {
	for _, n := range s.PrivateNet {
		if n.Network == network {
			return n.IP
		}
	}
	return ""
}

type publicNet struct {
	EnableIPv4 bool `json:"enable_ipv4"`
	EnableIPv6 bool `json:"enable_ipv6"`
}

type createServer struct {
	Name       string            `json:"name"`
	ServerType string            `json:"server_type"`
	Image      string            `json:"image"`
	Location   string            `json:"location"`
	SSHKeys    []int64           `json:"ssh_keys"`
	UserData   string            `json:"user_data"`
	Labels     map[string]string `json:"labels"`
	Networks   []int64           `json:"networks"`
	PublicNet  publicNet         `json:"public_net"`
}

type sshKey struct {
	ID int64 `json:"id"`
}

func newAPI(baseURL, token string) *api {
	return &api{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// servers returns the servers that match the label selector and, when name
// is set, have that name.
func (a *api) servers(ctx context.Context, selector, name string) ([]server, error) {
	var all []server
	for page := 1; ; {
		var body struct {
			Servers []server `json:"servers"`
			Meta    struct {
				Pagination struct {
					NextPage *int `json:"next_page"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		q := url.Values{"label_selector": {selector}, "per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
		if name != "" {
			q.Set("name", name)
		}
		if err := a.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &body); err != nil {
			return nil, fmt.Errorf("list servers: %w", err)
		}
		all = append(all, body.Servers...)
		next := body.Meta.Pagination.NextPage
		if next == nil || *next <= page {
			break
		}
		page = *next
	}
	return all, nil
}

func (a *api) server(ctx context.Context, id int64) (server, error) {
	var body struct {
		Server server `json:"server"`
	}
	if err := a.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &body); err != nil {
		return server{}, err
	}
	return body.Server, nil
}

func (a *api) createServer(ctx context.Context, req createServer) error {
	if err := a.do(ctx, http.MethodPost, "/servers", req, nil); err != nil {
		return fmt.Errorf("create server %s: %w", req.Name, err)
	}
	return nil
}

// deleteServer starts deleting the server. Deleting a missing server
// succeeds.
func (a *api) deleteServer(ctx context.Context, id int64) error {
	err := a.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("delete server %d: %w", id, err)
	}
	return nil
}

// sshKeyID returns the id of the SSH key with the fingerprint, or 0 when the
// project has none.
func (a *api) sshKeyID(ctx context.Context, fingerprint string) (int64, error) {
	var body struct {
		SSHKeys []sshKey `json:"ssh_keys"`
	}
	if err := a.do(ctx, http.MethodGet, "/ssh_keys?fingerprint="+url.QueryEscape(fingerprint), nil, &body); err != nil {
		return 0, fmt.Errorf("look up SSH key: %w", err)
	}
	if len(body.SSHKeys) == 0 {
		return 0, nil
	}
	return body.SSHKeys[0].ID, nil
}

func (a *api) addSSHKey(ctx context.Context, name, publicKey string) (int64, error) {
	var body struct {
		SSHKey sshKey `json:"ssh_key"`
	}
	if err := a.do(ctx, http.MethodPost, "/ssh_keys", map[string]string{"name": name, "public_key": publicKey}, &body); err != nil {
		return 0, fmt.Errorf("add SSH key: %w", err)
	}
	return body.SSHKey.ID, nil
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
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&apiErr)
		err := fmt.Errorf("the Hetzner Cloud API answered %s: %s (%s)", resp.Status, apiErr.Error.Message, apiErr.Error.Code)
		if resp.StatusCode == http.StatusLocked {
			return fmt.Errorf("%w: %w", errLocked, err)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
