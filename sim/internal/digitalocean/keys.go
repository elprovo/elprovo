package digitalocean

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func fingerprint(publicKey string) (string, error) {
	fields := strings.Fields(publicKey)
	if len(fields) < 2 {
		return "", fmt.Errorf("key should be of the format `type key [comment]`")
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

func (a *account) keyByRef(ref string) *sshKey {
	for _, k := range a.Keys {
		if k.Fingerprint == ref || strconv.FormatInt(k.ID, 10) == ref {
			return k
		}
	}
	return nil
}

func (s *Simulation) listKeys(r *http.Request) (int, any, error) {
	var keys []sshKey
	s.store.Read(func(a *account) {
		for _, k := range a.Keys {
			keys = append(keys, *k)
		}
	})
	return listResponse(r, "ssh_keys", keys)
}

func (s *Simulation) createKey(r *http.Request) (int, any, error) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if strings.TrimSpace(req.Name) == "" {
		return 0, nil, errUnprocessable("name is required")
	}
	fp, err := fingerprint(req.PublicKey)
	if err != nil {
		return 0, nil, errUnprocessable("Key invalid, %v", err)
	}
	var created sshKey
	err = s.store.Update(func(a *account) error {
		if a.keyByRef(fp) != nil {
			return errUnprocessable("SSH Key is already in use on your account")
		}
		k := &sshKey{ID: a.nextID(), Name: req.Name, Fingerprint: fp, PublicKey: req.PublicKey}
		a.Keys = append(a.Keys, k)
		created = *k
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"ssh_key": created}, nil
}

func (s *Simulation) getKey(r *http.Request) (int, any, error) {
	var found *sshKey
	s.store.Read(func(a *account) {
		if k := a.keyByRef(r.PathValue("key")); k != nil {
			copied := *k
			found = &copied
		}
	})
	if found == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"ssh_key": found}, nil
}

func (s *Simulation) updateKey(r *http.Request) (int, any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var updated sshKey
	err := s.store.Update(func(a *account) error {
		k := a.keyByRef(r.PathValue("key"))
		if k == nil {
			return errNotFound()
		}
		if req.Name != "" {
			k.Name = req.Name
		}
		updated = *k
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ssh_key": updated}, nil
}

func (s *Simulation) deleteKey(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		k := a.keyByRef(r.PathValue("key"))
		if k == nil {
			return errNotFound()
		}
		a.Keys = slices.DeleteFunc(a.Keys, func(other *sshKey) bool { return other.ID == k.ID })
		return nil
	})
	return http.StatusNoContent, nil, err
}
