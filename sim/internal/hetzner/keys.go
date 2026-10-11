package hetzner

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type sshKeyJSON struct {
	ID          int64             `json:"id"`
	Name        string            `json:"name"`
	Fingerprint string            `json:"fingerprint"`
	PublicKey   string            `json:"public_key"`
	Labels      map[string]string `json:"labels"`
	Created     string            `json:"created"`
}

func (k *sshKey) render() sshKeyJSON {
	return sshKeyJSON{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey, Labels: labelsOrEmpty(k.Labels), Created: timestamp(k.Created)}
}

// fingerprint returns the MD5 fingerprint of an OpenSSH public key, as
// colon-separated hex, which is how Hetzner identifies keys.
func fingerprint(publicKey string) (string, error) {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return "", fmt.Errorf("must be of the format `type key [comment]`")
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 4 {
		return "", fmt.Errorf("key data is not base64")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n)+4 > uint64(len(blob)) || !bytes.Equal(blob[4:4+n], []byte(fields[0])) {
		return "", fmt.Errorf("key data does not match key type %s", fields[0])
	}
	sum := md5.Sum(blob)
	hexes := make([]string, len(sum))
	for i, b := range sum {
		hexes[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(hexes, ":"), nil
}

func (s *Simulation) listSSHKeys(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	name, fp := q.Get("name"), q.Get("fingerprint")
	var out []sshKeyJSON
	s.store.Read(func(p *project) {
		for _, k := range p.SSHKeys {
			if (name == "" || k.Name == name) && (fp == "" || k.Fingerprint == fp) && keep(k.Labels) {
				out = append(out, k.render())
			}
		}
	})
	return listResponse(r, "ssh_keys", out, sortFields[sshKeyJSON]{
		"id":   byID(func(k sshKeyJSON) int64 { return k.ID }),
		"name": byString(func(k sshKeyJSON) string { return k.Name }),
	})
}

func (s *Simulation) createSSHKey(r *http.Request) (int, any, error) {
	var req struct {
		Name      string            `json:"name"`
		PublicKey string            `json:"public_key"`
		Labels    map[string]string `json:"labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(req.Name) == "" {
		return 0, nil, errInvalidInput("name", "is required")
	}
	fp, err := fingerprint(req.PublicKey)
	if err != nil {
		return 0, nil, errInvalidInput("public_key", "%v", err)
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	var out sshKeyJSON
	err = s.store.Update(func(p *project) error {
		for _, k := range p.SSHKeys {
			if k.Name == req.Name {
				return errUniqueness("name", "SSH key with the same name already exists")
			}
			if k.Fingerprint == fp {
				return errUniqueness("public_key", "SSH key with the same fingerprint already exists")
			}
		}
		k := &sshKey{ID: p.nextID(), Name: req.Name, Fingerprint: fp, PublicKey: req.PublicKey, Labels: req.Labels, Created: s.now().UTC()}
		p.SSHKeys = append(p.SSHKeys, k)
		out = k.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"ssh_key": out}, nil
}

func (s *Simulation) getSSHKey(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "SSH key")
	if err != nil {
		return 0, nil, err
	}
	var out *sshKeyJSON
	s.store.Read(func(p *project) {
		if k := p.sshKey(id); k != nil {
			j := k.render()
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("SSH key")
	}
	return http.StatusOK, map[string]any{"ssh_key": out}, nil
}

func (s *Simulation) updateSSHKey(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "SSH key")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Name   string             `json:"name"`
		Labels *map[string]string `json:"labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out sshKeyJSON
	err = s.store.Update(func(p *project) error {
		k := p.sshKey(id)
		if k == nil {
			return errNotFound("SSH key")
		}
		if req.Name != "" && req.Name != k.Name {
			if slices.ContainsFunc(p.SSHKeys, func(o *sshKey) bool { return o.Name == req.Name }) {
				return errUniqueness("name", "SSH key with the same name already exists")
			}
			k.Name = req.Name
		}
		if req.Labels != nil {
			k.Labels = *req.Labels
		}
		out = k.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ssh_key": out}, nil
}

func (s *Simulation) deleteSSHKey(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "SSH key")
	if err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(p *project) error {
		if p.sshKey(id) == nil {
			return errNotFound("SSH key")
		}
		p.SSHKeys = slices.DeleteFunc(p.SSHKeys, func(k *sshKey) bool { return k.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}
