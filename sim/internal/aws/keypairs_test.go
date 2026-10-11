package aws

import (
	"encoding/base64"
	"encoding/pem"
	"testing"
)

func TestImportedKeyPairHasTheFingerprintAWSComputes(t *testing.T) {
	e := newEnv(t)
	var imported struct {
		Fingerprint string `xml:"keyFingerprint"`
		ID          string `xml:"keyPairId"`
	}

	e.mustEC2(&imported, "ImportKeyPair", "KeyName", "agent", "PublicKeyMaterial", base64.StdEncoding.EncodeToString([]byte(agentKey)))

	// ssh-keygen -lf prints SHA256:sfcczA2bW1XmjTMxI4P9CxV3eU4MExH+bCFjw0Obwvk
	// for agentKey; EC2 reports the same digest in padded base64.
	if imported.Fingerprint != "sfcczA2bW1XmjTMxI4P9CxV3eU4MExH+bCFjw0Obwvk=" || imported.ID == "" {
		t.Fatalf("imported key = %+v", imported)
	}
	var found struct {
		Keys []struct {
			Name      string `xml:"keyName"`
			Type      string `xml:"keyType"`
			PublicKey string `xml:"publicKey"`
		} `xml:"keySet>item"`
	}
	e.mustEC2(&found, "DescribeKeyPairs", "KeyName.1", "agent", "IncludePublicKey", "true")
	if len(found.Keys) != 1 || found.Keys[0].Type != "ed25519" || found.Keys[0].PublicKey == "" {
		t.Fatalf("DescribeKeyPairs = %+v", found.Keys)
	}
	if code := e.ec2Error("ImportKeyPair", "KeyName", "agent", "PublicKeyMaterial", base64.StdEncoding.EncodeToString([]byte(agentKey))); code != "InvalidKeyPair.Duplicate" {
		t.Fatalf("second import = %s, want InvalidKeyPair.Duplicate", code)
	}
	e.mustEC2(nil, "DeleteKeyPair", "KeyName", "agent")
	if code := e.ec2Error("DescribeKeyPairs", "KeyName.1", "agent"); code != "InvalidKeyPair.NotFound" {
		t.Fatalf("deleted key = %s, want InvalidKeyPair.NotFound", code)
	}
	e.mustEC2(nil, "DeleteKeyPair", "KeyName", "agent")
}

func TestCreatedKeyPairReturnsAnRSAPrivateKey(t *testing.T) {
	e := newEnv(t)
	var created struct {
		Material string `xml:"keyMaterial"`
	}

	e.mustEC2(&created, "CreateKeyPair", "KeyName", "packer")

	if block, _ := pem.Decode([]byte(created.Material)); block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("key material is not an RSA private key in PEM: %q", created.Material)
	}
	if code := e.ec2Error("CreateKeyPair", "KeyName", "other", "KeyType", "ed25519"); code != "UnsupportedOperation" {
		t.Fatalf("ED25519 key pair = %s, want UnsupportedOperation", code)
	}
}

func TestMalformedPublicKeyIsRefused(t *testing.T) {
	e := newEnv(t)
	for _, key := range []string{"", "ssh-ed25519", "ssh-ed25519 not-base64!", "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTY="} {
		if code := e.ec2Error("ImportKeyPair", "KeyName", "k", "PublicKeyMaterial", base64.StdEncoding.EncodeToString([]byte(key))); code != "InvalidKey.Format" {
			t.Errorf("key %q = %s, want InvalidKey.Format", key, code)
		}
	}
}
