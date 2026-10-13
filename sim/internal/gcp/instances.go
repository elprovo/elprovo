package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	metadataAddress  = "169.254.169.254"
	maxMetadataValue = 256 << 10
	maxMetadataTotal = 512 << 10
)

var (
	metadataKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	tagPattern         = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	labelKeyPattern    = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValuePattern  = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

// instance is a VM instance. Its machine runs while the instance is RUNNING.
type instance struct {
	ID                      uint64            `json:"id"`
	Name                    string            `json:"name"`
	Zone                    string            `json:"zone"`
	Description             string            `json:"description"`
	MachineType             string            `json:"machine_type"`
	Status                  string            `json:"status"`
	Network                 string            `json:"network"`
	Subnetwork              string            `json:"subnetwork"`
	AccessConfig            *accessConfig     `json:"access_config,omitempty"`
	StaticIP                string            `json:"static_ip,omitempty"`
	Metadata                []metadataItem    `json:"metadata"`
	Tags                    []string          `json:"tags"`
	Labels                  map[string]string `json:"labels"`
	ServiceAccounts         []serviceAccount  `json:"service_accounts"`
	Scheduling              map[string]any    `json:"scheduling"`
	ShieldedInstanceConfig  map[string]any    `json:"shielded_instance_config,omitempty"`
	AdvancedMachineFeatures map[string]any    `json:"advanced_machine_features,omitempty"`
	CanIPForward            bool              `json:"can_ip_forward"`
	DeletionProtection      bool              `json:"deletion_protection"`
	BootDisk                string            `json:"boot_disk"`
	DeviceName              string            `json:"device_name"`
	AutoDelete              bool              `json:"auto_delete"`
	CreatedAt               time.Time         `json:"created_at"`
	StartedAt               time.Time         `json:"started_at"`
	Machine                 string            `json:"machine"`
	PrivateIP               string            `json:"private_ip"`
	PublicIP                string            `json:"public_ip"`
}

type accessConfig struct {
	Name        string `json:"name"`
	NetworkTier string `json:"networkTier"`
}

type serviceAccount struct {
	Email  string   `json:"email"`
	Scopes []string `json:"scopes"`
}

// natIP is the instance's external address: its static address, or the
// machine's own public address.
func (i *instance) natIP() string {
	if i.AccessConfig == nil {
		return ""
	}
	if i.StaticIP != "" {
		return i.StaticIP
	}
	return i.PublicIP
}

// extraAddresses are the addresses the machine holds besides its own.
func (i *instance) extraAddresses() []string {
	if i.StaticIP == "" {
		return []string{}
	}
	return []string{i.StaticIP}
}

func (i *instance) metadataValue(key string) (string, bool) {
	for _, item := range i.Metadata {
		if item.Key == key {
			return item.Value, true
		}
	}
	return "", false
}

type instanceJSON struct {
	Kind                    string             `json:"kind"`
	ID                      string             `json:"id"`
	CreationTimestamp       string             `json:"creationTimestamp"`
	Name                    string             `json:"name"`
	Description             string             `json:"description,omitempty"`
	Tags                    tagsJSON           `json:"tags"`
	MachineType             string             `json:"machineType"`
	Status                  string             `json:"status"`
	Zone                    string             `json:"zone"`
	CanIPForward            bool               `json:"canIpForward"`
	NetworkInterfaces       []nicJSON          `json:"networkInterfaces"`
	Disks                   []attachedDiskJSON `json:"disks"`
	Metadata                metadataJSON       `json:"metadata"`
	ServiceAccounts         []serviceAccount   `json:"serviceAccounts,omitempty"`
	SelfLink                string             `json:"selfLink"`
	Scheduling              map[string]any     `json:"scheduling"`
	CPUPlatform             string             `json:"cpuPlatform"`
	Labels                  map[string]string  `json:"labels,omitempty"`
	LabelFingerprint        string             `json:"labelFingerprint"`
	StartRestricted         bool               `json:"startRestricted"`
	DeletionProtection      bool               `json:"deletionProtection"`
	ShieldedInstanceConfig  map[string]any     `json:"shieldedInstanceConfig,omitempty"`
	AdvancedMachineFeatures map[string]any     `json:"advancedMachineFeatures,omitempty"`
	Fingerprint             string             `json:"fingerprint"`
	LastStartTimestamp      string             `json:"lastStartTimestamp,omitempty"`
}

type tagsJSON struct {
	Items       []string `json:"items,omitempty"`
	Fingerprint string   `json:"fingerprint"`
}

type nicJSON struct {
	Kind          string             `json:"kind"`
	Network       string             `json:"network"`
	Subnetwork    string             `json:"subnetwork"`
	NetworkIP     string             `json:"networkIP,omitempty"`
	Name          string             `json:"name"`
	AccessConfigs []accessConfigJSON `json:"accessConfigs,omitempty"`
	Fingerprint   string             `json:"fingerprint"`
	StackType     string             `json:"stackType"`
}

type accessConfigJSON struct {
	Kind        string `json:"kind"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	NatIP       string `json:"natIP,omitempty"`
	NetworkTier string `json:"networkTier"`
}

type attachedDiskJSON struct {
	Kind            string           `json:"kind"`
	Type            string           `json:"type"`
	Mode            string           `json:"mode"`
	Source          string           `json:"source"`
	DeviceName      string           `json:"deviceName"`
	Index           int              `json:"index"`
	Boot            bool             `json:"boot"`
	AutoDelete      bool             `json:"autoDelete"`
	Licenses        []string         `json:"licenses,omitempty"`
	Interface       string           `json:"interface"`
	GuestOsFeatures []map[string]any `json:"guestOsFeatures"`
	DiskSizeGb      string           `json:"diskSizeGb"`
	Architecture    string           `json:"architecture,omitempty"`
}

func (s *Simulation) renderInstance(w *world, inst *instance) instanceJSON {
	region := regionOf(inst.Zone)
	nic := nicJSON{
		Kind: "compute#networkInterface", Network: s.link("global", "networks", inst.Network),
		Subnetwork: s.link("regions", region, "subnetworks", inst.Subnetwork), NetworkIP: inst.PrivateIP, Name: "nic0",
		Fingerprint: fingerprint(fmt.Sprint(inst.ID, inst.PrivateIP)), StackType: "IPV4_ONLY",
	}
	if inst.AccessConfig != nil {
		nic.AccessConfigs = []accessConfigJSON{{
			Kind: "compute#accessConfig", Type: "ONE_TO_ONE_NAT", Name: inst.AccessConfig.Name, NatIP: inst.natIP(),
			NetworkTier: inst.AccessConfig.NetworkTier,
		}}
	}
	out := instanceJSON{
		Kind: "compute#instance", ID: idString(inst.ID), CreationTimestamp: timestamp(inst.CreatedAt), Name: inst.Name,
		Description: inst.Description, Tags: tagsJSON{Items: inst.Tags, Fingerprint: fingerprint(jsonKey(inst.Tags))},
		MachineType: s.link("zones", inst.Zone, "machineTypes", inst.MachineType), Status: inst.Status,
		Zone: s.link("zones", inst.Zone), CanIPForward: inst.CanIPForward, NetworkInterfaces: []nicJSON{nic},
		Disks: []attachedDiskJSON{}, Metadata: renderMetadata(inst.Metadata), ServiceAccounts: inst.ServiceAccounts,
		SelfLink: s.link("zones", inst.Zone, "instances", inst.Name), Scheduling: inst.Scheduling,
		CPUPlatform: "Intel Cascade Lake", Labels: inst.Labels, LabelFingerprint: fingerprint(jsonKey(inst.Labels)),
		DeletionProtection: inst.DeletionProtection, ShieldedInstanceConfig: inst.ShieldedInstanceConfig,
		AdvancedMachineFeatures: inst.AdvancedMachineFeatures, Fingerprint: fingerprint(fmt.Sprint(inst.ID, inst.Status)),
	}
	if !inst.StartedAt.IsZero() {
		out.LastStartTimestamp = timestamp(inst.StartedAt)
	}
	if d := w.disk(inst.Zone, inst.BootDisk); d != nil {
		out.Disks = append(out.Disks, attachedDiskJSON{
			Kind: "compute#attachedDisk", Type: "PERSISTENT", Mode: "READ_WRITE", Source: s.link("zones", d.Zone, "disks", d.Name),
			DeviceName: inst.DeviceName, Boot: true, AutoDelete: inst.AutoDelete, Interface: "SCSI",
			GuestOsFeatures: guestOSFeatures(), DiskSizeGb: strconv.Itoa(d.SizeGB), Architecture: d.Architecture,
		})
	}
	return out
}

func renderMetadata(items []metadataItem) metadataJSON {
	return metadataJSON{Kind: "compute#metadata", Fingerprint: fingerprint(jsonKey(items)), Items: items}
}

func guestOSFeatures() []map[string]any {
	return []map[string]any{{"type": "VIRTIO_SCSI_MULTIQUEUE"}, {"type": "UEFI_COMPATIBLE"}, {"type": "GVNIC"}}
}

// flexInt is an int64 that JSON clients send as a number or, as Google's
// discovery format encodes int64, as a string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), `"`)
	if text == "" || text == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not an integer", data)
	}
	*f = flexInt(n)
	return nil
}

type instanceRequest struct {
	Name              string                     `json:"name"`
	Description       string                     `json:"description"`
	MachineType       string                     `json:"machineType"`
	Disks             []attachedDiskRequest      `json:"disks"`
	NetworkInterfaces []*networkInterfaceRequest `json:"networkInterfaces"`
	Metadata          *struct {
		Items []struct {
			Key   string  `json:"key"`
			Value *string `json:"value"`
		} `json:"items"`
	} `json:"metadata"`
	Tags *struct {
		Items []string `json:"items"`
	} `json:"tags"`
	Labels                  map[string]string `json:"labels"`
	ServiceAccounts         []serviceAccount  `json:"serviceAccounts"`
	Scheduling              map[string]any    `json:"scheduling"`
	ShieldedInstanceConfig  map[string]any    `json:"shieldedInstanceConfig"`
	AdvancedMachineFeatures map[string]any    `json:"advancedMachineFeatures"`
	CanIPForward            bool              `json:"canIpForward"`
	DeletionProtection      bool              `json:"deletionProtection"`
}

type attachedDiskRequest struct {
	Type              string          `json:"type"`
	Mode              string          `json:"mode"`
	Boot              bool            `json:"boot"`
	AutoDelete        bool            `json:"autoDelete"`
	DeviceName        string          `json:"deviceName"`
	Source            string          `json:"source"`
	Interface         string          `json:"interface"`
	DiskEncryptionKey json.RawMessage `json:"diskEncryptionKey"`
	InitializeParams  *struct {
		DiskName    string            `json:"diskName"`
		SourceImage string            `json:"sourceImage"`
		DiskSizeGb  flexInt           `json:"diskSizeGb"`
		DiskType    string            `json:"diskType"`
		Labels      map[string]string `json:"labels"`
	} `json:"initializeParams"`
}

type networkInterfaceRequest struct {
	Network       string `json:"network"`
	Subnetwork    string `json:"subnetwork"`
	NetworkIP     string `json:"networkIP"`
	StackType     string `json:"stackType"`
	AccessConfigs []*struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		NatIP       string `json:"natIP"`
		NetworkTier string `json:"networkTier"`
	} `json:"accessConfigs"`
	AliasIPRanges     json.RawMessage `json:"aliasIpRanges"`
	IPv6AccessConfigs json.RawMessage `json:"ipv6AccessConfigs"`
}

// instanceFields are the fields of an instance insert that the simulation
// reads. ignoredInstanceFields are accepted only while they hold no value.
// Any other field is an error, so a request the simulation would not honor
// fails loudly.
var (
	instanceFields = []string{
		"kind", "name", "description", "machineType", "disks", "networkInterfaces", "metadata", "tags", "labels",
		"serviceAccounts", "scheduling", "shieldedInstanceConfig", "advancedMachineFeatures", "canIpForward",
		"deletionProtection",
	}
	ignoredInstanceFields = []string{
		"guestAccelerators", "minCpuPlatform", "resourcePolicies", "reservationAffinity", "confidentialInstanceConfig",
		"params", "displayDevice", "keyRevocationActionType", "networkPerformanceConfig", "instanceEncryptionKey",
		"workloadIdentityConfig", "hostname",
	}
)

func decodeInstanceRequest(r *http.Request) (instanceRequest, error) {
	var req instanceRequest
	var raw map[string]json.RawMessage
	if err := decodeBody(r, &raw); err != nil {
		return req, err
	}
	for key, value := range raw {
		switch {
		case slices.Contains(instanceFields, key):
		case slices.Contains(ignoredInstanceFields, key):
			if !emptyJSON(value) {
				return req, errUnsupported("The instance field %s", key)
			}
		default:
			return req, errUnsupported("The instance field %s", key)
		}
	}
	data, _ := json.Marshal(raw)
	if err := json.Unmarshal(data, &req); err != nil {
		return req, &apiError{http.StatusBadRequest, "parseError", "Invalid JSON payload received. " + err.Error()}
	}
	return req, nil
}

// emptyJSON reports whether a JSON value holds nothing: null, false, zero,
// an empty string, or an array or object of empty values.
func emptyJSON(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return emptyValue(v)
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case float64:
		return t == 0
	case string:
		return t == ""
	case []any:
		return !slices.ContainsFunc(t, func(e any) bool { return !emptyValue(e) })
	case map[string]any:
		for _, e := range t {
			if !emptyValue(e) {
				return false
			}
		}
		return true
	}
	return false
}

func (s *Simulation) checkInstanceRequest(zone string, req instanceRequest) (*instance, error) {
	if err := checkName("instance", req.Name); err != nil {
		return nil, err
	}
	mt := parseRef(req.MachineType)
	if !s.belongs(mt, "machineTypes", zone) {
		return nil, errInvalid("Invalid value for field 'resource.machineType': '%s'. Machine type with name '%s' does not exist in zone '%s'.", req.MachineType, mt.name, zone)
	}
	if _, ok := machineTypeNamed(mt.name); !ok {
		return nil, errInvalid("Invalid value for field 'resource.machineType': '%s'. Machine type with name '%s' does not exist in zone '%s'.", req.MachineType, mt.name, zone)
	}
	inst := &instance{
		Name: req.Name, Zone: zone, Description: req.Description, MachineType: mt.name, Status: "PROVISIONING",
		Labels: req.Labels, ServiceAccounts: req.ServiceAccounts, Scheduling: schedulingWithDefaults(req.Scheduling),
		ShieldedInstanceConfig: req.ShieldedInstanceConfig, AdvancedMachineFeatures: req.AdvancedMachineFeatures,
		CanIPForward: req.CanIPForward, DeletionProtection: req.DeletionProtection,
	}
	if err := checkLabels(req.Labels); err != nil {
		return nil, err
	}
	if req.Tags != nil {
		for _, t := range req.Tags.Items {
			if !tagPattern.MatchString(t) {
				return nil, errInvalid("Invalid value for field 'resource.tags.items': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)'", t)
			}
			if !slices.Contains(inst.Tags, t) {
				inst.Tags = append(inst.Tags, t)
			}
		}
	}
	if err := s.checkMetadata(inst, req); err != nil {
		return nil, err
	}
	if len(req.NetworkInterfaces) != 1 || req.NetworkInterfaces[0] == nil {
		return nil, errUnsupported("An instance without exactly one network interface")
	}
	return inst, s.checkNetworkInterface(inst, req.NetworkInterfaces[0])
}

func schedulingWithDefaults(in map[string]any) map[string]any {
	out := map[string]any{"onHostMaintenance": "MIGRATE", "automaticRestart": true, "preemptible": false, "provisioningModel": "STANDARD"}
	for k, v := range in {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

func checkLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return errInvalid("Invalid value for field 'resource.labels': at most 64 labels are allowed.")
	}
	for k, v := range labels {
		if !labelKeyPattern.MatchString(k) || !labelValuePattern.MatchString(v) {
			return errInvalid("Invalid value for field 'resource.labels': '%s=%s'. Label keys and values must be lowercase letters, digits, '_' and '-'.", k, v)
		}
	}
	return nil
}

var unsupportedMetadata = []string{"startup-script", "startup-script-url", "shutdown-script", "shutdown-script-url", "enable-oslogin", "windows-startup-script-ps1"}

func (s *Simulation) checkMetadata(inst *instance, req instanceRequest) error {
	if req.Metadata == nil {
		return nil
	}
	total := 0
	for _, item := range req.Metadata.Items {
		value := ""
		if item.Value != nil {
			value = *item.Value
		}
		switch {
		case !metadataKeyPattern.MatchString(item.Key):
			return errInvalid("Invalid value for field 'resource.metadata': key '%s' must match [a-zA-Z0-9-_]{1,128}.", item.Key)
		case len(value) > maxMetadataValue:
			return errInvalid("Invalid value for field 'resource.metadata': the value of '%s' exceeds 256 KB.", item.Key)
		case slices.ContainsFunc(inst.Metadata, func(m metadataItem) bool { return m.Key == item.Key }):
			return errInvalid("Invalid value for field 'resource.metadata': duplicate key '%s'.", item.Key)
		case slices.Contains(unsupportedMetadata, item.Key) && value != "" && !strings.EqualFold(value, "false"):
			return errUnsupported("The metadata key %s (the simulated machine has no guest agent)", item.Key)
		}
		total += len(item.Key) + len(value)
		inst.Metadata = append(inst.Metadata, metadataItem{Key: item.Key, Value: value})
	}
	if total > maxMetadataTotal {
		return errInvalid("Invalid value for field 'resource.metadata': the metadata exceeds 512 KB.")
	}
	return nil
}

func (s *Simulation) checkNetworkInterface(inst *instance, nic *networkInterfaceRequest) error {
	switch {
	case nic.NetworkIP != "":
		return errUnsupported("Choosing the internal address (networkIP)")
	case nic.StackType != "" && nic.StackType != "IPV4_ONLY":
		return errUnsupported("stackType %s", nic.StackType)
	case len(nic.AliasIPRanges) > 0 && !emptyJSON(nic.AliasIPRanges):
		return errUnsupported("aliasIpRanges")
	case len(nic.IPv6AccessConfigs) > 0 && !emptyJSON(nic.IPv6AccessConfigs):
		return errUnsupported("ipv6AccessConfigs")
	}
	region := regionOf(inst.Zone)
	if nic.Subnetwork != "" {
		sub := parseRef(nic.Subnetwork)
		if !s.belongs(sub, "subnetworks", region) {
			return errInvalid("Invalid value for field 'resource.networkInterfaces[0].subnetwork': '%s'.", nic.Subnetwork)
		}
		inst.Subnetwork = sub.name
	}
	if nic.Network != "" {
		n := parseRef(nic.Network)
		if !s.belongs(n, "networks", "") {
			return errInvalid("Invalid value for field 'resource.networkInterfaces[0].network': '%s'.", nic.Network)
		}
		inst.Network = n.name
	}
	if nic.Network == "" && nic.Subnetwork == "" {
		inst.Network = "default"
	}
	var configs []accessConfig
	for _, ac := range nic.AccessConfigs {
		if ac == nil {
			continue
		}
		if ac.Type != "" && ac.Type != "ONE_TO_ONE_NAT" {
			return errUnsupported("Access config type %s", ac.Type)
		}
		name, tier := ac.Name, ac.NetworkTier
		if name == "" {
			name = "external-nat"
		}
		if tier == "" {
			tier = "PREMIUM"
		}
		configs = append(configs, accessConfig{Name: name, NetworkTier: tier})
		inst.StaticIP = ac.NatIP
	}
	if len(configs) > 1 {
		return errInvalid("Invalid value for field 'resource.networkInterfaces[0].accessConfigs': at most one access config is allowed.")
	}
	if len(configs) == 1 {
		inst.AccessConfig = &configs[0]
	}
	return nil
}

// resolveNetwork fills in the instance's network and subnetwork from the
// world and checks its static address.
func (s *Simulation) resolveNetwork(w *world, inst *instance) error {
	region := regionOf(inst.Zone)
	if inst.Subnetwork != "" {
		sub := w.subnetwork(region, inst.Subnetwork)
		if sub == nil {
			return errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/subnetworks/" + inst.Subnetwork)
		}
		if inst.Network != "" && inst.Network != sub.Network {
			return errInvalid("Invalid value for field 'resource.networkInterfaces[0].subnetwork': the subnetwork belongs to network '%s'.", sub.Network)
		}
		inst.Network = sub.Network
	}
	n := w.network(inst.Network)
	if n == nil {
		return errNotFound("projects/" + s.cfg.project + "/global/networks/" + inst.Network)
	}
	if inst.Subnetwork == "" {
		if !n.AutoCreateSubnetworks {
			return errInvalid("Invalid value for field 'resource.networkInterfaces[0]': Subnetwork should be specified for custom subnetmode network.")
		}
		inst.Subnetwork = n.Name
	}
	if inst.StaticIP == "" {
		return nil
	}
	a := find(w.Addresses, func(a *address) bool { return a.Address == inst.StaticIP })
	switch {
	case a == nil:
		return errInvalid("Invalid value for field 'resource.networkInterfaces[0].accessConfigs[0].natIP': '%s'. The specified external IP address is not reserved.", inst.StaticIP)
	case a.Region != region:
		return errInvalid("Invalid value for field 'resource.networkInterfaces[0].accessConfigs[0].natIP': '%s'. The address is in region %s.", inst.StaticIP, a.Region)
	case w.addressUser(a) != nil:
		return errInUse("The address '%s' is already being used by another resource.", inst.StaticIP)
	}
	return nil
}

func (s *Simulation) insertInstance(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	req, err := decodeInstanceRequest(r)
	if err != nil {
		return 0, nil, err
	}
	inst, err := s.checkInstanceRequest(zone, req)
	if err != nil {
		return 0, nil, err
	}
	diskReq, err := bootDiskRequest(req.Disks)
	if err != nil {
		return 0, nil, err
	}
	var out operationJSON
	var job provisioning
	err = s.store.Update(func(w *world) error {
		if w.instance(zone, inst.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/zones/" + zone + "/instances/" + inst.Name)
		}
		if len(w.Instances) >= instanceLimit {
			return &apiError{http.StatusForbidden, "quotaExceeded", fmt.Sprintf("Quota 'INSTANCES' exceeded. Limit: %d.0 in region %s.", instanceLimit, regionOf(zone))}
		}
		if err := s.resolveNetwork(w, inst); err != nil {
			return err
		}
		inst.ID, inst.CreatedAt = w.nextID(), s.now()
		d, imageRef, err := s.newBootDisk(w, inst, diskReq)
		if err != nil {
			return err
		}
		d.ID, d.CreatedAt = w.nextID(), s.now()
		w.Instances = append(w.Instances, inst)
		w.Disks = append(w.Disks, d)
		op := s.newOperation(w, "zone", zone, "insert", s.link("zones", zone, "instances", inst.Name), inst.ID)
		out = s.renderOperation(w, op)
		job = provisioning{instanceID: inst.ID, operationID: op.ID, spec: s.machineSpec(w, inst, imageRef)}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.provision(ctx, job) })
	return http.StatusOK, out, nil
}

func bootDiskRequest(disks []attachedDiskRequest) (attachedDiskRequest, error) {
	if len(disks) != 1 || !disks[0].Boot {
		return attachedDiskRequest{}, errUnsupported("An instance without exactly one disk, its boot disk,")
	}
	d := disks[0]
	switch {
	case d.Type != "" && d.Type != "PERSISTENT":
		return d, errUnsupported("Disk type %s", d.Type)
	case d.Mode != "" && d.Mode != "READ_WRITE":
		return d, errUnsupported("Disk mode %s", d.Mode)
	case d.Source != "":
		return d, errUnsupported("Attaching an existing disk (source)")
	case len(d.DiskEncryptionKey) > 0 && !emptyJSON(d.DiskEncryptionKey):
		return d, errUnsupported("diskEncryptionKey")
	case d.InitializeParams == nil || d.InitializeParams.SourceImage == "":
		return d, errInvalid("Invalid value for field 'resource.disks[0].initializeParams.sourceImage': ''. A boot disk needs a source image.")
	}
	return d, nil
}

type provisioning struct {
	instanceID  uint64
	operationID uint64
	spec        machines.Spec
}

func (s *Simulation) machineSpec(w *world, inst *instance, imageRef string) machines.Spec {
	id := idString(inst.ID)
	userData, _ := inst.metadataValue("user-data")
	if enc, _ := inst.metadataValue("user-data-encoding"); enc == "base64" {
		if decoded, err := base64.StdEncoding.DecodeString(userData); err == nil {
			userData = string(decoded)
		}
	}
	sshKeys, _ := inst.metadataValue("ssh-keys")
	project, number := s.cfg.project, w.ProjectNumber
	snapshot := *inst
	return machines.Spec{
		Name:     fmt.Sprintf("elprovo-sim-gcp-%s-%s", w.Account[:8], id),
		Hostname: inst.Name,
		Cloud:    cloudLabel, Account: w.Account, Resource: id,
		Image: imageRef,
		Seed: machines.Seed{
			InstanceID: id, PublicKeys: publicKeys(sshKeys), UserData: userData,
			Metadata: func(privateIPv4, publicIPv4 string) map[string]string {
				return instanceMetadata(&snapshot, project, number, privateIPv4, publicIPv4)
			},
		},
		Firewall: policyFor(w, inst),
		Hosts:    map[string]string{"metadata.google.internal": metadataAddress},
	}
}

// publicKeys returns the keys of an ssh-keys metadata value, whose lines read
// "user:key". The simulated machine gives every key to root.
func publicKeys(sshKeys string) []string {
	var keys []string
	for _, line := range strings.Split(sshKeys, "\n") {
		_, key, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.TrimSpace(key) != "" {
			keys = append(keys, strings.TrimSpace(key))
		}
	}
	return keys
}

func (s *Simulation) provision(ctx context.Context, job provisioning) {
	m, err := s.machines.Create(ctx, job.spec)
	if err != nil {
		s.log.Error("create instance machine", "instance", job.instanceID, "error", err)
		var released []string
		s.finishOperation(job.operationID, &opError{"INTERNAL_ERROR", "The simulation could not create the instance's machine.", http.StatusInternalServerError}, func(w *world) {
			w.Instances = slices.DeleteFunc(w.Instances, func(i *instance) bool { return i.ID == job.instanceID })
			released = removeDisksOf(w, job.instanceID)
		})
		s.releaseImageRefs(ctx, released)
		return
	}
	s.firewalls.applied(job.instanceID, jsonKey(job.spec.Firewall))
	var static []string
	s.store.Read(func(w *world) {
		if inst := w.instanceByID(job.instanceID); inst != nil {
			static = inst.extraAddresses()
		}
	})
	if len(static) > 0 {
		if err := s.machines.SetAddresses(ctx, m.ID, static); err != nil {
			s.log.Warn("give the instance machine its static address; retrying", "instance", job.instanceID, "error", err)
		} else {
			s.addresses.applied(job.instanceID, jsonKey(static))
		}
	} else {
		s.addresses.applied(job.instanceID, jsonKey(static))
	}
	s.finishOperation(job.operationID, nil, func(w *world) {
		if inst := w.instanceByID(job.instanceID); inst != nil {
			inst.Status, inst.Machine, inst.PrivateIP, inst.PublicIP, inst.StartedAt = "RUNNING", m.ID, m.PrivateIP, m.PublicIP, s.now()
		}
	})
	s.firewalls.kick()
	s.addresses.kick()
}

func (s *Simulation) getInstance(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	var out *instanceJSON
	s.store.Read(func(w *world) {
		if inst := w.instance(zone, r.PathValue("name")); inst != nil {
			rendered := s.renderInstance(w, inst)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/instances/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) listInstances(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	match, err := parseFilter(r.URL.Query().Get("filter"))
	if err != nil {
		return 0, nil, err
	}
	var out []instanceJSON
	s.store.Read(func(w *world) {
		for _, inst := range w.Instances {
			if inst.Zone == zone && match(inst) {
				out = append(out, s.renderInstance(w, inst))
			}
		}
	})
	return page(r, "compute#instanceList", s.link("zones", zone, "instances"), out)
}

// parseFilter supports the Compute Engine list filters that the simulation
// needs: comparisons such as labels.KEY = "VALUE", name = NAME or
// status != RUNNING, joined with AND or spaces.
func parseFilter(filter string) (func(*instance) bool, error) {
	if strings.TrimSpace(filter) == "" {
		return func(*instance) bool { return true }, nil
	}
	if strings.Contains(filter, " OR ") || strings.Contains(filter, "NOT ") {
		return nil, errUnsupported("The filter %q", filter)
	}
	spaced := strings.NewReplacer("(", " ", ")", " ", "!=", "\x00").Replace(filter)
	spaced = strings.NewReplacer("=", " = ", "\x00", " != ").Replace(spaced)
	tokens := slices.DeleteFunc(strings.Fields(spaced), func(t string) bool { return t == "AND" })
	if len(tokens)%3 != 0 {
		return nil, errUnsupported("The filter %q", filter)
	}
	var checks []func(*instance) bool
	for i := 0; i < len(tokens); i += 3 {
		field, op, value := tokens[i], tokens[i+1], strings.Trim(tokens[i+2], `"`)
		if op != "=" && op != "!=" && op != "eq" && op != "ne" {
			return nil, errUnsupported("The filter %q", filter)
		}
		get, err := filterField(field)
		if err != nil {
			return nil, err
		}
		want := op == "=" || op == "eq"
		checks = append(checks, func(i *instance) bool { return (get(i) == value) == want })
	}
	return func(i *instance) bool {
		return !slices.ContainsFunc(checks, func(check func(*instance) bool) bool { return !check(i) })
	}, nil
}

func filterField(field string) (func(*instance) string, error) {
	switch {
	case field == "name":
		return func(i *instance) string { return i.Name }, nil
	case field == "status":
		return func(i *instance) string { return i.Status }, nil
	case strings.HasPrefix(field, "labels."):
		key := strings.TrimPrefix(field, "labels.")
		return func(i *instance) string { return i.Labels[key] }, nil
	}
	return nil, errUnsupported("Filtering on %q", field)
}

func (s *Simulation) deleteInstance(r *http.Request) (int, any, error) {
	zone, err := s.zone(r)
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	var job destruction
	err = s.store.Update(func(w *world) error {
		inst := w.instance(zone, name)
		if inst == nil {
			return errNotFound("projects/" + s.cfg.project + "/zones/" + zone + "/instances/" + name)
		}
		if inst.DeletionProtection {
			return errInvalid("Invalid resource usage: 'Resource cannot be deleted if it's protected against deletion.'.")
		}
		if inst.Status != "RUNNING" && inst.Status != "TERMINATED" {
			return &apiError{http.StatusBadRequest, "resourceNotReady", fmt.Sprintf("The resource 'projects/%s/zones/%s/instances/%s' is not ready", s.cfg.project, zone, name)}
		}
		inst.Status = "STOPPING"
		op := s.newOperation(w, "zone", zone, "delete", s.link("zones", zone, "instances", name), inst.ID)
		out = s.renderOperation(w, op)
		job = destruction{instanceID: inst.ID, operationID: op.ID, machine: inst.Machine, keepDisk: !inst.AutoDelete}
		if d := w.disk(zone, inst.BootDisk); d != nil && job.keepDisk {
			job.diskRef = fmt.Sprintf("elprovo-sim-gcp-disk:%s-%d", w.Account[:8], d.ID)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	return http.StatusOK, out, nil
}

type destruction struct {
	instanceID  uint64
	operationID uint64
	machine     string
	// keepDisk says the boot disk outlives the instance. Its content is
	// committed to diskRef first.
	keepDisk bool
	diskRef  string
}

func (s *Simulation) destroy(ctx context.Context, job destruction) {
	if err := s.removeMachine(ctx, job); err != nil {
		s.log.Error("delete instance machine", "instance", job.instanceID, "error", err)
		s.finishOperation(job.operationID, &opError{"INTERNAL_ERROR", "The simulation could not delete the instance's machine.", http.StatusInternalServerError}, func(w *world) {
			if inst := w.instanceByID(job.instanceID); inst != nil {
				inst.Status = "RUNNING"
			}
		})
		return
	}
	var released []string
	s.finishOperation(job.operationID, nil, func(w *world) {
		w.Instances = slices.DeleteFunc(w.Instances, func(i *instance) bool { return i.ID == job.instanceID })
		for _, d := range w.Disks {
			if d.User == job.instanceID {
				released = append(released, d.ImageRef)
				if job.keepDisk {
					d.User, d.ImageRef = 0, job.diskRef
				}
			}
		}
		w.Disks = slices.DeleteFunc(w.Disks, func(d *disk) bool { return d.User == job.instanceID })
	})
	s.firewalls.forget(job.instanceID)
	s.addresses.forget(job.instanceID)
	s.firewalls.kick()
	s.releaseImageRefs(ctx, released)
}

func (s *Simulation) removeMachine(ctx context.Context, job destruction) error {
	if job.machine == "" {
		return nil
	}
	if !job.keepDisk || job.diskRef == "" {
		return s.machines.Delete(ctx, job.machine)
	}
	if _, err := s.machines.Commit(ctx, job.machine, job.diskRef); err != nil {
		return fmt.Errorf("save the boot disk: %w", err)
	}
	if err := s.machines.Delete(ctx, job.machine); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		return errors.Join(err, s.machines.RemoveImage(cleanupCtx, job.diskRef))
	}
	return nil
}
