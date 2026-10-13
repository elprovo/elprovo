// Package terraform holds the bootstrap modules that `elprovo setup`
// applies, one folder per cloud, so the CLI carries them in its binary.
package terraform

import "embed"

// AWS is the AWS bootstrap module.
//
//go:embed aws/*.tf aws/.terraform.lock.hcl
var AWS embed.FS

// DigitalOcean is the DigitalOcean bootstrap module.
//
//go:embed digitalocean/*.tf digitalocean/.terraform.lock.hcl
var DigitalOcean embed.FS

// GoogleCloud is the Google Cloud bootstrap module.
//
//go:embed google-cloud/*.tf google-cloud/.terraform.lock.hcl
var GoogleCloud embed.FS

// Hetzner is the Hetzner Cloud bootstrap module.
//
//go:embed hetzner/*.tf hetzner/.terraform.lock.hcl
var Hetzner embed.FS

// Lima is the bootstrap module of the local environment on a Mac.
//
//go:embed lima/*.tf lima/.terraform.lock.hcl
var Lima embed.FS

// Vultr is the Vultr bootstrap module.
//
//go:embed vultr/*.tf vultr/.terraform.lock.hcl
var Vultr embed.FS

// Azure is the Azure bootstrap module.
//
//go:embed azure/*.tf azure/.terraform.lock.hcl
var Azure embed.FS
