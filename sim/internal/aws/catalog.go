package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// canonicalOwner is the account that publishes Ubuntu's official AMIs.
const canonicalOwner = "099720109477"

// zoneLetters are the availability zones of every simulated region.
var zoneLetters = []string{"a", "b", "c"}

type instanceType struct {
	name       string
	vcpus      int
	memoryMiB  int
	burstable  bool
	network    string
	interfaces int
}

var instanceTypes = []instanceType{
	{"t3.nano", 2, 512, true, "Up to 5 Gigabit", 2},
	{"t3.micro", 2, 1024, true, "Up to 5 Gigabit", 2},
	{"t3.small", 2, 2048, true, "Up to 5 Gigabit", 3},
	{"t3.medium", 2, 4096, true, "Up to 5 Gigabit", 3},
	{"t3.large", 2, 8192, true, "Up to 5 Gigabit", 3},
	{"t3.xlarge", 4, 16384, true, "Up to 5 Gigabit", 4},
	{"t3.2xlarge", 8, 32768, true, "Up to 5 Gigabit", 4},
	{"m7i.large", 2, 8192, false, "Up to 12.5 Gigabit", 3},
	{"m7i.xlarge", 4, 16384, false, "Up to 12.5 Gigabit", 4},
	{"c7i.large", 2, 4096, false, "Up to 12.5 Gigabit", 3},
}

// stockImage is a public AMI that every region offers. Its id differs per
// region, as on AWS.
type stockImage struct {
	name         string
	description  string
	creationDate string
}

var stockImages = []stockImage{{
	name:         "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-20260918",
	description:  "Canonical, Ubuntu, 24.04, amd64 noble image",
	creationDate: "2026-09-18T07:21:44.000Z",
}}

type catalog struct {
	regions []string
}

func (c catalog) hasRegion(name string) bool { return slices.Contains(c.regions, name) }

func (c catalog) zones(region string) []string {
	out := make([]string, len(zoneLetters))
	for i, l := range zoneLetters {
		out[i] = region + l
	}
	return out
}

// zoneID returns the id of a zone, such as use1-az1 for us-east-1a.
func zoneID(zone string) string {
	region, letter := zone[:len(zone)-1], zone[len(zone)-1:]
	parts := strings.Split(region, "-")
	short := parts[0]
	for _, p := range parts[1 : len(parts)-1] {
		for _, word := range []string{"north", "south", "east", "west", "central"} {
			if rest, ok := strings.CutPrefix(p, word); ok {
				short += word[:1]
				p = rest
			}
		}
	}
	return short + parts[len(parts)-1] + "-az" + string(rune('1'+strings.Index("abcdef", letter)))
}

func (c catalog) instanceType(name string) (instanceType, bool) {
	i := slices.IndexFunc(instanceTypes, func(t instanceType) bool { return t.name == name })
	if i < 0 {
		return instanceType{}, false
	}
	return instanceTypes[i], true
}

// stockImageID returns the id of a stock image in a region.
func stockImageID(img stockImage, region string) string {
	sum := sha256.Sum256([]byte(img.name + "|" + region))
	return "ami-" + hex.EncodeToString(sum[:])[:17]
}

// stockSnapshotID returns the id of the snapshot behind a stock image.
func stockSnapshotID(img stockImage, region string) string {
	sum := sha256.Sum256([]byte("snapshot|" + img.name + "|" + region))
	return "snap-" + hex.EncodeToString(sum[:])[:17]
}

// regionDomain is the suffix of instance DNS names in a region.
func regionDomain(region string) (private, public string) {
	if region == "us-east-1" {
		return "ec2.internal", "compute-1.amazonaws.com"
	}
	return region + ".compute.internal", region + ".compute.amazonaws.com"
}
