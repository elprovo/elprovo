package machines

import (
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
)

// caPath is where a machine keeps the CA certificates that -machine-ca names.
// Ubuntu's update-ca-certificates adds the files in this folder to the
// machine's trust store.
const caPath = "usr/local/share/ca-certificates/stagely-sim.crt"

// Options are what every simulated cloud's machines share.
type Options struct {
	// CACerts is a PEM bundle of CA certificates that every machine trusts,
	// as a cloud image trusts the public CAs.
	CACerts []byte
}

// Flags registers the machines' flags on fs and returns a function that reads
// them after parsing.
func Flags(fs *flag.FlagSet) func() (Options, error) {
	caFile := fs.String("machine-ca", "", "PEM file of CA certificates that every simulated machine trusts, as a cloud image trusts the public CAs")
	return func() (Options, error) {
		if *caFile == "" {
			return Options{}, nil
		}
		certs, err := os.ReadFile(*caFile)
		if err != nil {
			return Options{}, fmt.Errorf("read -machine-ca: %w", err)
		}
		if err := checkCertificates(certs); err != nil {
			return Options{}, fmt.Errorf("-machine-ca %s: %w", *caFile, err)
		}
		return Options{CACerts: certs}, nil
	}
}

func checkCertificates(bundle []byte) error {
	found := false
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("has a %s block; it must hold only certificates", block.Type)
		}
		found = true
	}
	if !found {
		return errors.New("holds no PEM certificate")
	}
	return nil
}
