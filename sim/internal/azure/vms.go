package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const (
	// maxCustomData is the longest base64 custom data Azure accepts.
	maxCustomData = 87380
	osDiskSizeGB  = 30
)

var (
	vmNamePattern    = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,62}[a-zA-Z0-9_])?$`)
	adminNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,63}$`)
	diskSKUs         = []string{"Standard_LRS", "StandardSSD_LRS", "Premium_LRS", "StandardSSD_ZRS", "Premium_ZRS"}
)

type vm struct {
	meta
	Zones             []string     `json:"zones,omitempty"`
	VMID              string       `json:"vm_id"`
	Properties        vmProperties `json:"properties"`
	ProvisioningState string       `json:"provisioning_state"`
	PowerState        string       `json:"power_state"`
	Generalized       bool         `json:"generalized,omitempty"`
	Machine           string       `json:"machine,omitempty"`
	ImageVersion      string       `json:"image_version,omitempty"`
	HyperVGeneration  string       `json:"hyper_v_generation"`
	TimeCreated       time.Time    `json:"time_created"`
}

type vmProperties struct {
	HardwareProfile         hardwareProfile `json:"hardwareProfile"`
	StorageProfile          storageProfile  `json:"storageProfile"`
	OSProfile               *osProfile      `json:"osProfile,omitempty"`
	NetworkProfile          networkProfile  `json:"networkProfile"`
	DiagnosticsProfile      json.RawMessage `json:"diagnosticsProfile,omitempty"`
	SecurityProfile         json.RawMessage `json:"securityProfile,omitempty"`
	AdditionalCapabilities  json.RawMessage `json:"additionalCapabilities,omitempty"`
	ExtensionsTimeBudget    string          `json:"extensionsTimeBudget,omitempty"`
	LicenseType             string          `json:"licenseType,omitempty"`
	UserData                string          `json:"userData,omitempty"`
	Priority                string          `json:"priority,omitempty"`
	AvailabilitySet         *subResource    `json:"availabilitySet,omitempty"`
	ProximityPlacementGroup *subResource    `json:"proximityPlacementGroup,omitempty"`
	Host                    *subResource    `json:"host,omitempty"`
	HostGroup               *subResource    `json:"hostGroup,omitempty"`
	VirtualMachineScaleSet  *subResource    `json:"virtualMachineScaleSet,omitempty"`
	CapacityReservation     json.RawMessage `json:"capacityReservation,omitempty"`
	VMID                    string          `json:"vmId,omitempty"`
	ProvisioningState       string          `json:"provisioningState,omitempty"`
	TimeCreated             string          `json:"timeCreated,omitempty"`
	InstanceView            *instanceView   `json:"instanceView,omitempty"`
}

type hardwareProfile struct {
	VMSize string `json:"vmSize"`
}

type storageProfile struct {
	ImageReference     *imageReference `json:"imageReference,omitempty"`
	OSDisk             osDisk          `json:"osDisk"`
	DataDisks          []any           `json:"dataDisks"`
	DiskControllerType string          `json:"diskControllerType,omitempty"`
}

type imageReference struct {
	Publisher               string `json:"publisher,omitempty"`
	Offer                   string `json:"offer,omitempty"`
	Sku                     string `json:"sku,omitempty"`
	Version                 string `json:"version,omitempty"`
	ExactVersion            string `json:"exactVersion,omitempty"`
	ID                      string `json:"id,omitempty"`
	SharedGalleryImageID    string `json:"sharedGalleryImageId,omitempty"`
	CommunityGalleryImageID string `json:"communityGalleryImageId,omitempty"`
}

type osDisk struct {
	OSType                  string       `json:"osType,omitempty"`
	Name                    string       `json:"name,omitempty"`
	Caching                 string       `json:"caching,omitempty"`
	CreateOption            string       `json:"createOption"`
	DiskSizeGB              int          `json:"diskSizeGB,omitempty"`
	ManagedDisk             *managedDisk `json:"managedDisk,omitempty"`
	DeleteOption            string       `json:"deleteOption,omitempty"`
	WriteAcceleratorEnabled bool         `json:"writeAcceleratorEnabled,omitempty"`
	Vhd                     *struct {
		URI string `json:"uri"`
	} `json:"vhd,omitempty"`
	DiffDiskSettings json.RawMessage `json:"diffDiskSettings,omitempty"`
}

type managedDisk struct {
	ID                 string          `json:"id,omitempty"`
	StorageAccountType string          `json:"storageAccountType,omitempty"`
	DiskEncryptionSet  json.RawMessage `json:"diskEncryptionSet,omitempty"`
	SecurityProfile    json.RawMessage `json:"securityProfile,omitempty"`
}

type osProfile struct {
	ComputerName                string              `json:"computerName"`
	AdminUsername               string              `json:"adminUsername"`
	AdminPassword               string              `json:"adminPassword,omitempty"`
	CustomData                  string              `json:"customData,omitempty"`
	LinuxConfiguration          *linuxConfiguration `json:"linuxConfiguration,omitempty"`
	WindowsConfiguration        json.RawMessage     `json:"windowsConfiguration,omitempty"`
	Secrets                     []any               `json:"secrets"`
	AllowExtensionOperations    *bool               `json:"allowExtensionOperations,omitempty"`
	RequireGuestProvisionSignal *bool               `json:"requireGuestProvisionSignal,omitempty"`
}

type linuxConfiguration struct {
	DisablePasswordAuthentication bool            `json:"disablePasswordAuthentication"`
	SSH                           *sshConfig      `json:"ssh,omitempty"`
	ProvisionVMAgent              *bool           `json:"provisionVMAgent,omitempty"`
	PatchSettings                 json.RawMessage `json:"patchSettings,omitempty"`
	EnableVMAgentPlatformUpdates  *bool           `json:"enableVMAgentPlatformUpdates,omitempty"`
}

type sshConfig struct {
	PublicKeys []sshPublicKey `json:"publicKeys"`
}

type sshPublicKey struct {
	Path    string `json:"path"`
	KeyData string `json:"keyData"`
}

type networkProfile struct {
	NetworkInterfaces              []nicReference `json:"networkInterfaces"`
	NetworkInterfaceConfigurations []any          `json:"networkInterfaceConfigurations,omitempty"`
	NetworkAPIVersion              string         `json:"networkApiVersion,omitempty"`
}

type nicReference struct {
	ID         string `json:"id"`
	Properties *struct {
		Primary      *bool  `json:"primary,omitempty"`
		DeleteOption string `json:"deleteOption,omitempty"`
	} `json:"properties,omitempty"`
}

func (r nicReference) deleteOption() string {
	if r.Properties == nil || r.Properties.DeleteOption == "" {
		return "Detach"
	}
	return r.Properties.DeleteOption
}

type instanceView struct {
	ComputerName     string           `json:"computerName,omitempty"`
	OSName           string           `json:"osName,omitempty"`
	OSVersion        string           `json:"osVersion,omitempty"`
	HyperVGeneration string           `json:"hyperVGeneration,omitempty"`
	Disks            []diskView       `json:"disks"`
	Statuses         []instanceStatus `json:"statuses"`
}

type diskView struct {
	Name     string           `json:"name"`
	Statuses []instanceStatus `json:"statuses"`
}

type instanceStatus struct {
	Code          string `json:"code"`
	Level         string `json:"level"`
	DisplayStatus string `json:"displayStatus"`
	Time          string `json:"time,omitempty"`
}

func (s *Simulation) renderVM(v *vm, expand string) resourceJSON {
	props := v.Properties
	props.VMID, props.ProvisioningState, props.TimeCreated = v.VMID, v.ProvisioningState, timestamp(v.TimeCreated)
	props.StorageProfile.DataDisks = []any{}
	if ref := props.StorageProfile.ImageReference; ref != nil && ref.ID == "" {
		c := *ref
		c.ExactVersion = v.ImageVersion
		props.StorageProfile.ImageReference = &c
	}
	if props.OSProfile != nil {
		c := *props.OSProfile
		c.AdminPassword, c.CustomData, c.Secrets = "", "", []any{}
		props.OSProfile = &c
	}
	if !strings.Contains(strings.ToLower(expand), "userdata") {
		props.UserData = ""
	}
	if strings.Contains(strings.ToLower(expand), "instanceview") {
		view := s.instanceView(v)
		props.InstanceView = &view
	}
	return resourceJSON{ID: s.idFor(typeVM, &v.meta), Name: v.Name, Type: typeVM, Location: v.Location, Tags: cloneTags(v.Tags), Zones: v.Zones, Properties: props}
}

func (s *Simulation) instanceView(v *vm) instanceView {
	now := timestamp(s.now())
	state := strings.ToLower(v.ProvisioningState)
	statuses := []instanceStatus{{Code: "ProvisioningState/" + state, Level: "Info", DisplayStatus: "Provisioning " + state, Time: now}}
	if v.PowerState != "" {
		statuses = append(statuses, instanceStatus{Code: "PowerState/" + v.PowerState, Level: "Info", DisplayStatus: powerDisplay(v.PowerState)})
	}
	if v.Generalized {
		statuses = append(statuses, instanceStatus{Code: "OSState/generalized", Level: "Info", DisplayStatus: "VM generalized"})
	}
	view := instanceView{OSName: "ubuntu", OSVersion: "24.04", HyperVGeneration: v.HyperVGeneration, Statuses: statuses}
	if v.Properties.OSProfile != nil {
		view.ComputerName = v.Properties.OSProfile.ComputerName
	}
	view.Disks = []diskView{{Name: v.Properties.StorageProfile.OSDisk.Name, Statuses: []instanceStatus{{Code: "ProvisioningState/succeeded", Level: "Info", DisplayStatus: "Provisioning succeeded", Time: now}}}}
	return view
}

func powerDisplay(state string) string {
	switch state {
	case "deallocated":
		return "VM deallocated"
	case "deallocating":
		return "VM deallocating"
	default:
		return "VM " + state
	}
}

// vmPlan is a validated virtual machine create.
type vmPlan struct {
	props      vmProperties
	customData string
	image      string
	imageRef   *imageReference
	version    string
	hyperV     string
	diskSKU    string
	privateIP  string
}

// planVM validates a create request and fills in Azure's defaults.
func (s *Simulation) planVM(sub *subscription, group, name, location string, req resourceRequest[vmProperties]) (vmPlan, error) {
	props := req.Properties
	plan := vmPlan{props: props}
	switch {
	case !vmNamePattern.MatchString(name):
		return plan, errInvalidParameter("Virtual machine name '%s' is invalid. A Linux virtual machine name has 1 to 64 letters, digits, '.', '_' or '-', and cannot end with '.' or '-'.", name)
	case len(req.Identity) > 0 && !strings.Contains(string(req.Identity), `"None"`) && string(req.Identity) != "null":
		return plan, errUnsupported("A managed identity on a virtual machine")
	case len(req.Plan) > 0 && string(req.Plan) != "null":
		return plan, errUnsupported("A marketplace plan")
	case props.Priority != "" && !strings.EqualFold(props.Priority, "Regular"):
		return plan, errUnsupported("The %s priority", props.Priority)
	case props.AvailabilitySet != nil, props.ProximityPlacementGroup != nil, props.Host != nil, props.HostGroup != nil, props.VirtualMachineScaleSet != nil,
		len(props.CapacityReservation) > 0 && string(props.CapacityReservation) != "null":
		return plan, errUnsupported("Availability sets, placement groups, dedicated hosts, scale sets and capacity reservations")
	case len(props.StorageProfile.DataDisks) > 0:
		return plan, errUnsupported("A data disk")
	case len(props.NetworkProfile.NetworkInterfaceConfigurations) > 0 || props.NetworkProfile.NetworkAPIVersion != "":
		return plan, errUnsupported("Network interface configurations in a virtual machine; create the network interface first")
	}
	if _, ok := findSize(props.HardwareProfile.VMSize); !ok {
		return plan, errBadRequest("InvalidParameter", "The value %s provided for the VM size is not valid. The simulation's sizes are listed in its README.", props.HardwareProfile.VMSize)
	}
	if err := s.planImage(sub, location, &plan); err != nil {
		return plan, err
	}
	if err := s.planOSDisk(sub, group, name, &plan); err != nil {
		return plan, err
	}
	if err := planOSProfile(name, &plan); err != nil {
		return plan, err
	}
	if props.UserData != "" {
		if _, err := base64.StdEncoding.DecodeString(props.UserData); err != nil {
			return plan, errInvalidParameter("The userData of virtual machine %s is not valid base64.", name)
		}
	}
	return plan, s.planNICs(sub, s.resourceID(group, typeVM, name), location, &plan)
}

func (s *Simulation) planImage(sub *subscription, location string, plan *vmPlan) error {
	ref := plan.props.StorageProfile.ImageReference
	if ref == nil {
		return errUnsupported("A virtual machine without an image reference")
	}
	if ref.SharedGalleryImageID != "" || ref.CommunityGalleryImageID != "" {
		return errUnsupported("A gallery image")
	}
	if ref.ID != "" {
		img := s.imageByID(sub, ref.ID)
		if img == nil {
			return errNotFound(typeImage, ref.ID, "")
		}
		if img.ProvisioningState != statusSucceeded {
			return errConflict("OperationNotAllowed", "The image %s is not ready yet.", ref.ID)
		}
		if img.Location != location {
			return errBadRequest("InvalidParameter", "The image %s is in location %s; the virtual machine must be in the same location.", ref.ID, img.Location)
		}
		plan.image, plan.hyperV = img.ImageRef, img.HyperVGeneration
		plan.imageRef = &imageReference{ID: s.idFor(typeImage, &img.meta)}
		return nil
	}
	img, ok := findMarketplaceImage(ref.Publisher, ref.Offer, ref.Sku, ref.Version)
	if !ok {
		return errBadRequest("PlatformImageNotFound", "The platform image '%s:%s:%s:%s' is not available. Verify that all fields in the storage profile are correct.", ref.Publisher, ref.Offer, ref.Sku, ref.Version)
	}
	plan.version, plan.hyperV = ubuntuVersion, img.HyperVGeneration
	plan.imageRef = &imageReference{Publisher: ref.Publisher, Offer: ref.Offer, Sku: ref.Sku, Version: ref.Version}
	return nil
}

func (s *Simulation) planOSDisk(sub *subscription, group, name string, plan *vmPlan) error {
	d := &plan.props.StorageProfile.OSDisk
	switch {
	case d.Vhd != nil:
		return errUnsupported("An unmanaged OS disk")
	case len(d.DiffDiskSettings) > 0 && string(d.DiffDiskSettings) != "null":
		return errUnsupported("An ephemeral OS disk")
	case !strings.EqualFold(d.CreateOption, "FromImage"):
		return errUnsupported("The OS disk create option %s; use FromImage", d.CreateOption)
	case d.OSType != "" && !strings.EqualFold(d.OSType, "Linux"):
		return errUnsupported("The OS type %s", d.OSType)
	}
	d.CreateOption, d.OSType = "FromImage", "Linux"
	if d.Caching == "" {
		d.Caching = "ReadWrite"
	}
	if d.DeleteOption == "" {
		d.DeleteOption = "Detach"
	}
	if _, ok := canonical(d.DeleteOption, "Delete", "Detach"); !ok {
		return errInvalidParameter("The OS disk delete option %s is not valid; use Delete or Detach.", d.DeleteOption)
	}
	if d.DiskSizeGB == 0 {
		d.DiskSizeGB = osDiskSizeGB
	}
	if d.DiskSizeGB < osDiskSizeGB {
		return errBadRequest("ResizeDiskError", "Disk size %d GB is smaller than the size of the image, %d GB.", d.DiskSizeGB, osDiskSizeGB)
	}
	if d.Name == "" {
		d.Name = name + "_OsDisk_1_" + strings.ReplaceAll(newUUID(), "-", "")
	}
	if lookup(sub.Disks, group, d.Name) != nil {
		return errConflict("ConflictingUserInput", "Disk %s already exists in resource group %s. Only CreateOption.Attach is supported.", d.Name, group)
	}
	plan.diskSKU = "Standard_LRS"
	if d.ManagedDisk != nil {
		if d.ManagedDisk.StorageAccountType != "" {
			sku, ok := canonical(d.ManagedDisk.StorageAccountType, diskSKUs...)
			if !ok {
				return errUnsupported("The disk storage account type %s", d.ManagedDisk.StorageAccountType)
			}
			plan.diskSKU = sku
		}
		if len(d.ManagedDisk.DiskEncryptionSet) > 0 && string(d.ManagedDisk.DiskEncryptionSet) != "null" {
			return errUnsupported("A disk encryption set")
		}
	}
	d.ManagedDisk = &managedDisk{StorageAccountType: plan.diskSKU, ID: s.resourceID(group, typeDisk, d.Name)}
	return nil
}

func planOSProfile(name string, plan *vmPlan) error {
	p := plan.props.OSProfile
	if p == nil {
		return errInvalidParameter("Required parameter 'osProfile' is missing (null).")
	}
	switch {
	case len(p.WindowsConfiguration) > 0 && string(p.WindowsConfiguration) != "null":
		return errUnsupported("A Windows virtual machine")
	case len(p.Secrets) > 0:
		return errUnsupported("Key Vault secrets on a virtual machine")
	case p.AdminPassword != "" || (p.LinuxConfiguration != nil && !p.LinuxConfiguration.DisablePasswordAuthentication):
		return errUnsupported("Password authentication; use SSH public keys")
	case slices.Contains(reservedAdminNames, strings.ToLower(p.AdminUsername)):
		return errBadRequest("InvalidParameter", "The Admin Username specified is not allowed.")
	case !adminNamePattern.MatchString(p.AdminUsername):
		return errBadRequest("InvalidParameter", "The admin user name %q is not a valid Linux user name.", p.AdminUsername)
	case len(p.CustomData) > maxCustomData:
		return errBadRequest("InvalidParameter", "Custom data in OSProfile must be in Base64 encoding and with a maximum length of %d characters.", maxCustomData)
	}
	data, err := base64.StdEncoding.DecodeString(p.CustomData)
	if err != nil {
		return errBadRequest("InvalidParameter", "Custom data in OSProfile must be in Base64 encoding and with a maximum length of %d characters.", maxCustomData)
	}
	plan.customData = string(data)
	if p.ComputerName == "" {
		p.ComputerName = name
	}
	if len(p.ComputerName) > 64 {
		return errInvalidParameter("Linux computer name cannot be more than 64 characters long.")
	}
	if p.LinuxConfiguration == nil || p.LinuxConfiguration.SSH == nil || len(p.LinuxConfiguration.SSH.PublicKeys) == 0 {
		return errBadRequest("InvalidParameter", "Authentication using either SSH or by user name and password must be enabled in Linux profile.")
	}
	want := "/home/" + p.AdminUsername + "/.ssh/authorized_keys"
	for _, k := range p.LinuxConfiguration.SSH.PublicKeys {
		if k.Path != want {
			return errBadRequest("InvalidParameter", "Destination path for SSH public keys is currently limited to its default value %s due to a known issue in Linux provisioning agent.", want)
		}
		if !strings.HasPrefix(k.KeyData, "ssh-rsa ") && !strings.HasPrefix(k.KeyData, "ssh-ed25519 ") {
			return errBadRequest("InvalidParameter", "The value of parameter linuxConfiguration.ssh.publicKeys.keyData is invalid.")
		}
	}
	allow, provision := true, true
	p.AllowExtensionOperations, p.LinuxConfiguration.ProvisionVMAgent = &allow, &provision
	p.RequireGuestProvisionSignal, p.CustomData = &provision, ""
	return nil
}

func (s *Simulation) planNICs(sub *subscription, vmID, location string, plan *vmPlan) error {
	refs := plan.props.NetworkProfile.NetworkInterfaces
	switch {
	case len(refs) == 0:
		return errInvalidParameter("Virtual machine %s must have at least one network interface.", vmID)
	case len(refs) > 1:
		return errUnsupported("A virtual machine with more than one network interface")
	}
	n := s.nicByID(sub, refs[0].ID)
	if n == nil {
		return s.missingReference(refs[0].ID, vmID)
	}
	if user := s.nicUser(sub, s.idFor(typeNIC, &n.meta)); user != nil && !sameID(s.idFor(typeVM, &user.meta), vmID) {
		return errBadRequest("NicInUse", "Network Interface %s is used by existing resource %s.", refs[0].ID, s.idFor(typeVM, &user.meta))
	}
	if n.Location != location {
		return errBadRequest("NicNotInSameLocation", "Network interface %s is in location %s, but the virtual machine is in location %s.", refs[0].ID, n.Location, location)
	}
	ref := nicReference{ID: s.idFor(typeNIC, &n.meta)}
	option := refs[0].deleteOption()
	if _, ok := canonical(option, "Delete", "Detach"); !ok {
		return errInvalidParameter("The network interface delete option %s is not valid; use Delete or Detach.", option)
	}
	primary := true
	ref.Properties = &struct {
		Primary      *bool  `json:"primary,omitempty"`
		DeleteOption string `json:"deleteOption,omitempty"`
	}{&primary, option}
	plan.props.NetworkProfile.NetworkInterfaces = []nicReference{ref}
	plan.privateIP = n.IPConfigurations[0].Properties.PrivateIPAddress
	return nil
}

func (s *Simulation) putVM(r *http.Request, p params) (reply, error) {
	var req resourceRequest[vmProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	var out resourceJSON
	var op *operation
	var job *provisioning
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		if v := lookup(sub.VMs, g.Name, p["name"]); v != nil {
			if v.ProvisioningState == "Deleting" || v.ProvisioningState == "Creating" {
				return errConflict("OperationNotAllowed", "Operation 'PUT' is not allowed on virtual machine %s while it is %s.", v.Name, strings.ToLower(v.ProvisioningState))
			}
			if !samePlan(v.Properties, req.Properties) {
				return errUnsupported("Changing virtual machine %s with PUT; only its tags can change", v.Name)
			}
			v.Tags = cloneTags(req.Tags)
			op = s.doneOperation(sub, "Microsoft.Compute", location)
			out = s.renderVM(v, "")
			return nil
		}
		plan, err := s.planVM(sub, g.Name, p["name"], location, req)
		if err != nil {
			return err
		}
		v := &vm{
			meta:  meta{Group: g.Name, Name: p["name"], Location: location, Tags: cloneTags(req.Tags), GUID: newUUID()},
			Zones: req.Zones, VMID: newUUID(), Properties: plan.props, ProvisioningState: "Creating", PowerState: "starting",
			ImageVersion: plan.version, HyperVGeneration: plan.hyperV, TimeCreated: s.now().UTC(),
		}
		v.Properties.StorageProfile.ImageReference = plan.imageRef
		v.Properties.DiagnosticsProfile = meaningful(v.Properties.DiagnosticsProfile)
		v.Properties.SecurityProfile = meaningful(v.Properties.SecurityProfile)
		v.Properties.AdditionalCapabilities = meaningful(v.Properties.AdditionalCapabilities)
		sub.VMs = append(sub.VMs, v)
		sub.Disks = append(sub.Disks, s.osDiskOf(v, plan))
		op = s.startOperation(sub, "Microsoft.Compute", location)
		job = &provisioning{group: v.Group, name: v.Name, opID: op.ID, spec: s.machineSpec(sub, v, plan)}
		status = http.StatusCreated
		out = s.renderVM(v, "")
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	if job != nil {
		s.background(func(ctx context.Context) { s.provision(ctx, *job) })
	}
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// samePlan reports whether a PUT on an existing virtual machine asks for the
// same machine: size, image, OS disk name, admin user and network
// interfaces.
func samePlan(have vmProperties, want vmProperties) bool {
	if !strings.EqualFold(have.HardwareProfile.VMSize, want.HardwareProfile.VMSize) {
		return false
	}
	if want.OSProfile != nil && have.OSProfile != nil && want.OSProfile.AdminUsername != have.OSProfile.AdminUsername {
		return false
	}
	if n := want.StorageProfile.OSDisk.Name; n != "" && !strings.EqualFold(n, have.StorageProfile.OSDisk.Name) {
		return false
	}
	if len(want.NetworkProfile.NetworkInterfaces) != len(have.NetworkProfile.NetworkInterfaces) {
		return false
	}
	for i, ref := range want.NetworkProfile.NetworkInterfaces {
		if !sameID(ref.ID, have.NetworkProfile.NetworkInterfaces[i].ID) {
			return false
		}
	}
	return true
}

// meaningful drops an object the client sent with nothing in it, which
// Azure does not echo.
func meaningful(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || len(fields) == 0 {
		return nil
	}
	return raw
}

type provisioning struct {
	group, name, opID string
	spec              machines.Spec
}

func (s *Simulation) machineSpec(sub *subscription, v *vm, plan vmPlan) machines.Spec {
	keys := v.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys
	var publicKeys []string
	for _, k := range keys {
		publicKeys = append(publicKeys, k.KeyData)
	}
	host := v.Properties.OSProfile.ComputerName
	if len(host) > 63 {
		host = host[:63]
	}
	docs := s.instanceMetadata(sub, v)
	return machines.Spec{
		Name:     "stagely-sim-az-" + sub.Account[:8] + "-" + v.VMID,
		Hostname: host,
		Cloud:    cloudLabel, Account: sub.Account, Resource: v.VMID,
		Image:     plan.image,
		PrivateIP: plan.privateIP,
		Seed: machines.Seed{
			InstanceID: v.VMID, PublicKeys: publicKeys, UserData: plan.customData,
			Metadata: func(string, string) map[string]string { return docs },
		},
		Firewall: s.policyFor(sub, v),
	}
}

// adminUserScript does what Azure's Linux provisioning does with the OS
// profile: it creates the admin user with passwordless sudo and installs the
// SSH public keys, read from stdin, as its authorized keys.
const adminUserScript = `set -eu
user=$1
id -u "$user" >/dev/null 2>&1 || useradd --create-home --shell /bin/bash --user-group --groups sudo "$user"
install -d -m 0700 -o "$user" -g "$user" "/home/$user/.ssh"
cat > "/home/$user/.ssh/authorized_keys"
chown "$user:$user" "/home/$user/.ssh/authorized_keys"
chmod 0600 "/home/$user/.ssh/authorized_keys"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$user" > /etc/sudoers.d/90-cloud-init-users
chmod 0440 /etc/sudoers.d/90-cloud-init-users
`

func (s *Simulation) provision(ctx context.Context, job provisioning) {
	m, err := s.machines.Create(ctx, job.spec)
	if err == nil {
		err = s.provisionGuest(ctx, job, m.ID)
	}
	if err != nil {
		s.log.Error("provision a virtual machine", "group", job.group, "vm", job.name, "error", err)
		err = &armError{http.StatusInternalServerError, "OSProvisioningClientError", "The simulation could not provision the virtual machine: " + err.Error()}
	}
	updateErr := s.store.Update(func(sub *subscription) error {
		if v := lookup(sub.VMs, job.group, job.name); v != nil && v.ProvisioningState == "Creating" {
			v.ProvisioningState, v.PowerState = statusSucceeded, "running"
			if err != nil {
				v.ProvisioningState = statusFailed
			}
			if m.ID != "" {
				v.Machine = m.ID
			}
		}
		s.finishOperation(sub, job.opID, err)
		return nil
	})
	if updateErr != nil {
		s.log.Error("record a provisioned virtual machine", "vm", job.name, "error", updateErr)
	}
	s.firewalls.kick()
}

// provisionGuest creates the admin user and gives the machine its public IP
// addresses.
func (s *Simulation) provisionGuest(ctx context.Context, job provisioning, machine string) error {
	var user string
	var keys []string
	var addrs []string
	s.store.Read(func(sub *subscription) {
		v := lookup(sub.VMs, job.group, job.name)
		if v == nil {
			return
		}
		user = v.Properties.OSProfile.AdminUsername
		for _, k := range v.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys {
			keys = append(keys, k.KeyData)
		}
		addrs = s.publicAddressesOf(sub, v)
	})
	if user == "" {
		return fmt.Errorf("virtual machine %s is gone", job.name)
	}
	if _, err := s.machines.Exec(ctx, machine, strings.NewReader(strings.Join(keys, "\n")+"\n"), "sh", "-c", adminUserScript, "sh", user); err != nil {
		return fmt.Errorf("create admin user %s: %w", user, err)
	}
	if len(addrs) > 0 {
		if err := s.machines.SetAddresses(ctx, machine, addrs); err != nil {
			return fmt.Errorf("give the machine its public IP addresses: %w", err)
		}
		s.firewalls.addressesApplied(machine, addrs)
	}
	return nil
}

// publicAddressesOf returns the public IP addresses of the virtual machine's
// network interfaces.
func (s *Simulation) publicAddressesOf(sub *subscription, v *vm) []string {
	var out []string
	for _, ref := range v.Properties.NetworkProfile.NetworkInterfaces {
		n := s.nicByID(sub, ref.ID)
		if n == nil {
			continue
		}
		for _, c := range n.IPConfigurations {
			if ip := s.publicIPByID(sub, idOf(c.Properties.PublicIPAddress)); ip != nil {
				out = append(out, ip.Address)
			}
		}
	}
	return out
}

func (s *Simulation) getVM(r *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if v := lookup(sub.VMs, p["group"], p["name"]); v != nil {
			j := s.renderVM(v, r.URL.Query().Get("$expand"))
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeVM, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) getVMInstanceView(_ *http.Request, p params) (reply, error) {
	var out *instanceView
	s.store.Read(func(sub *subscription) {
		if v := lookup(sub.VMs, p["group"], p["name"]); v != nil {
			view := s.instanceView(v)
			out = &view
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeVM, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listVMs(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, v := range sub.VMs {
			if p["group"] == "" || strings.EqualFold(v.Group, p["group"]) {
				out = append(out, s.renderVM(v, r.URL.Query().Get("$expand")))
			}
		}
	})
	return listReply(r, out)
}

func (s *Simulation) patchVM(r *http.Request, p params) (reply, error) {
	var req map[string]json.RawMessage
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	for k := range req {
		if k != "tags" {
			return reply{}, errUnsupported("Changing %s of a virtual machine", k)
		}
	}
	var tags map[string]string
	if err := json.Unmarshal(req["tags"], &tags); err != nil && len(req["tags"]) > 0 {
		return reply{}, errBadRequest("InvalidRequestContent", "tags must be an object of strings")
	}
	var out resourceJSON
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VMs, p["group"], p["name"])
		if v == nil {
			return errNotFound(typeVM, p["name"], p["group"])
		}
		if req["tags"] != nil {
			v.Tags = cloneTags(tags)
		}
		op = s.doneOperation(sub, "Microsoft.Compute", v.Location)
		out = s.renderVM(v, "")
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK, body: out, headers: s.asyncHeaders(r, op)}, nil
}

type destruction struct {
	group, name, machine, opID string
}

func (s *Simulation) deleteVM(r *http.Request, p params) (reply, error) {
	var op *operation
	var job destruction
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VMs, p["group"], p["name"])
		if v == nil {
			return nil
		}
		if v.ProvisioningState == "Creating" {
			return errConflict("OperationNotAllowed", "Operation 'delete' is not allowed on virtual machine %s while it is being created.", v.Name)
		}
		op = s.startOperation(sub, "Microsoft.Compute", v.Location)
		v.ProvisioningState = "Deleting"
		job = destruction{group: v.Group, name: v.Name, machine: v.Machine, opID: op.ID}
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	s.background(func(ctx context.Context) { s.destroyVM(ctx, job) })
	return s.accepted(r, op), nil
}

// destroyVM deletes the machine first, so the virtual machine is gone only
// when its machine is, then the OS disk and network interface that the
// virtual machine says to delete with it.
func (s *Simulation) destroyVM(ctx context.Context, job destruction) {
	if job.machine != "" {
		if err := s.machines.Delete(ctx, job.machine); err != nil {
			s.log.Error("delete a virtual machine's machine", "vm", job.name, "error", err)
			_ = s.store.Update(func(sub *subscription) error {
				if v := lookup(sub.VMs, job.group, job.name); v != nil {
					v.ProvisioningState = statusFailed
				}
				s.finishOperation(sub, job.opID, err)
				return nil
			})
			return
		}
	}
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VMs, job.group, job.name)
		if v == nil {
			s.finishOperation(sub, job.opID, nil)
			return nil
		}
		sub.VMs = without(sub.VMs, v.Group, v.Name)
		d := v.Properties.StorageProfile.OSDisk
		if strings.EqualFold(d.DeleteOption, "Delete") {
			sub.Disks = without(sub.Disks, v.Group, d.Name)
		} else if dk := lookup(sub.Disks, v.Group, d.Name); dk != nil {
			dk.ManagedBy = ""
		}
		for _, ref := range v.Properties.NetworkProfile.NetworkInterfaces {
			if n := s.nicByID(sub, ref.ID); n != nil && strings.EqualFold(ref.deleteOption(), "Delete") {
				sub.NICs = without(sub.NICs, n.Group, n.Name)
			}
		}
		s.finishOperation(sub, job.opID, nil)
		return nil
	})
	if err != nil {
		s.log.Error("remove a deleted virtual machine", "vm", job.name, "error", err)
	}
	s.firewalls.forget(job.machine)
	s.firewalls.kick()
}

// vmAction runs a power action on a virtual machine as a long-running
// operation.
func (s *Simulation) vmAction(r *http.Request, p params) (reply, error) {
	action := strings.ToLower(p["action"])
	q := r.URL.Query()
	var run func(ctx context.Context, machine string) (string, error)
	switch action {
	case "poweroff":
		graceful := q.Get("skipShutdown") != "true"
		run = func(ctx context.Context, machine string) (string, error) {
			return "stopped", s.machines.Stop(ctx, machine, graceful)
		}
	case "deallocate":
		if q.Get("hibernate") == "true" {
			return reply{}, errUnsupported("Hibernation")
		}
		run = func(ctx context.Context, machine string) (string, error) {
			return "deallocated", s.machines.Stop(ctx, machine, true)
		}
	case "start":
		run = func(ctx context.Context, machine string) (string, error) {
			_, err := s.machines.Start(ctx, machine)
			return "running", err
		}
	case "restart":
		run = func(ctx context.Context, machine string) (string, error) {
			_, err := s.machines.Restart(ctx, machine, true)
			return "running", err
		}
	case "generalize":
		return s.generalizeVM(p)
	default:
		return reply{}, errUnsupported("The virtual machine action %s", p["action"])
	}
	var op *operation
	var machine, before string
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VMs, p["group"], p["name"])
		if v == nil {
			return errNotFound(typeVM, p["name"], p["group"])
		}
		if v.ProvisioningState != statusSucceeded || v.Machine == "" {
			return errConflict("OperationNotAllowed", "Operation '%s' is not allowed on VM '%s' since the VM is %s.", action, v.Name, strings.ToLower(v.ProvisioningState))
		}
		if v.Generalized && (action == "start" || action == "restart") {
			return errConflict("OperationNotAllowed", "Operation '%s' is not allowed on VM '%s' since the VM is generalized.", action, v.Name)
		}
		op = s.startOperation(sub, "Microsoft.Compute", v.Location)
		machine, before = v.Machine, v.PowerState
		v.PowerState = map[string]string{"poweroff": "stopping", "deallocate": "deallocating", "start": "starting", "restart": "starting"}[action]
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	group, name := p["group"], p["name"]
	s.background(func(ctx context.Context) {
		state, err := run(ctx, machine)
		if err != nil {
			state = before
		}
		_ = s.store.Update(func(sub *subscription) error {
			if v := lookup(sub.VMs, group, name); v != nil {
				v.PowerState = state
			}
			s.finishOperation(sub, op.ID, err)
			return nil
		})
		s.firewalls.kick()
	})
	return s.accepted(r, op), nil
}

// generalizeVM marks a stopped virtual machine as generalized, so an image
// can be captured from it. It never starts again.
func (s *Simulation) generalizeVM(p params) (reply, error) {
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VMs, p["group"], p["name"])
		if v == nil {
			return errNotFound(typeVM, p["name"], p["group"])
		}
		if v.PowerState != "stopped" && v.PowerState != "deallocated" {
			return errConflict("OperationNotAllowed", "Operation 'generalize' is not allowed on VM '%s' since the VM is running. Please power off explicitly in case you shut down the VM from inside the guest operating system.", v.Name)
		}
		v.Generalized = true
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK}, nil
}

func (s *Simulation) vmByID(sub *subscription, id string) *vm {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typeVM || r.child != "" {
		return nil
	}
	return lookup(sub.VMs, r.group, r.name)
}
