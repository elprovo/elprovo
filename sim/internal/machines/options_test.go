package machines

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCertificatePEM = `-----BEGIN CERTIFICATE-----
MIIBKDCBz6ADAgECAgEBMAoGCCqGSM49BAMCMBQxEjAQBgNVBAMTCXRlc3QgY2Eg
MB4XDTI2MDEwMTAwMDAwMFoXDTM2MDEwMTAwMDAwMFowFDESMBAGA1UEAxMJdGVz
dCBjYSAwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAQ=
-----END CERTIFICATE-----
`

func TestMachineCAFlagAcceptsOnlyCertificates(t *testing.T) {
	tests := []struct {
		name, contents, wantErr string
	}{
		{"certificate bundle", testCertificatePEM + testCertificatePEM, ""},
		{"private key", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "PRIVATE KEY block"},
		{"no PEM", "not a certificate\n", "holds no PEM certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(file, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			read := Flags(fs)
			if err := fs.Parse([]string{"-machine-ca", file}); err != nil {
				t.Fatal(err)
			}

			options, err := read()

			if tt.wantErr == "" && (err != nil || string(options.CACerts) != tt.contents) {
				t.Fatalf("read = %q, %v; want the bundle", options.CACerts, err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("read error = %v, want one that says %q", err, tt.wantErr)
			}
		})
	}
}
