// Command elprovo is the operator CLI: it signs in to an Elprovo control
// plane and bootstraps a cloud account for an environment.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/elprovo/elprovo/internal/controlplane"
	"github.com/elprovo/elprovo/internal/login"
	"github.com/elprovo/elprovo/internal/setup"
	"github.com/elprovo/elprovo/terraform"
)

const usage = `usage:
  elprovo login [-server URL]          sign in to an Elprovo control plane in your browser
  elprovo whoami                       show who you are signed in as, and in which account
  elprovo setup digitalocean [flags]   bootstrap a DigitalOcean account for an environment
  elprovo setup aws [flags]            bootstrap an AWS account for an environment
  elprovo setup gcp [flags]            bootstrap a Google Cloud project for an environment
  elprovo setup hetzner [flags]        bootstrap a Hetzner Cloud project for an environment
  elprovo setup vultr [flags]          bootstrap a Vultr account for an environment
  elprovo setup azure [flags]          bootstrap an Azure subscription for an environment
  elprovo setup lima [flags]           bootstrap the local environment on this Mac
  elprovo logout                       revoke the sign-in and forget it`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := login.Store{Path: filepath.Join(configDir(), "credentials.json")}
	client := &http.Client{Timeout: 30 * time.Second}
	var err error
	switch os.Args[1] {
	case "login":
		err = loginCommand(ctx, os.Args[2:], store, client, os.Stdout)
	case "whoami":
		err = whoami(ctx, store, client, os.Stdout)
	case "setup":
		err = setupCommand(ctx, os.Args[2:], store, client, os.Stdout)
	case "logout":
		err = logoutCommand(ctx, store, client, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "elprovo:", err)
		os.Exit(1)
	}
}

func logoutCommand(ctx context.Context, store login.Store, client *http.Client, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := login.Logout(ctx, store, client); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Signed out.")
	return nil
}

func loginCommand(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", defaultServer(store), "control plane URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	creds, err := login.Flow{Server: *server, HTTP: client, OpenBrowser: openBrowser, Out: out}.Run(ctx)
	if err != nil {
		return err
	}
	if err := store.Save(creds); err != nil {
		return err
	}
	return whoami(ctx, store, client, out)
}

func whoami(ctx context.Context, store login.Store, client *http.Client, out io.Writer) error {
	s, err := login.Open(store, client)
	if err != nil {
		return err
	}
	var me struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Accounts []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"accounts"`
	}
	if err := s.Do(ctx, http.MethodGet, "/me", nil, &me); err != nil {
		return err
	}
	if len(me.Accounts) != 1 {
		return errors.New("the token is not bound to one account; sign in again")
	}
	_, err = fmt.Fprintf(out, "Signed in to %s as %s, in account %s (%s).\n", s.Server(), me.User.Login, me.Accounts[0].Name, me.Accounts[0].Role)
	return err
}

func setupCommand(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	commands := map[string]func(context.Context, []string, login.Store, *http.Client, io.Writer) error{
		"digitalocean": setupDigitalOcean, "aws": setupAWS, "gcp": setupGCP, "hetzner": setupHetzner,
		"vultr": setupVultr, "azure": setupAzure, "lima": setupLima,
	}
	if len(args) == 0 || commands[args[0]] == nil {
		return errors.New("usage: elprovo setup digitalocean|aws|gcp|hetzner|vultr|azure|lima [flags]; run with -h for the flags")
	}
	return commands[args[0]](ctx, args[1:], store, client, out)
}

func setupDigitalOcean(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup digitalocean", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var do setup.DigitalOcean
	fs.StringVar(&do.Region, "region", "", "DigitalOcean region, such as fra1 (required)")
	fs.StringVar(&do.RegistryName, "registry", "", "name of the DigitalOcean container registry (required)")
	fs.BoolVar(&do.CreateRegistry, "create-registry", true, "create the registry; DigitalOcean allows one per account, so set false to use an existing one")
	fs.StringVar(&do.DNSDomain, "dns-domain", "", "a domain in DigitalOcean DNS that holds the preview domain; setup then adds the wildcard record")
	fs.StringVar(&do.APIEndpoint, "do-api-url", "https://api.digitalocean.com", "DigitalOcean API base URL")
	fs.StringVar(&do.GoldenImage, "golden-image", "", "snapshot id of a golden image built from images/digitalocean; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	do.Token = os.Getenv("DIGITALOCEAN_TOKEN")
	if do.Token == "" {
		return errors.New("set DIGITALOCEAN_TOKEN to a DigitalOcean API token")
	}
	module, err := iofs.Sub(terraform.DigitalOcean, "digitalocean")
	if err != nil {
		return err
	}
	cloud, err := do.Cloud(module, c.run.PreviewDomain)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, do.DNSDomain == "")
}

func setupAWS(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup aws", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var a setup.AWS
	fs.StringVar(&a.Region, "region", "", "AWS region, such as eu-central-1 (required)")
	fs.StringVar(&a.AvailabilityZone, "availability-zone", "", "availability zone of the environment's subnet; AWS chooses when empty")
	fs.StringVar(&a.RegistryPrefix, "registry-prefix", "", "ECR repository name prefix that build workers push under (default the environment name)")
	fs.StringVar(&a.Endpoint, "aws-endpoint", "", "one URL for the EC2, ECR and STS APIs, such as a simulation; AWS when empty")
	fs.StringVar(&a.GoldenImage, "golden-image", "", "AMI id of a golden image built from images/aws; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a.AccessKeyID, a.SecretAccessKey, a.SessionToken = os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")
	if a.AccessKeyID == "" || a.SecretAccessKey == "" {
		return errors.New("set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY to an AWS access key")
	}
	module, err := iofs.Sub(terraform.AWS, "aws")
	if err != nil {
		return err
	}
	cloud, err := a.Cloud(module)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, true)
}

func setupHetzner(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup hetzner", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var h setup.Hetzner
	fs.StringVar(&h.Location, "location", "", "Hetzner location, such as fsn1 (required)")
	fs.StringVar(&h.Registry, "registry", "", "image name prefix in your container registry, such as registry.acme.dev/elprovo; Hetzner has no managed registry (required)")
	fs.StringVar(&h.RegistryUsername, "registry-username", "", "registry user name; the password comes from ELPROVO_REGISTRY_PASSWORD")
	fs.StringVar(&h.Endpoint, "hcloud-api-url", "https://api.hetzner.cloud/v1", "Hetzner Cloud API base URL, with the /v1 path")
	fs.StringVar(&h.GoldenImage, "golden-image", "", "snapshot id of a golden image built from images/hetzner; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h.Token = os.Getenv("HCLOUD_TOKEN")
	if h.Token == "" {
		return errors.New("set HCLOUD_TOKEN to a Hetzner Cloud API token with read and write access")
	}
	h.RegistryPassword = os.Getenv("ELPROVO_REGISTRY_PASSWORD")
	module, err := iofs.Sub(terraform.Hetzner, "hetzner")
	if err != nil {
		return err
	}
	cloud, err := h.Cloud(module)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, true)
}

func setupVultr(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup vultr", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var v setup.Vultr
	fs.StringVar(&v.Region, "region", "", "Vultr region, such as fra (required)")
	fs.StringVar(&v.RegistryName, "registry", "", "name of the Vultr container registry to create; Vultr registry names are global and hold lowercase letters and digits only (required)")
	fs.StringVar(&v.GoldenImage, "golden-image", "", "snapshot id of a golden image built from images/vultr; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	v.APIKey = os.Getenv("VULTR_API_KEY")
	if v.APIKey == "" {
		return errors.New("set VULTR_API_KEY to a Vultr API key")
	}
	module, err := iofs.Sub(terraform.Vultr, "vultr")
	if err != nil {
		return err
	}
	cloud, err := v.Cloud(module)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, true)
}

func setupAzure(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup azure", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var az setup.Azure
	fs.StringVar(&az.Location, "location", "", "Azure location, such as westeurope (required)")
	fs.StringVar(&az.RegistryName, "registry", "", "name of the Azure container registry to create: 5 to 50 letters and digits, unique across Azure (required)")
	fs.StringVar(&az.ARMURL, "azure-arm-url", "https://management.azure.com", "Azure Resource Manager endpoint, whose metadata document describes the cloud")
	fs.StringVar(&az.GoldenImage, "golden-image", "", "id of a managed image built from images/azure; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	az.TenantID, az.ClientID, az.ClientSecret, az.SubscriptionID = os.Getenv("ARM_TENANT_ID"), os.Getenv("ARM_CLIENT_ID"), os.Getenv("ARM_CLIENT_SECRET"), os.Getenv("ARM_SUBSCRIPTION_ID")
	az.AgentClientID, az.AgentClientSecret = os.Getenv("ARM_AGENT_CLIENT_ID"), os.Getenv("ARM_AGENT_CLIENT_SECRET")
	if az.TenantID == "" || az.ClientID == "" || az.ClientSecret == "" || az.SubscriptionID == "" {
		return errors.New("set ARM_TENANT_ID, ARM_CLIENT_ID, ARM_CLIENT_SECRET and ARM_SUBSCRIPTION_ID to a service principal and its subscription")
	}
	module, err := iofs.Sub(terraform.Azure, "azure")
	if err != nil {
		return err
	}
	cloud, err := az.Cloud(module)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, true)
}

func setupGCP(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	fs := flag.NewFlagSet("setup gcp", flag.ExitOnError)
	var c cloudSetup
	c.addFlags(fs)
	var g setup.GoogleCloud
	credentials := fs.String("credentials", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), "service account key file (default $GOOGLE_APPLICATION_CREDENTIALS)")
	fs.StringVar(&g.Project, "project", "", "Google Cloud project ID (default the service account key's project)")
	fs.StringVar(&g.Region, "region", "", "Google Cloud region, such as us-central1 (required)")
	fs.StringVar(&g.Zone, "zone", "", "zone in the region for the hosts and preview machines, such as us-central1-a (required)")
	fs.StringVar(&g.RegistryName, "registry", "", "ID of the Artifact Registry Docker repository that setup creates (required)")
	fs.StringVar(&g.ComputeEndpoint, "gcp-compute-url", "", "Compute Engine API base URL, ending in /compute/v1/; Google's when empty")
	fs.StringVar(&g.ArtifactRegistryEndpoint, "gcp-artifact-registry-url", "", "Artifact Registry API base URL, ending in /v1/; Google's when empty")
	fs.StringVar(&g.IAMEndpoint, "gcp-iam-url", "", "IAM API base URL, ending in /v1/; Google's when empty")
	fs.StringVar(&g.IAMCredentialsEndpoint, "gcp-iam-credentials-url", "", "IAM Service Account Credentials API base URL, ending in /v1/; Google's when empty")
	fs.StringVar(&g.GoldenImage, "golden-image", "", "image built from images/google-cloud; the agent boots machines from it instead of the stock image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *credentials == "" {
		return errors.New("set -credentials or GOOGLE_APPLICATION_CREDENTIALS to a service account key file")
	}
	var err error
	if g.Credentials, err = os.ReadFile(*credentials); err != nil {
		return fmt.Errorf("read the service account key: %w", err)
	}
	module, err := iofs.Sub(terraform.GoogleCloud, "google-cloud")
	if err != nil {
		return err
	}
	cloud, err := g.Cloud(module)
	if err != nil {
		return err
	}
	return c.apply(ctx, cloud, store, client, out, true)
}

// cloudSetup holds what every cloud's setup command shares: the flags of
// setup.Run, and the run itself once the command has its cloud.
type cloudSetup struct {
	run   setup.Run
	cidrs listFlag
}

func (c *cloudSetup) addFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.run.Environment, "environment", "", "environment name in Elprovo; created when missing (required)")
	fs.StringVar(&c.run.PreviewDomain, "preview-domain", "", "domain that preview host names live under, such as previews.acme.dev (required)")
	fs.Var(&c.cidrs, "operator-cidr", "address range that may reach the hosts over SSH, such as 203.0.113.7/32; repeat for more (required)")
	fs.StringVar(&c.run.Binaries, "binaries", "", "directory with elprovo-agent and elprovo-gateway built for the hosts' Linux CPU (required)")
	fs.StringVar(&c.run.ACME.Email, "acme-email", "", "contact email for the gateway's ACME account")
	fs.StringVar(&c.run.ACME.DirectoryURL, "acme-directory", "", "ACME directory URL; Let's Encrypt when empty")
	fs.StringVar(&c.run.ACME.DirectoryCA, "acme-directory-ca", "", "PEM file the gateway trusts for the ACME directory, for test servers")
	fs.StringVar(&c.run.Tofu, "tofu", defaultTofu(), "OpenTofu or Terraform binary")
	fs.StringVar(&c.run.WorkDir, "work-dir", "", "where setup keeps the module state and the SSH keys; keep it for later runs (default ~/.elprovo/cli/setup/<environment>)")
	fs.StringVar(&c.run.SSHPort, "ssh-port", "22", "SSH port of the hosts")
}

// apply signs in, sets up cloud and says where the gateway is. With
// askForRecord, it also asks for the wildcard DNS record that the module
// does not manage.
func (c *cloudSetup) apply(ctx context.Context, cloud setup.Cloud, store login.Store, client *http.Client, out io.Writer, askForRecord bool) error {
	r := c.run
	r.Cloud, r.OperatorCIDRs = cloud, c.cidrs
	if r.WorkDir == "" {
		r.WorkDir = filepath.Join(configDir(), "setup", r.Environment)
	}
	session, err := login.Open(store, client)
	if err != nil {
		return err
	}
	r.API, r.Server, r.Out, r.ReadyTimeout = session, session.Server(), out, 10*time.Minute
	res, err := r.Run(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nEnvironment %s (%s) is ready. Its gateway is at %s.\n", res.Environment.Name, res.Environment.ID, res.GatewayIP)
	if askForRecord {
		_, _ = fmt.Fprintf(out, "Point *.%s at %s with an A record, so preview host names reach the gateway.\n", r.PreviewDomain, res.GatewayIP)
	}
	return nil
}

func setupLima(ctx context.Context, args []string, store login.Store, client *http.Client, out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("elprovo setup lima runs the agent under launchd, on macOS")
	}
	fs := flag.NewFlagSet("setup lima", flag.ExitOnError)
	var l setup.Lima
	fs.StringVar(&l.Environment, "environment", "local", "environment name in Elprovo; created when missing")
	fs.StringVar(&l.PreviewDomain, "preview-domain", "previews.localhost", "domain that preview host names live under")
	fs.IntVar(&l.GatewayPort, "gateway-port", 8443, "HTTPS port of the gateway on this Mac")
	fs.StringVar(&l.HostAddress, "host-address", "host.lima.internal", "address at which Lima VMs reach this Mac")
	fs.IntVar(&l.RegistryPort, "registry-port", 5050, "port of the local image registry; 5050 because macOS AirPlay often holds 5000")
	fs.StringVar(&l.Binaries, "binaries", "bin", "directory with elprovo-agent built for this Mac")
	fs.StringVar(&l.GatewayBinary, "gateway-binary", filepath.Join("bin", "linux-"+runtime.GOARCH, "elprovo-gateway"), "elprovo-gateway built for Linux on this Mac's CPU, for the gateway's Lima VM")
	fs.StringVar(&l.Tofu, "tofu", defaultTofu(), "OpenTofu or Terraform binary")
	fs.StringVar(&l.WorkDir, "work-dir", "", "where setup keeps the module state, the daemons' data and logs (default ~/.elprovo/cli/setup/<environment>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if l.WorkDir == "" {
		l.WorkDir = filepath.Join(configDir(), "setup", l.Environment)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if l.Binaries, err = filepath.Abs(l.Binaries); err != nil {
		return err
	}
	if l.GatewayBinary, err = filepath.Abs(l.GatewayBinary); err != nil {
		return err
	}
	module, err := iofs.Sub(terraform.Lima, "lima")
	if err != nil {
		return err
	}
	session, err := login.Open(store, client)
	if err != nil {
		return err
	}
	l.API, l.Server, l.Module, l.Out = session, session.Server(), module, out
	l.LaunchAgents, l.UID, l.Path = filepath.Join(home, "Library", "LaunchAgents"), os.Getuid(), os.Getenv("PATH")
	l.Launchctl = func(ctx context.Context, args ...string) error {
		if output, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	l.Limactl = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "limactl", args...)
		cmd.Stdin = stdin
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("limactl %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return output, nil
	}
	l.ReadyTimeout = 5 * time.Minute
	env, err := l.Run(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\nEnvironment %s (%s) runs on this Mac. Previews open at https://<id>.%s:%d.\n", env.Name, env.ID, l.PreviewDomain, l.GatewayPort)
	return nil
}

// listFlag collects a repeated string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func defaultTofu() string {
	if _, err := exec.LookPath("tofu"); err == nil {
		return "tofu"
	}
	return "terraform"
}

func defaultServer(store login.Store) string {
	if s := os.Getenv("ELPROVO_SERVER"); s != "" {
		return s
	}
	if creds, err := store.Load(); err == nil {
		return creds.Server
	}
	return controlplane.HostedURL
}

func configDir() string {
	if dir := os.Getenv("ELPROVO_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "elprovo: no home directory; set ELPROVO_CONFIG_DIR:", err)
		os.Exit(1)
	}
	return filepath.Join(home, ".elprovo", "cli")
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return errors.New("refusing to open a non-web URL")
	}
	return exec.Command(name, url).Start() //nolint:noctx // the browser outlives this command
}
