package vultr

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"strings"
)

const sshKeyNotFound = "Invalid ssh key ID."

type sshKeyJSON struct {
	ID          string `json:"id"`
	DateCreated string `json:"date_created"`
	Name        string `json:"name"`
	SSHKey      string `json:"ssh_key"`
}

func renderSSHKey(k *sshKey) sshKeyJSON {
	return sshKeyJSON{ID: k.ID, DateCreated: timestamp(k.CreatedAt), Name: k.Name, SSHKey: k.SSHKey}
}

// checkPublicKey accepts an OpenSSH authorized key: a type, base64 key data
// that starts with that type, and an optional comment.
func checkPublicKey(key string) error {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return errors.New("the key must be of the form `type key [comment]`")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 4 {
		return errors.New("the key data is not base64")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n)+4 > uint64(len(blob)) || !bytes.Equal(blob[4:4+n], []byte(fields[0])) {
		return errors.New("the key data does not match the key type " + fields[0])
	}
	return nil
}

func (s *Simulation) listSSHKeys(r *http.Request) (int, any, error) {
	var out []sshKeyJSON
	s.store.Read(func(a *account) {
		for _, k := range a.SSHKeys {
			out = append(out, renderSSHKey(k))
		}
	})
	return listResponse(r, "ssh_keys", out)
}

type sshKeyRequest struct {
	Name   *string `json:"name"`
	SSHKey *string `json:"ssh_key"`
}

func (req sshKeyRequest) check() error {
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		return errBadRequest("Invalid SSH key name.")
	}
	if req.SSHKey != nil {
		if err := checkPublicKey(*req.SSHKey); err != nil {
			return errBadRequest("Invalid SSH key: %v.", err)
		}
	}
	return nil
}

func (s *Simulation) createSSHKey(r *http.Request) (int, any, error) {
	var req sshKeyRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Name == nil || req.SSHKey == nil {
		return 0, nil, errBadRequest("name and ssh_key are required.")
	}
	if err := req.check(); err != nil {
		return 0, nil, err
	}
	var out sshKeyJSON
	err := s.store.Update(func(a *account) error {
		k := &sshKey{ID: newUUID(), Name: *req.Name, SSHKey: *req.SSHKey, CreatedAt: s.now()}
		a.SSHKeys = append(a.SSHKeys, k)
		out = renderSSHKey(k)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"ssh_key": out}, nil
}

func (s *Simulation) getSSHKey(r *http.Request) (int, any, error) {
	var out *sshKeyJSON
	s.store.Read(func(a *account) {
		if k := a.sshKey(r.PathValue("id")); k != nil {
			j := renderSSHKey(k)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(sshKeyNotFound)
	}
	return http.StatusOK, map[string]any{"ssh_key": out}, nil
}

func (s *Simulation) updateSSHKey(r *http.Request) (int, any, error) {
	var req sshKeyRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if err := req.check(); err != nil {
		return 0, nil, err
	}
	err := s.store.Update(func(a *account) error {
		k := a.sshKey(r.PathValue("id"))
		if k == nil {
			return errNotFound(sshKeyNotFound)
		}
		if req.Name != nil {
			k.Name = *req.Name
		}
		if req.SSHKey != nil {
			k.SSHKey = *req.SSHKey
		}
		return nil
	})
	return http.StatusNoContent, nil, err
}

func (s *Simulation) deleteSSHKey(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		if a.sshKey(r.PathValue("id")) == nil {
			return errNotFound(sshKeyNotFound)
		}
		a.SSHKeys = slices.DeleteFunc(a.SSHKeys, func(k *sshKey) bool { return k.ID == r.PathValue("id") })
		return nil
	})
	return http.StatusNoContent, nil, err
}
