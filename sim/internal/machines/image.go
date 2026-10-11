package machines

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"strings"

	"github.com/stagely-dev/stagely/sim/vm"
)

const imageRepository = "stagely-sim-vm"

// BaseImage returns the reference of the machine image, building it from
// sim/vm when Docker does not have it yet. The tag is a hash of the build
// context, so a changed context builds a new image.
func (b *Backend) BaseImage(ctx context.Context) (string, error) {
	b.imageMu.Lock()
	defer b.imageMu.Unlock()
	if b.image != "" {
		return b.image, nil
	}
	archive, tag, err := buildContext(vm.Context)
	if err != nil {
		return "", err
	}
	ref := imageRepository + ":" + tag
	if _, err := b.docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", ref); err == nil {
		b.image = ref
		return ref, nil
	}
	b.log.Info("building the machine image; the first build downloads Ubuntu packages and takes a few minutes", "image", ref)
	if _, err := b.docker(ctx, bytes.NewReader(archive), "build", "--tag", ref, "-"); err != nil {
		return "", err
	}
	b.image = ref
	return ref, nil
}

func buildContext(fsys fs.FS) ([]byte, string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hash := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: path + "/", Mode: 0o755})
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		hash.Write([]byte(path + "\x00"))
		hash.Write(data)
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), hex.EncodeToString(hash.Sum(nil))[:12], nil
}

func seedArchive(spec Spec, addrs addresses, options Options) ([]byte, error) {
	meta, err := json.Marshal(map[string]any{
		"instance-id":    spec.Seed.InstanceID,
		"local-hostname": spec.Hostname,
		"local-ipv4":     addrs.private,
		"public-ipv4":    addrs.public,
		"public-keys":    append([]string{}, spec.Seed.PublicKeys...),
	})
	if err != nil {
		return nil, err
	}
	firewall, err := json.Marshal(spec.Firewall)
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	if spec.Seed.Metadata != nil {
		metadata = spec.Seed.Metadata(addrs.private, addrs.public)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	type seedFile struct {
		path string
		data []byte
		mode int64
	}
	files := []seedFile{
		{"var/lib/cloud/seed/nocloud/meta-data", meta, 0o644},
		{"var/lib/cloud/seed/nocloud/user-data", []byte(spec.Seed.UserData), 0o600},
		{"var/lib/stagely-vm/firewall.json", firewall, 0o600},
		{"var/lib/stagely-vm/metadata.json", metadataJSON, 0o600},
		// A machine booted from a snapshot must not keep its source's
		// reserved IPs.
		{"var/lib/stagely-vm/addresses.json", []byte(`{"addresses":[]}`), 0o600},
	}
	if len(options.CACerts) > 0 {
		files = append(files, seedFile{caPath, options.CACerts, 0o644})
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.path, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
