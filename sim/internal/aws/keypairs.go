package aws

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"time"
)

type keyPair struct {
	ID          string    `json:"id"`
	Region      string    `json:"region"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	CreateTime  time.Time `json:"create_time"`
}

func (s *Simulation) ec2KeyPairActions() map[string]ec2Action {
	return map[string]ec2Action{
		"ImportKeyPair":    {s.importKeyPair, []string{"KeyName", "PublicKeyMaterial", "TagSpecification"}},
		"CreateKeyPair":    {s.createKeyPair, []string{"KeyName", "KeyType", "KeyFormat", "TagSpecification"}},
		"DescribeKeyPairs": {s.describeKeyPairs, []string{"KeyName", "KeyPairId", "Filter", "IncludePublicKey"}},
		"DeleteKeyPair":    {s.deleteKeyPair, []string{"KeyName", "KeyPairId"}},
	}
}

// parsePublicKey reads an OpenSSH public key line and returns its type, as
// EC2 names it, and its fingerprint as EC2 computes it for imported keys:
// the MD5 of the key blob for RSA (RFC 4716) and the base64 SHA-256 of the
// blob for ED25519.
func parsePublicKey(line string) (keyType, fingerprint, normalized string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", "", false
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) < 4 {
		return "", "", "", false
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n)+4 > uint64(len(blob)) || string(blob[4:4+n]) != fields[0] {
		return "", "", "", false
	}
	normalized = fields[0] + " " + fields[1]
	switch fields[0] {
	case "ssh-rsa":
		sum := md5.Sum(blob)
		return "rsa", colonHex(sum[:]), normalized, true
	case "ssh-ed25519":
		sum := sha256.Sum256(blob)
		return "ed25519", base64.StdEncoding.EncodeToString(sum[:]), normalized, true
	}
	return "", "", "", false
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = hex.EncodeToString([]byte{c})
	}
	return strings.Join(parts, ":")
}

func checkKeyName(name string) error {
	if name == "" {
		return errMissingParameter("KeyName")
	}
	if len(name) > 255 {
		return errInvalidParameter("Key pair names can have up to 255 characters")
	}
	return nil
}

type importKeyPairResponse struct {
	ec2Meta
	KeyFingerprint string `xml:"keyFingerprint"`
	KeyName        string `xml:"keyName"`
	KeyPairID      string `xml:"keyPairId"`
	TagSet         []tag  `xml:"tagSet>item"`
}

func (s *Simulation) importKeyPair(c *ec2Call) (ec2Response, error) {
	name := c.params.get("KeyName")
	if err := checkKeyName(name); err != nil {
		return nil, err
	}
	material, err := base64.StdEncoding.DecodeString(c.params.get("PublicKeyMaterial"))
	if err != nil {
		return nil, errInvalidParameter("Value for parameter PublicKeyMaterial is invalid. Length exceeds maximum of 2048.")
	}
	keyType, fingerprint, public, valid := parsePublicKey(strings.TrimSpace(string(material)))
	if !valid {
		return nil, clientError("InvalidKey.Format", "Key is not in valid OpenSSH public key format")
	}
	specs, err := tagSpecifications(c.params, "key-pair")
	if err != nil {
		return nil, err
	}
	out := &importKeyPairResponse{KeyName: name, KeyFingerprint: fingerprint}
	err = s.store.Update(func(w *world) error {
		if w.keyPair(c.region, name) != nil {
			return clientError("InvalidKeyPair.Duplicate", "The keypair already exists")
		}
		k := &keyPair{ID: newID("key"), Region: c.region, Name: name, Type: keyType, Fingerprint: fingerprint, PublicKey: public, CreateTime: s.now().UTC()}
		w.KeyPairs = append(w.KeyPairs, k)
		for _, t := range specs["key-pair"] {
			w.setTag(k.ID, t)
		}
		out.KeyPairID, out.TagSet = k.ID, w.tagsOf(k.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type createKeyPairResponse struct {
	ec2Meta
	KeyFingerprint string `xml:"keyFingerprint"`
	KeyMaterial    string `xml:"keyMaterial"`
	KeyName        string `xml:"keyName"`
	KeyPairID      string `xml:"keyPairId"`
	TagSet         []tag  `xml:"tagSet>item"`
}

// createKeyPair makes an RSA key pair and returns the private key in PEM, as
// EC2 does. Its fingerprint is the SHA-1 of the private key in PKCS#8.
func (s *Simulation) createKeyPair(c *ec2Call) (ec2Response, error) {
	name := c.params.get("KeyName")
	if err := checkKeyName(name); err != nil {
		return nil, err
	}
	if t := c.params.get("KeyType"); t != "" && t != "rsa" {
		return nil, errUnsupported("Creating %s key pairs", t)
	}
	if f := c.params.get("KeyFormat"); f != "" && f != "pem" {
		return nil, errUnsupported("Key format %s", f)
	}
	specs, err := tagSpecifications(c.params, "key-pair")
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(pkcs8)
	out := &createKeyPairResponse{
		KeyName: name, KeyFingerprint: colonHex(sum[:]),
		KeyMaterial: strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))),
	}
	err = s.store.Update(func(w *world) error {
		if w.keyPair(c.region, name) != nil {
			return clientError("InvalidKeyPair.Duplicate", "The keypair already exists")
		}
		k := &keyPair{ID: newID("key"), Region: c.region, Name: name, Type: "rsa", Fingerprint: out.KeyFingerprint, PublicKey: rsaAuthorizedKey(&key.PublicKey), CreateTime: s.now().UTC()}
		w.KeyPairs = append(w.KeyPairs, k)
		for _, t := range specs["key-pair"] {
			w.setTag(k.ID, t)
		}
		out.KeyPairID, out.TagSet = k.ID, w.tagsOf(k.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rsaAuthorizedKey encodes an RSA public key as an OpenSSH authorized_keys
// line (RFC 4253, section 6.6).
func rsaAuthorizedKey(key *rsa.PublicKey) string {
	var blob bytes.Buffer
	writeString := func(b []byte) {
		_ = binary.Write(&blob, binary.BigEndian, uint32(len(b)))
		blob.Write(b)
	}
	mpint := func(n *big.Int) []byte {
		b := n.Bytes()
		if len(b) > 0 && b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		return b
	}
	writeString([]byte("ssh-rsa"))
	writeString(mpint(big.NewInt(int64(key.E))))
	writeString(mpint(key.N))
	return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob.Bytes())
}

type keyPairXML struct {
	KeyPairID      string `xml:"keyPairId"`
	KeyType        string `xml:"keyType"`
	KeyName        string `xml:"keyName"`
	KeyFingerprint string `xml:"keyFingerprint"`
	PublicKey      string `xml:"publicKey,omitempty"`
	CreateTime     string `xml:"createTime"`
	TagSet         []tag  `xml:"tagSet>item"`
}

type describeKeyPairsResponse struct {
	ec2Meta
	KeyPairs []keyPairXML `xml:"keySet>item"`
}

func (s *Simulation) describeKeyPairs(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	includePublic, _ := c.params.boolean("IncludePublicKey")
	out := &describeKeyPairsResponse{}
	var err error
	s.store.Read(func(w *world) {
		keys := regional(w.KeyPairs, c.region)
		keys, err = selectIDs(keys, func(k *keyPair) string { return k.Name }, c.params.list("KeyName"), func(name string) *apiError {
			return errNotFound("InvalidKeyPair.NotFound", "The key pair '%s' does not exist", name)
		})
		if err == nil {
			keys, err = selectIDs(keys, func(k *keyPair) string { return k.ID }, c.params.list("KeyPairId"), notFoundByID)
		}
		for _, k := range keys {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "key-name":
					return values(k.Name), true
				case "key-pair-id":
					return values(k.ID), true
				case "fingerprint":
					return values(k.Fingerprint), true
				case "key-type":
					return values(k.Type), true
				}
				return nil, false
			}, w.tagsOf(k.ID))
			if !match {
				continue
			}
			x := keyPairXML{KeyPairID: k.ID, KeyType: k.Type, KeyName: k.Name, KeyFingerprint: k.Fingerprint, CreateTime: timestamp(k.CreateTime), TagSet: w.tagsOf(k.ID)}
			if includePublic {
				x.PublicKey = k.PublicKey
			}
			out.KeyPairs = append(out.KeyPairs, x)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type deleteKeyPairResponse struct {
	ec2Meta
	Return    bool   `xml:"return"`
	KeyPairID string `xml:"keyPairId,omitempty"`
}

// deleteKeyPair deletes a key pair by name or id. Deleting a name that does
// not exist succeeds, as in EC2.
func (s *Simulation) deleteKeyPair(c *ec2Call) (ec2Response, error) {
	name, id := c.params.get("KeyName"), c.params.get("KeyPairId")
	if name == "" && id == "" {
		return nil, errMissingParameter("KeyName")
	}
	out := &deleteKeyPairResponse{Return: true}
	err := s.store.Update(func(w *world) error {
		k := find(w.KeyPairs, func(k *keyPair) bool {
			return k.Region == c.region && ((name != "" && k.Name == name) || (id != "" && k.ID == id))
		})
		if k == nil {
			if id != "" {
				return notFoundByID(id)
			}
			return nil
		}
		out.KeyPairID = k.ID
		w.KeyPairs = slices.DeleteFunc(w.KeyPairs, func(x *keyPair) bool { return x == k })
		w.forget(k.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
