// Package azure runs stagelet machines as Azure virtual machines. The agent
// creates each machine's network interface and virtual machine through
// Azure Resource Manager and hands the machine a host key that the agent
// generated, so it never has to trust an unknown host key. It then drives
// the machine over SSH as root with its own key, on its private address.
package azure

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/stagely-dev/stagely/internal/cloudinit"
	"github.com/stagely-dev/stagely/internal/sshrun"
)

// tagKey is the tag whose value marks the machines of one environment.
const tagKey = "stagely"

// Config says where and how the provider creates virtual machines.
type Config struct {
	// ARMURL is the Resource Manager endpoint whose metadata document names
	// the cloud's login endpoint: https://management.azure.com on Azure.
	ARMURL         string
	TenantID       string
	ClientID       string
	ClientSecret   string
	SubscriptionID string
	ResourceGroup  string
	Location       string
	// SubnetID is the subnet that machines join. Its network security group
	// decides who reaches them.
	SubnetID string
	Size     string
	// Image is a platform image, publisher:offer:sku:version; cloud-init
	// installs Docker on it.
	Image string
	// GoldenImage, when set, is the id of a managed image built from
	// images/azure that already has Docker, Compose and BuildKit. It
	// replaces Image.
	GoldenImage string
	// Registry is the resource id of the environment's container registry,
	// which RegistryCredentials makes tokens for.
	Registry string
	// Tag is the value of the stagely tag on every machine the provider
	// creates. The provider lists and deletes only machines with it.
	Tag string
	// AdminUsername is the admin user that Azure requires. The provider
	// logs in as root.
	AdminUsername string
	HostKeyDir    string
	ReadyTimeout  time.Duration
}

// Provider creates, finds, drives and deletes virtual machines.
type Provider struct {
	api       *api
	cfg       Config
	machines  sshrun.Machines
	pollEvery time.Duration
}

// wireserver is Azure's host at 168.63.129.16, which serves the VM's goal
// state and extension settings and, on port 53, its DNS. Containers run
// untrusted code, so they may reach only its DNS, which containers on
// Docker's default bridge query directly.
var wireserver = cloudinit.HostBlock{Address: "168.63.129.16", ExceptPort: 53}

var (
	machineNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	// machinePackages give a stock image Docker with Compose and BuildKit.
	machinePackages = []string{"docker.io", "docker-compose-v2", "docker-buildx"}
)

// New returns a provider that signs in to machines with key.
func New(cfg Config, key ed25519.PrivateKey) (*Provider, error) {
	required := map[string]string{
		"Resource Manager URL": cfg.ARMURL, "tenant id": cfg.TenantID, "client id": cfg.ClientID, "client secret": cfg.ClientSecret,
		"subscription id": cfg.SubscriptionID, "resource group": cfg.ResourceGroup, "location": cfg.Location, "subnet id": cfg.SubnetID,
		"size": cfg.Size, "image": cfg.Image, "tag": cfg.Tag, "admin user name": cfg.AdminUsername, "host key directory": cfg.HostKeyDir,
	}
	for name, value := range required {
		if value == "" {
			return nil, fmt.Errorf("the Azure provider needs a %s", name)
		}
	}
	if _, err := platformImage(cfg.Image); err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("SSH key: %w", err)
	}
	p := &Provider{
		api: newAPI(cfg.ARMURL, cfg.TenantID, cfg.ClientID, cfg.ClientSecret), cfg: cfg, machines: sshrun.Machines{Signer: signer, Port: "22", HostKeyDir: cfg.HostKeyDir}, pollEvery: 5 * time.Second,
	}
	p.machines.Address = p.Address
	return p, nil
}

type imageReference struct {
	Publisher string `json:"publisher,omitempty"`
	Offer     string `json:"offer,omitempty"`
	Sku       string `json:"sku,omitempty"`
	Version   string `json:"version,omitempty"`
	ID        string `json:"id,omitempty"`
}

func platformImage(urn string) (imageReference, error) {
	parts := strings.Split(urn, ":")
	if len(parts) != 4 || slices.Contains(parts, "") {
		return imageReference{}, fmt.Errorf("image %q is not publisher:offer:sku:version", urn)
	}
	return imageReference{Publisher: parts[0], Offer: parts[1], Sku: parts[2], Version: parts[3]}, nil
}

func (p *Provider) groupPath() string {
	return "/subscriptions/" + p.cfg.SubscriptionID + "/resourceGroups/" + p.cfg.ResourceGroup
}

func (p *Provider) vmPath(name string) string {
	return p.groupPath() + "/providers/Microsoft.Compute/virtualMachines/" + name
}

func (p *Provider) nicPath(name string) string {
	return p.groupPath() + "/providers/Microsoft.Network/networkInterfaces/" + name + "-nic"
}

type virtualMachine struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		ProvisioningState string `json:"provisioningState"`
		NetworkProfile    struct {
			NetworkInterfaces []struct {
				ID string `json:"id"`
			} `json:"networkInterfaces"`
		} `json:"networkProfile"`
	} `json:"properties"`
}

type networkInterface struct {
	Tags       map[string]string `json:"tags"`
	Properties struct {
		IPConfigurations []struct {
			Properties struct {
				PrivateIPAddress string `json:"privateIPAddress"`
			} `json:"properties"`
		} `json:"ipConfigurations"`
	} `json:"properties"`
}

func (n networkInterface) privateIP() string {
	if len(n.Properties.IPConfigurations) == 0 {
		return ""
	}
	return n.Properties.IPConfigurations[0].Properties.PrivateIPAddress
}

// machine returns the provider's virtual machine with the name, or
// errNotFound when there is none with its tag.
func (p *Provider) machine(ctx context.Context, name string) (virtualMachine, error) {
	var vm virtualMachine
	if _, err := p.api.do(ctx, http.MethodGet, p.vmPath(name), computeAPIVersion, nil, &vm); err != nil {
		return vm, err
	}
	if vm.Tags[tagKey] != p.cfg.Tag {
		return vm, errNotFound
	}
	return vm, nil
}

// Exists reports whether a virtual machine with the name and the provider's
// tag exists.
func (p *Provider) Exists(ctx context.Context, name string) (bool, error) {
	_, err := p.machine(ctx, name)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Create creates the machine's network interface and virtual machine and
// waits until Docker Compose runs on it.
func (p *Provider) Create(ctx context.Context, name string) error {
	if !machineNamePattern.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	hostKey, err := p.machines.NewHostKey(name)
	if err != nil {
		return err
	}
	image, _ := platformImage(p.cfg.Image)
	packages := machinePackages
	if p.cfg.GoldenImage != "" {
		image, packages = imageReference{ID: p.cfg.GoldenImage}, nil
	}
	userData, err := cloudinit.MachineConfigBlocking(hostKey, []cloudinit.HostBlock{wireserver}, packages...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	if err := p.refuseForeign(ctx, name); err != nil {
		return err
	}
	nicID, err := p.createInterface(ctx, name)
	if err != nil {
		return err
	}
	// Azure gives the SSH key to the admin user; the agent logs in as root,
	// which cloud-init allows with the same key only without disable_root.
	if err := p.createVM(ctx, name, nicID, image, userData+"disable_root: false\n"); err != nil {
		return err
	}
	return p.waitReady(ctx, name)
}

// refuseForeign fails when the machine's virtual machine or network
// interface exists without the provider's tag. Resource Manager's PUT
// updates an existing resource, so creating over one would take over a
// resource of someone else.
func (p *Provider) refuseForeign(ctx context.Context, name string) error {
	resources := []struct{ path, version string }{{p.vmPath(name), computeAPIVersion}, {p.nicPath(name), networkAPIVersion}}
	for _, r := range resources {
		var existing struct {
			Tags map[string]string `json:"tags"`
		}
		_, err := p.api.do(ctx, http.MethodGet, r.path, r.version, nil, &existing)
		switch {
		case errors.Is(err, errNotFound):
		case err != nil:
			return fmt.Errorf("look up %s: %w", r.path, err)
		case existing.Tags[tagKey] != p.cfg.Tag:
			return fmt.Errorf("%s exists without the tag %s=%s; the agent does not take it over", r.path, tagKey, p.cfg.Tag)
		}
	}
	return nil
}

func (p *Provider) createInterface(ctx context.Context, name string) (string, error) {
	body := map[string]any{
		"location": p.cfg.Location, "tags": map[string]string{tagKey: p.cfg.Tag},
		"properties": map[string]any{"ipConfigurations": []any{map[string]any{
			"name":       "primary",
			"properties": map[string]any{"subnet": map[string]string{"id": p.cfg.SubnetID}, "privateIPAllocationMethod": "Dynamic"},
		}}},
	}
	var nic struct {
		ID string `json:"id"`
	}
	resp, err := p.api.do(ctx, http.MethodPut, p.nicPath(name), networkAPIVersion, body, &nic)
	if err == nil && resp.operation != "" {
		err = p.api.wait(ctx, resp.operation, p.pollEvery)
	}
	if err != nil {
		return "", fmt.Errorf("create network interface of %s: %w", name, err)
	}
	return nic.ID, nil
}

func (p *Provider) createVM(ctx context.Context, name, nicID string, image imageReference, userData string) error {
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.machines.Signer.PublicKey())))
	body := map[string]any{
		"location": p.cfg.Location, "tags": map[string]string{tagKey: p.cfg.Tag},
		"properties": map[string]any{
			"hardwareProfile": map[string]string{"vmSize": p.cfg.Size},
			"storageProfile": map[string]any{
				"imageReference": image,
				"osDisk": map[string]any{
					"name": name + "-osdisk", "createOption": "FromImage", "caching": "ReadWrite", "deleteOption": "Delete",
					"managedDisk": map[string]string{"storageAccountType": "StandardSSD_LRS"},
				},
			},
			"osProfile": map[string]any{
				"computerName": name, "adminUsername": p.cfg.AdminUsername, "customData": base64.StdEncoding.EncodeToString([]byte(userData)),
				"linuxConfiguration": map[string]any{
					"disablePasswordAuthentication": true,
					"ssh": map[string]any{"publicKeys": []any{map[string]string{
						"path": "/home/" + p.cfg.AdminUsername + "/.ssh/authorized_keys", "keyData": public,
					}}},
				},
			},
			"networkProfile": map[string]any{"networkInterfaces": []any{map[string]any{
				"id": nicID, "properties": map[string]any{"primary": true, "deleteOption": "Delete"},
			}}},
		},
	}
	resp, err := p.api.do(ctx, http.MethodPut, p.vmPath(name), computeAPIVersion, body, nil)
	if err == nil && resp.operation != "" {
		err = p.api.wait(ctx, resp.operation, p.pollEvery)
	}
	if err != nil {
		return fmt.Errorf("create virtual machine %s: %w", name, err)
	}
	return nil
}

func (p *Provider) waitReady(ctx context.Context, name string) error {
	for {
		_, err := p.machines.Output(ctx, name, nil, "sh", "-c", "cloud-init status --wait >/dev/null; docker compose version")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("virtual machine %s is not ready after %v: %w", name, p.cfg.ReadyTimeout, err)
		case <-time.After(p.pollEvery):
		}
	}
}

// Address returns the private address of the machine's network interface.
func (p *Provider) Address(ctx context.Context, name string) (string, error) {
	vm, err := p.machine(ctx, name)
	if err != nil {
		return "", fmt.Errorf("virtual machine %s: %w", name, err)
	}
	if state := vm.Properties.ProvisioningState; state != "Succeeded" {
		return "", fmt.Errorf("virtual machine %s is not ready (provisioning state %s)", name, state)
	}
	nics := vm.Properties.NetworkProfile.NetworkInterfaces
	if len(nics) == 0 {
		return "", fmt.Errorf("virtual machine %s has no network interface", name)
	}
	var nic networkInterface
	if _, err := p.api.do(ctx, http.MethodGet, nics[0].ID, networkAPIVersion, nil, &nic); err != nil {
		return "", fmt.Errorf("network interface of %s: %w", name, err)
	}
	if addr := nic.privateIP(); addr != "" {
		return addr, nil
	}
	return "", fmt.Errorf("virtual machine %s has no private address yet", name)
}

// Run runs a command on the machine over SSH.
func (p *Provider) Run(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	return p.machines.Run(ctx, name, stdin, stdout, stderr, args...)
}

// List returns the names of the resource group's virtual machines with the
// provider's tag.
func (p *Provider) List(ctx context.Context) ([]string, error) {
	var names []string
	err := p.api.list(ctx, p.groupPath()+"/providers/Microsoft.Compute/virtualMachines", computeAPIVersion, func(item json.RawMessage) error {
		var vm virtualMachine
		if err := json.Unmarshal(item, &vm); err != nil {
			return fmt.Errorf("decode virtual machine: %w", err)
		}
		if vm.Tags[tagKey] == p.cfg.Tag {
			names = append(names, vm.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list virtual machines: %w", err)
	}
	return names, nil
}

// CopyIn copies a file from the agent host to the machine.
func (p *Provider) CopyIn(ctx context.Context, name, localPath, remotePath string) error {
	return p.machines.CopyIn(ctx, name, localPath, remotePath)
}

// Delete deletes the machine's virtual machine with its OS disk, then its
// network interface, and waits until Azure answers 404 for both. Deleting a
// missing machine succeeds; a virtual machine without the provider's tag is
// not touched.
func (p *Provider) Delete(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.ReadyTimeout)
	defer cancel()
	_, err := p.machine(ctx, name)
	switch {
	case err == nil:
		if err := p.deleteAndConfirm(ctx, p.vmPath(name), computeAPIVersion); err != nil {
			return fmt.Errorf("delete virtual machine %s: %w", name, err)
		}
	case !errors.Is(err, errNotFound):
		return err
	}
	var nic networkInterface
	_, err = p.api.do(ctx, http.MethodGet, p.nicPath(name), networkAPIVersion, nil, &nic)
	switch {
	case err == nil && nic.Tags[tagKey] == p.cfg.Tag:
		if err := p.deleteAndConfirm(ctx, p.nicPath(name), networkAPIVersion); err != nil {
			return fmt.Errorf("delete network interface of %s: %w", name, err)
		}
	case err != nil && !errors.Is(err, errNotFound):
		return err
	}
	if err := p.machines.Forget(name); err != nil {
		return err
	}
	return nil
}

// deleteAndConfirm deletes the resource and polls it until Azure answers
// 404, rather than trusting the delete's answer.
func (p *Provider) deleteAndConfirm(ctx context.Context, path, apiVersion string) error {
	if _, err := p.api.do(ctx, http.MethodDelete, path, apiVersion, nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	for {
		_, err := p.api.do(ctx, http.MethodGet, path, apiVersion, nil, nil)
		if errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("it still exists: %w", errors.Join(ctx.Err(), err))
		case <-time.After(p.pollEvery):
		}
	}
}
