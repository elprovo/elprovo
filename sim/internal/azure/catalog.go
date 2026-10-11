package azure

import "strings"

// vmSize is a virtual machine size the simulation accepts. Sizes are labels:
// machines are not limited to the size's CPUs or memory.
type vmSize struct {
	Name     string
	Cores    int
	MemoryMB int
}

var vmSizes = []vmSize{
	{"Standard_B1s", 1, 1024},
	{"Standard_B1ms", 1, 2048},
	{"Standard_B2s", 2, 4096},
	{"Standard_B2ms", 2, 8192},
	{"Standard_B4ms", 4, 16384},
	{"Standard_B2ts_v2", 2, 1024},
	{"Standard_B2ls_v2", 2, 4096},
	{"Standard_B2s_v2", 2, 8192},
	{"Standard_B4s_v2", 4, 16384},
	{"Standard_B2als_v2", 2, 4096},
	{"Standard_B2as_v2", 2, 8192},
	{"Standard_B4as_v2", 4, 16384},
	{"Standard_D2s_v5", 2, 8192},
	{"Standard_D4s_v5", 4, 16384},
	{"Standard_D8s_v5", 8, 32768},
	{"Standard_D2as_v5", 2, 8192},
	{"Standard_D4as_v5", 4, 16384},
	{"Standard_D2s_v6", 2, 8192},
	{"Standard_D4s_v6", 4, 16384},
	{"Standard_F2s_v2", 2, 4096},
	{"Standard_F4s_v2", 4, 8192},
}

func findSize(name string) (vmSize, bool) {
	for _, s := range vmSizes {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return vmSize{}, false
}

// marketplaceImage is a platform image that boots the simulation's base
// machine image, Ubuntu 24.04.
type marketplaceImage struct {
	Publisher, Offer, Sku string
	// HyperVGeneration is the image's VM generation, V1 or V2.
	HyperVGeneration string
}

// ubuntuVersion is the one version of each platform image, which "latest"
// resolves to.
const ubuntuVersion = "24.04.202609240"

var marketplaceImages = []marketplaceImage{
	{"Canonical", "ubuntu-24_04-lts", "server", "V2"},
	{"Canonical", "ubuntu-24_04-lts", "server-gen1", "V1"},
	{"Canonical", "ubuntu-24_04-lts", "minimal", "V2"},
}

func findMarketplaceImage(publisher, offer, sku, version string) (marketplaceImage, bool) {
	if version != "latest" && version != ubuntuVersion {
		return marketplaceImage{}, false
	}
	for _, img := range marketplaceImages {
		if strings.EqualFold(img.Publisher, publisher) && strings.EqualFold(img.Offer, offer) && strings.EqualFold(img.Sku, sku) {
			return img, true
		}
	}
	return marketplaceImage{}, false
}

// reservedAdminNames are the Linux admin user names that Azure refuses.
var reservedAdminNames = []string{
	"administrator", "admin", "user", "user1", "test", "user2", "test1", "user3", "admin1", "1", "123", "a", "actuser", "adm", "admin2",
	"aspnet", "backup", "console", "david", "guest", "john", "owner", "root", "server", "sql", "support", "support_388945a0", "sys",
	"test2", "test3", "user4", "user5",
}
