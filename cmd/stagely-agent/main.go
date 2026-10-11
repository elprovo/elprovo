// Command stagely-agent runs stagelets for one environment, and manages the
// secret values that stay on the agent host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stagely-dev/stagely/internal/agent"
	"github.com/stagely-dev/stagely/internal/agent/build"
	"github.com/stagely-dev/stagely/internal/agent/journal"
	"github.com/stagely-dev/stagely/internal/agent/provider/aws"
	"github.com/stagely-dev/stagely/internal/agent/provider/azure"
	"github.com/stagely-dev/stagely/internal/agent/provider/digitalocean"
	"github.com/stagely-dev/stagely/internal/agent/provider/gcp"
	"github.com/stagely-dev/stagely/internal/agent/provider/hetzner"
	"github.com/stagely-dev/stagely/internal/agent/provider/lima"
	"github.com/stagely-dev/stagely/internal/agent/provider/vultr"
	"github.com/stagely-dev/stagely/internal/agent/secrets"
	"github.com/stagely-dev/stagely/internal/controlplane"
	"github.com/stagely-dev/stagely/protocol"
)

const usage = `usage:
  stagely-agent run [flags]
  stagely-agent secrets set [-data-dir dir] KEY     (reads the value from standard input)
  stagely-agent secrets delete [-data-dir dir] KEY
  stagely-agent secrets list [-data-dir dir]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "secrets":
		err = secretsCommand(os.Args[2:], os.Stdin, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("stagely-agent failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	controlPlane := fs.String("control-plane", "ws://localhost:8080/v1/connect", "control plane session URL")
	dataDir := fs.String("data-dir", defaultDataDir("agent"), "directory for the agent key and secret store")
	enrollmentToken := fs.String("enrollment-token", os.Getenv("STAGELY_ENROLLMENT_TOKEN"), "one-time enrollment token, needed on first start; defaults to STAGELY_ENROLLMENT_TOKEN")
	machinePrefix := fs.String("machine-prefix", "stagely-", "prefix for the names of machines the agent creates")
	maxJobs := fs.Int("max-jobs", 2, "jobs that run at the same time")
	buildWorkers := fs.Int("build-workers", 2, "size of the build-worker pool")
	registry := fs.String("registry", "", "image name prefix in the registry, such as registry.digitalocean.com/acme; required")
	registryUser := fs.String("registry-username", os.Getenv("STAGELY_REGISTRY_USERNAME"), "registry user name, defaults to STAGELY_REGISTRY_USERNAME; the password comes from STAGELY_REGISTRY_PASSWORD; without it, a provider with its own registry hands out short-lived credentials")
	secretsFile := fs.String("secrets-file", "", "dotenv file with secret values from your pipeline; its values win over the secret store")
	provider := fs.String("provider", "lima", "where stagelets run: lima, digitalocean, aws, gcp, hetzner, vultr or azure")
	limaInsecureRegistry := fs.String("lima-insecure-registry", "", "host:port of a plain HTTP registry that Lima VMs may use, for local testing")
	var do digitalocean.Config
	fs.StringVar(&do.APIURL, "do-api-url", "https://api.digitalocean.com", "DigitalOcean API base URL")
	fs.StringVar(&do.Region, "do-region", "", "DigitalOcean region for stagelet droplets")
	fs.StringVar(&do.Size, "do-size", "s-2vcpu-4gb", "DigitalOcean droplet size")
	fs.StringVar(&do.Image, "do-image", "ubuntu-24-04-x64", "stock DigitalOcean droplet image; cloud-init installs Docker on it")
	fs.StringVar(&do.GoldenImage, "do-golden-image", "", "Stagely golden image (snapshot id) with Docker preinstalled; used instead of -do-image")
	fs.StringVar(&do.VPCUUID, "do-vpc", "", "DigitalOcean VPC UUID; droplets join it and the agent reaches them on their private address")
	fs.StringVar(&do.Tag, "do-tag", "stagely-stagelet", "tag on every stagelet droplet")
	var awsCfg aws.Config
	fs.StringVar(&awsCfg.Region, "aws-region", "", "AWS region for stagelet instances, such as eu-central-1")
	fs.StringVar(&awsCfg.EC2URL, "aws-ec2-url", "", "EC2 API endpoint (default https://ec2.<region>.amazonaws.com)")
	fs.StringVar(&awsCfg.ECRURL, "aws-ecr-url", "", "ECR API endpoint (default https://api.ecr.<region>.amazonaws.com)")
	fs.StringVar(&awsCfg.STSURL, "aws-sts-url", "", "STS API endpoint, for the registry's temporary credentials (default https://sts.<region>.amazonaws.com)")
	fs.StringVar(&awsCfg.InstanceType, "aws-instance-type", "t3.medium", "EC2 instance type")
	fs.StringVar(&awsCfg.Image, "aws-image", "", "stock Ubuntu AMI id; cloud-init installs Docker on it")
	fs.StringVar(&awsCfg.GoldenImage, "aws-golden-image", "", "Stagely golden image AMI id with Docker preinstalled; used instead of -aws-image")
	fs.StringVar(&awsCfg.SubnetID, "aws-subnet", "", "subnet id; instances launch into it and the agent reaches them on their private address")
	fs.StringVar(&awsCfg.SecurityGroupID, "aws-security-group", "", "security group id of every stagelet instance; it must let in only the agent and the gateway (required)")
	fs.IntVar(&awsCfg.VolumeSize, "aws-volume-size", 30, "root volume size of every instance in GiB, for images, build cache and stagelet data")
	fs.StringVar(&awsCfg.Group, "aws-group", "stagely-stagelet", "value of the stagely-group tag on every stagelet instance")
	var g gcp.Config
	fs.StringVar(&g.ComputeURL, "gcp-compute-url", "https://compute.googleapis.com/compute/v1/", "Compute Engine API base URL")
	fs.StringVar(&g.Project, "gcp-project", "", "Google Cloud project of the stagelet instances (default the service account key's project)")
	fs.StringVar(&g.Zone, "gcp-zone", "", "Compute Engine zone for stagelet instances, such as us-central1-a")
	fs.StringVar(&g.MachineType, "gcp-machine-type", "e2-medium", "Compute Engine machine type")
	fs.StringVar(&g.Image, "gcp-image", "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64", "stock image; cloud-init installs Docker on it")
	fs.StringVar(&g.GoldenImage, "gcp-golden-image", "", "Stagely golden image (an image name in the project, or its path) with Docker preinstalled; used instead of -gcp-image")
	fs.StringVar(&g.Subnetwork, "gcp-subnetwork", "", "subnetwork that stagelet instances join, such as projects/<project>/regions/<region>/subnetworks/<name> (default the default network)")
	fs.StringVar(&g.Tag, "gcp-tag", "stagely-stagelet", "network tag and stagely label on every stagelet instance")
	fs.StringVar(&g.IAMCredentialsURL, "gcp-iam-credentials-url", "https://iamcredentials.googleapis.com/v1/", "IAM Service Account Credentials API base URL")
	fs.StringVar(&g.RegistryPushAccount, "gcp-registry-push-account", "", "service account that may push to the image repository; build workers sign in with its short-lived tokens")
	fs.StringVar(&g.RegistryPullAccount, "gcp-registry-pull-account", "", "service account that may pull from the image repository; stagelet machines sign in with its short-lived tokens")
	var hz hetzner.Config
	fs.StringVar(&hz.APIURL, "hcloud-api-url", "https://api.hetzner.cloud/v1", "Hetzner Cloud API base URL, with the /v1 path")
	fs.StringVar(&hz.Location, "hcloud-location", "", "Hetzner location for stagelet servers, such as fsn1")
	fs.StringVar(&hz.ServerType, "hcloud-server-type", "cx23", "Hetzner server type")
	fs.StringVar(&hz.Image, "hcloud-image", "ubuntu-24.04", "stock Hetzner image; cloud-init installs Docker on it")
	fs.StringVar(&hz.GoldenImage, "hcloud-golden-image", "", "Stagely golden image (snapshot id) with Docker preinstalled; used instead of -hcloud-image")
	fs.StringVar(&hz.Label, "hcloud-label", "stagely=stagelet", "label, as key=value, on every stagelet server")
	fs.Int64Var(&hz.NetworkID, "hcloud-network", 0, "id of the Hetzner network that every stagelet server joins; the agent reaches servers on their address there")
	fs.TextVar(&hz.Network.Range, "hcloud-network-range", netip.Prefix{}, "address range of -hcloud-network, such as 10.42.0.0/20")
	fs.Func("hcloud-network-allow", "comma-separated addresses on -hcloud-network that may connect to servers: the agent's and the gateway's", addressList(&hz.Network.Sources))
	var vu vultr.Config
	fs.StringVar(&vu.APIURL, "vultr-api-url", "https://api.vultr.com", "Vultr API base URL")
	fs.StringVar(&vu.Region, "vultr-region", "", "Vultr region for stagelet instances")
	fs.StringVar(&vu.Plan, "vultr-plan", "vc2-2c-4gb", "Vultr plan of stagelet instances")
	fs.IntVar(&vu.OSID, "vultr-os", 2284, "stock Vultr operating system id (2284 is Ubuntu 24.04); cloud-init installs Docker on it")
	fs.StringVar(&vu.GoldenImage, "vultr-golden-image", "", "Stagely golden image (snapshot id) with Docker preinstalled; used instead of -vultr-os")
	fs.StringVar(&vu.FirewallGroupID, "vultr-firewall-group", "", "Vultr firewall group id that every stagelet instance joins")
	fs.StringVar(&vu.Tag, "vultr-tag", "stagely-stagelet", "tag on every stagelet instance")
	fs.StringVar(&vu.VPCID, "vultr-vpc", "", "id of the Vultr VPC that every stagelet instance joins; the agent reaches instances on their address there")
	fs.TextVar(&vu.Network.Range, "vultr-vpc-range", netip.Prefix{}, "address range of -vultr-vpc, such as 10.42.0.0/20")
	fs.Func("vultr-vpc-allow", "comma-separated addresses in -vultr-vpc that may connect to instances: the agent's and the gateway's", addressList(&vu.Network.Sources))
	var az azure.Config
	fs.StringVar(&az.ARMURL, "azure-arm-url", "https://management.azure.com", "Azure Resource Manager endpoint; its metadata document names the login endpoint. The service principal comes from AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET")
	fs.StringVar(&az.SubscriptionID, "azure-subscription", "", "Azure subscription id")
	fs.StringVar(&az.ResourceGroup, "azure-resource-group", "", "resource group that stagelet virtual machines go in")
	fs.StringVar(&az.Location, "azure-location", "", "Azure location for stagelet virtual machines, such as westeurope")
	fs.StringVar(&az.SubnetID, "azure-subnet", "", "id of the subnet that stagelet virtual machines join; its network security group admits only the agent and the gateway")
	fs.StringVar(&az.Size, "azure-size", "Standard_B2s", "Azure virtual machine size")
	fs.StringVar(&az.Image, "azure-image", "Canonical:ubuntu-24_04-lts:server:latest", "platform image as publisher:offer:sku:version; cloud-init installs Docker on it")
	fs.StringVar(&az.GoldenImage, "azure-golden-image", "", "id of a Stagely managed image with Docker preinstalled; used instead of -azure-image")
	fs.StringVar(&az.Registry, "azure-registry", "", "id of the container registry that machines get short-lived token credentials for")
	fs.StringVar(&az.Tag, "azure-tag", "stagely-stagelet", "value of the stagely tag on every stagelet virtual machine")
	fs.StringVar(&vu.RegistryID, "vultr-registry", "", "id of the Vultr container registry that machines get short-lived Docker credentials for; without it, machines sign in only with -registry-username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *registry == "" {
		return errors.New("set -registry: build workers push images there and stagelet machines pull them")
	}
	if *buildWorkers < 1 || *buildWorkers > 100 || *maxJobs < 1 {
		return errors.New("-build-workers must be 1 to 100 and -max-jobs at least 1")
	}
	awsCfg.Credentials = aws.Credentials{
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	_, awsCfg.RepositoryPrefix, _ = strings.Cut(*registry, "/")
	machines, credentials, err := newMachines(*provider, *dataDir, do, *limaInsecureRegistry, awsCfg, g, hz, vu, az)
	if err != nil {
		return err
	}
	if *registryUser != "" {
		credentials = agent.StaticCredentials(*registryUser, os.Getenv("STAGELY_REGISTRY_PASSWORD"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	jrnl, err := journal.Open(ctx, filepath.Join(*dataDir, "journal.db"))
	if err != nil {
		return err
	}
	defer func() { _ = jrnl.Close() }()

	runner := &agent.Runner{
		Machines:       machines,
		Journal:        jrnl,
		Source:         &build.Source{HTTP: &http.Client{Timeout: 10 * time.Minute}, MaxBytes: 512 << 20},
		Secrets:        secrets.Resolver{Store: secretStore(*dataDir), File: *secretsFile},
		Registry:       agent.Registry{Prefix: *registry, Credentials: credentials},
		MachinePrefix:  *machinePrefix,
		BuildWorkers:   *buildWorkers,
		MaxJobs:        *maxJobs,
		RenewEvery:     20 * time.Second,
		HealthTimeout:  2 * time.Minute,
		InventoryEvery: 5 * time.Minute,
		Log:            log,
	}
	cfg := controlplane.Config{
		URL:             *controlPlane,
		Role:            protocol.RoleAgent,
		KeyPath:         filepath.Join(*dataDir, "agent.key"),
		EnrollmentToken: *enrollmentToken,
	}
	if err := runner.Recover(ctx); err != nil {
		return err
	}
	err = controlplane.Run(ctx, cfg, log, func(ctx context.Context, s *controlplane.Session) error {
		return runner.Serve(ctx, s)
	})
	runner.Wait()
	return err
}

// addressList returns a flag function that adds the comma-separated IP
// addresses of its value to addrs.
func addressList(addrs *[]netip.Addr) func(string) error {
	return func(value string) error {
		for _, field := range strings.Split(value, ",") {
			addr, err := netip.ParseAddr(strings.TrimSpace(field))
			if err != nil {
				return err
			}
			*addrs = append(*addrs, addr)
		}
		return nil
	}
}

func secretStore(dataDir string) *secrets.Store {
	return secrets.NewStore(filepath.Join(dataDir, "secrets.json"), filepath.Join(dataDir, "secrets.key"))
}

func secretsCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("secrets "+args[0], flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir("agent"), "directory for the agent key and secret store")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	store := secretStore(*dataDir)
	switch {
	case args[0] == "list" && fs.NArg() == 0:
		keys, err := store.Keys()
		for _, k := range keys {
			_, _ = fmt.Fprintln(stdout, k)
		}
		return err
	case args[0] == "set" && fs.NArg() == 1:
		value, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return fmt.Errorf("read the value from standard input: %w", err)
		}
		return store.Set(fs.Arg(0), strings.TrimSuffix(string(value), "\n"))
	case args[0] == "delete" && fs.NArg() == 1:
		return store.Delete(fs.Arg(0))
	default:
		return errors.New(usage)
	}
}

// newMachines returns the provider's machines, and the registry credentials
// it hands out when the cloud has its own registry.
func newMachines(provider, dataDir string, do digitalocean.Config, limaInsecureRegistry string, awsCfg aws.Config, g gcp.Config, hz hetzner.Config, vu vultr.Config, az azure.Config) (agent.Machines, agent.RegistryCredentials, error) {
	switch provider {
	case "lima":
		p := lima.New()
		if limaInsecureRegistry != "" {
			p.InsecureRegistries = []string{limaInsecureRegistry}
		}
		return p, nil, nil
	case "digitalocean":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		do.Token = os.Getenv("DIGITALOCEAN_TOKEN")
		do.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		do.ReadyTimeout = 10 * time.Minute
		p, err := digitalocean.New(do, key)
		if err != nil {
			return nil, nil, err
		}
		return p, p.RegistryCredentials, nil
	case "aws":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		awsCfg.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		awsCfg.ReadyTimeout = 10 * time.Minute
		p, err := aws.New(awsCfg, key)
		if err != nil {
			return nil, nil, err
		}
		return p, p.RegistryCredentials, nil
	case "gcp":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		g.Credentials = []byte(os.Getenv("GOOGLE_CREDENTIALS"))
		g.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		g.ReadyTimeout = 10 * time.Minute
		p, err := gcp.New(g, key)
		if err != nil {
			return nil, nil, err
		}
		return p, p.RegistryCredentials, nil
	case "hetzner":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		hz.Token = os.Getenv("HCLOUD_TOKEN")
		hz.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		hz.ReadyTimeout = 10 * time.Minute
		p, err := hetzner.New(hz, key)
		if err != nil {
			return nil, nil, err
		}
		return p, nil, nil
	case "vultr":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		vu.APIKey = os.Getenv("VULTR_API_KEY")
		vu.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		vu.ReadyTimeout = 10 * time.Minute
		p, err := vultr.New(vu, key)
		if err != nil {
			return nil, nil, err
		}
		if vu.RegistryID == "" {
			return p, nil, nil
		}
		return p, p.RegistryCredentials, nil
	case "azure":
		key, err := controlplane.LoadOrCreateKey(filepath.Join(dataDir, "ssh.key"))
		if err != nil {
			return nil, nil, err
		}
		az.TenantID, az.ClientID, az.ClientSecret = os.Getenv("AZURE_TENANT_ID"), os.Getenv("AZURE_CLIENT_ID"), os.Getenv("AZURE_CLIENT_SECRET")
		az.AdminUsername = "stagely"
		az.HostKeyDir = filepath.Join(dataDir, "hostkeys")
		az.ReadyTimeout = 10 * time.Minute
		p, err := azure.New(az, key)
		if err != nil {
			return nil, nil, err
		}
		if az.Registry == "" {
			return p, nil, nil
		}
		return p, p.RegistryCredentials, nil
	default:
		return nil, nil, fmt.Errorf("unknown provider %q: use lima, digitalocean, aws, gcp, hetzner, vultr or azure", provider)
	}
}

func defaultDataDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".stagely", name)
	}
	return filepath.Join(home, ".stagely", name)
}
