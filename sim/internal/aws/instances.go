package aws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const (
	maxUserData = 16 << 10
	// terminatedRetention is how long DescribeInstances still lists a
	// terminated instance, about an hour on AWS.
	terminatedRetention = time.Hour
	rootDeviceName      = "/dev/sda1"
)

// stateCodes are the codes EC2 gives each instance state.
var stateCodes = map[string]int{"pending": 0, "running": 16, "shutting-down": 32, "terminated": 48, "stopping": 64, "stopped": 80}

type instance struct {
	ID                    string    `json:"id"`
	Region                string    `json:"region"`
	ReservationID         string    `json:"reservation_id"`
	LaunchIndex           int       `json:"launch_index"`
	ImageID               string    `json:"image_id"`
	InstanceType          string    `json:"instance_type"`
	KeyName               string    `json:"key_name,omitempty"`
	SubnetID              string    `json:"subnet_id"`
	VPCID                 string    `json:"vpc_id"`
	AZ                    string    `json:"az"`
	SecurityGroupIDs      []string  `json:"security_group_ids"`
	UserData              string    `json:"user_data,omitempty"`
	State                 string    `json:"state"`
	StateReason           string    `json:"state_reason,omitempty"`
	TransitionReason      string    `json:"transition_reason,omitempty"`
	LaunchTime            time.Time `json:"launch_time"`
	TerminatedAt          time.Time `json:"terminated_at"`
	Machine               string    `json:"machine,omitempty"`
	PrivateIP             string    `json:"private_ip,omitempty"`
	MachinePublicIP       string    `json:"machine_public_ip,omitempty"`
	PublicIPOnLaunch      bool      `json:"public_ip_on_launch"`
	ENIID                 string    `json:"eni_id"`
	ENIAttachmentID       string    `json:"eni_attachment_id"`
	MAC                   string    `json:"mac"`
	VolumeID              string    `json:"volume_id"`
	ClientToken           string    `json:"client_token,omitempty"`
	DisableAPITermination bool      `json:"disable_api_termination"`
	DisableAPIStop        bool      `json:"disable_api_stop"`
	ShutdownBehavior      string    `json:"shutdown_behavior"`
	EBSOptimized          bool      `json:"ebs_optimized"`
	Monitoring            bool      `json:"monitoring"`
	SourceDestCheck       bool      `json:"source_dest_check"`
	CPUCredits            string    `json:"cpu_credits,omitempty"`
	HTTPTokens            string    `json:"http_tokens"`
	HTTPHopLimit          int       `json:"http_hop_limit"`
	HTTPEndpoint          string    `json:"http_endpoint"`
	MetadataTags          string    `json:"metadata_tags"`
	// Busy is set while Docker work on the instance's machine runs.
	Busy bool `json:"busy"`
}

// publicIP returns the instance's public address: its Elastic IP, or the
// address it got at launch while it runs.
func (w *world) publicIP(i *instance) string {
	if a := find(w.Addresses, func(a *address) bool { return a.InstanceID == i.ID }); a != nil {
		return a.PublicIP
	}
	if i.PublicIPOnLaunch && (i.State == "running" || i.State == "stopping" || i.State == "shutting-down") {
		return i.MachinePublicIP
	}
	return ""
}

func (s *Simulation) ec2InstanceActions() map[string]ec2Action {
	return map[string]ec2Action{
		"RunInstances": {s.runInstances, []string{
			"ImageId", "InstanceType", "MinCount", "MaxCount", "KeyName", "SubnetId", "SecurityGroupId", "SecurityGroup",
			"UserData", "TagSpecification", "Placement", "MetadataOptions", "CreditSpecification", "DisableApiTermination",
			"DisableApiStop", "InstanceInitiatedShutdownBehavior", "EbsOptimized", "Monitoring", "ClientToken",
			"BlockDeviceMapping", "CapacityReservationSpecification", "HibernationOptions", "EnclaveOptions",
			"MaintenanceOptions", "PrivateDnsNameOptions", "IamInstanceProfile",
		}},
		"DescribeInstances":                    {s.describeInstances, []string{"InstanceId", "Filter", "MaxResults", "NextToken"}},
		"TerminateInstances":                   {s.terminateInstances, []string{"InstanceId"}},
		"StopInstances":                        {s.stopInstances, []string{"InstanceId", "Force", "Hibernate"}},
		"StartInstances":                       {s.startInstances, []string{"InstanceId"}},
		"DescribeInstanceAttribute":            {s.describeInstanceAttribute, []string{"InstanceId", "Attribute"}},
		"ModifyInstanceAttribute":              {s.modifyInstanceAttribute, []string{"InstanceId", "DisableApiTermination", "DisableApiStop", "InstanceInitiatedShutdownBehavior", "UserData", "InstanceType", "SourceDestCheck", "GroupId"}},
		"DescribeInstanceCreditSpecifications": {s.describeInstanceCreditSpecifications, []string{"InstanceId", "Filter", "MaxResults", "NextToken"}},
		"GetInstanceUefiData":                  {s.getInstanceUEFIData, []string{"InstanceId"}},
	}
}

// launch is what RunInstances decided before it creates instances.
type launch struct {
	image        *image
	imageRef     string
	instanceType instanceType
	keyName      string
	publicKey    string
	subnet       *subnet
	groups       []string
	userData     string
	tags         map[string][]tag
	count        int
	publicIP     bool
	rootSize     int
	rootType     string
	shutdown     string
	metadata     metadataOptions
	cpuCredits   string
	disableTerm  bool
	disableStop  bool
	ebsOptimized bool
	monitoring   bool
	clientToken  string
}

type metadataOptions struct {
	tokens   string
	hopLimit int
	endpoint string
	tags     string
}

func (s *Simulation) planLaunch(w *world, c *ec2Call) (launch, error) {
	p := c.params
	var l launch
	minCount, err := p.integer("MinCount", 0)
	if err != nil {
		return l, err
	}
	maxCount, err := p.integer("MaxCount", 0)
	if err != nil {
		return l, err
	}
	switch {
	case p.get("ImageId") == "":
		return l, errMissingParameter("ImageId")
	case minCount < 1 || maxCount < minCount:
		return l, errInvalidParameter("MinCount and MaxCount must be at least 1, and MaxCount at least MinCount")
	}
	running := 0
	for _, i := range w.Instances {
		if i.State != "terminated" {
			running++
		}
	}
	l.count = min(maxCount, instanceLimit-running)
	if l.count < minCount {
		return l, clientError("InstanceLimitExceeded", "You have requested more instances (%d) than your current instance limit of %d allows.", minCount, instanceLimit)
	}
	if l.image = w.image(c.region, p.get("ImageId")); l.image == nil {
		return l, errNotFound("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", p.get("ImageId"))
	}
	if l.image.State != "available" {
		return l, clientError("InvalidAMIID.Unavailable", "The image id '[%s]' is not available", l.image.ID)
	}
	if l.image.SnapshotID != "" {
		if snap := w.snapshot(c.region, l.image.SnapshotID); snap != nil {
			l.imageRef = snap.ImageRef
		}
	}
	var ok bool
	if l.instanceType, ok = s.catalog.instanceType(p.get("InstanceType")); !ok {
		return l, errInvalidParameter("Invalid value '%s' for InstanceType.", p.get("InstanceType"))
	}
	if l.keyName = p.get("KeyName"); l.keyName != "" {
		k := w.keyPair(c.region, l.keyName)
		if k == nil {
			return l, errNotFound("InvalidKeyPair.NotFound", "The key pair '%s' does not exist", l.keyName)
		}
		l.publicKey = k.PublicKey
	}
	if l.subnet, err = launchSubnet(w, c); err != nil {
		return l, err
	}
	l.publicIP = l.subnet.MapPublicIP
	if l.groups, err = launchGroups(w, c, l.subnet.VPCID); err != nil {
		return l, err
	}
	if l.userData, err = launchUserData(p.get("UserData")); err != nil {
		return l, err
	}
	if l.tags, err = tagSpecifications(p, "instance", "volume", "network-interface"); err != nil {
		return l, err
	}
	if err := s.launchOptions(c, &l); err != nil {
		return l, err
	}
	return l, nil
}

func launchSubnet(w *world, c *ec2Call) (*subnet, error) {
	zone := c.params.get("Placement.AvailabilityZone")
	if id := c.params.get("SubnetId"); id != "" {
		sn := w.subnet(c.region, id)
		if sn == nil {
			return nil, notFoundByID(id)
		}
		if zone != "" && zone != sn.AZ {
			return nil, errInvalidParameter("The subnet %s is in %s, not in the requested zone %s", id, sn.AZ, zone)
		}
		return sn, nil
	}
	def := w.defaultVPC(c.region)
	if def == nil {
		return nil, clientError("VPCIdNotSpecified", "No default VPC for this user")
	}
	for _, sn := range w.Subnets {
		if sn.VPCID == def.ID && sn.DefaultForAZ && (zone == "" || sn.AZ == zone) {
			return sn, nil
		}
	}
	return nil, errInvalidParameter("No default subnet for availability zone '%s'", zone)
}

func launchGroups(w *world, c *ec2Call, vpcID string) ([]string, error) {
	var groups []string
	for _, id := range c.params.list("SecurityGroupId") {
		g := w.securityGroup(c.region, id)
		if g == nil {
			return nil, notFoundByID(id)
		}
		if g.VPCID != vpcID {
			return nil, errInvalidParameter("Security group %s and subnet belong to different networks.", id)
		}
		groups = append(groups, id)
	}
	for _, name := range c.params.list("SecurityGroup") {
		g := find(w.SecurityGroups, func(g *securityGroup) bool { return g.VPCID == vpcID && g.Name == name })
		if g == nil {
			return nil, errNotFound("InvalidGroup.NotFound", "The security group '%s' does not exist in VPC '%s'", name, vpcID)
		}
		groups = append(groups, g.ID)
	}
	if len(groups) == 0 {
		g := find(w.SecurityGroups, func(g *securityGroup) bool { return g.VPCID == vpcID && g.Name == "default" })
		if g != nil {
			groups = append(groups, g.ID)
		}
	}
	return groups, nil
}

func launchUserData(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errInvalidParameter("Invalid BASE64 encoding of user data.")
	}
	if len(data) > maxUserData {
		return "", errInvalidParameter("User data is limited to 16384 bytes")
	}
	return encoded, nil
}

// launchOptions reads the options that change no machine behavior but that
// EC2 reports back, and refuses the ones the simulation cannot honor.
func (s *Simulation) launchOptions(c *ec2Call, l *launch) error {
	p := c.params
	if t := p.get("Placement.Tenancy"); t != "" && t != "default" {
		return errUnsupported("Tenancy %s", t)
	}
	if v := p.get("CapacityReservationSpecification.CapacityReservationPreference"); v != "" && v != "open" && v != "none" {
		return errUnsupported("Capacity reservation preference %s", v)
	}
	if p.has("CapacityReservationSpecification.CapacityReservationTarget") {
		return errUnsupported("Capacity reservation targets")
	}
	if v, _ := p.boolean("HibernationOptions.Configured"); v {
		return errUnsupported("Hibernation")
	}
	if v, _ := p.boolean("EnclaveOptions.Enabled"); v {
		return errUnsupported("Nitro Enclaves")
	}
	if p.get("IamInstanceProfile.Name") != "" || p.get("IamInstanceProfile.Arn") != "" {
		return errUnsupported("Instance profiles")
	}
	if v := p.get("PrivateDnsNameOptions.HostnameType"); v != "" && v != "ip-name" {
		return errUnsupported("Hostname type %s", v)
	}
	l.rootSize, l.rootType = l.image.VolumeSize, "gp3"
	for _, m := range p.members("BlockDeviceMapping") {
		if m.get("DeviceName") != l.image.RootDeviceName || m.has("VirtualName") || m.has("NoDevice") {
			return errUnsupported("Block devices other than the root volume")
		}
		if v, _ := m.boolean("Ebs.Encrypted"); v {
			return errUnsupported("Encrypted volumes")
		}
		size, err := m.integer("Ebs.VolumeSize", l.rootSize)
		if err != nil {
			return err
		}
		if size < l.image.VolumeSize {
			return errInvalidParameter("Volume of size %dGB is smaller than snapshot '%s', expect size >= %dGB", size, l.image.SnapshotID, l.image.VolumeSize)
		}
		l.rootSize = size
		if v := m.get("Ebs.VolumeType"); v != "" {
			l.rootType = v
		}
	}
	l.shutdown = p.get("InstanceInitiatedShutdownBehavior")
	if l.shutdown == "" {
		l.shutdown = "stop"
	}
	if l.shutdown != "stop" && l.shutdown != "terminate" {
		return errInvalidParameter("Invalid value '%s' for InstanceInitiatedShutdownBehavior", l.shutdown)
	}
	hop, err := p.integer("MetadataOptions.HttpPutResponseHopLimit", 1)
	if err != nil {
		return err
	}
	l.metadata = metadataOptions{tokens: p.get("MetadataOptions.HttpTokens"), hopLimit: hop, endpoint: p.get("MetadataOptions.HttpEndpoint"), tags: p.get("MetadataOptions.InstanceMetadataTags")}
	if l.metadata.tokens == "" {
		l.metadata.tokens = "optional"
	}
	if l.metadata.endpoint == "" {
		l.metadata.endpoint = "enabled"
	}
	if l.metadata.tags == "" {
		l.metadata.tags = "disabled"
	}
	if l.instanceType.burstable {
		l.cpuCredits = p.get("CreditSpecification.CpuCredits")
		if l.cpuCredits == "" {
			l.cpuCredits = "unlimited"
		}
	} else if p.has("CreditSpecification") {
		return errInvalidParameter("The specified instance type does not support CPU credits.")
	}
	l.disableTerm, _ = p.boolean("DisableApiTermination")
	l.disableStop, _ = p.boolean("DisableApiStop")
	l.ebsOptimized, _ = p.boolean("EbsOptimized")
	l.monitoring, _ = p.boolean("Monitoring.Enabled")
	l.clientToken = p.get("ClientToken")
	return nil
}

type reservationXML struct {
	ReservationID string        `xml:"reservationId"`
	OwnerID       string        `xml:"ownerId"`
	Instances     []instanceXML `xml:"instancesSet>item"`
}

type runInstancesResponse struct {
	ec2Meta
	reservationXML
}

type provisioning struct {
	instanceID string
	spec       machines.Spec
}

func (s *Simulation) runInstances(c *ec2Call) (ec2Response, error) {
	out := &runInstancesResponse{}
	var jobs []provisioning
	err := s.store.Update(func(w *world) error {
		w.pruneTerminated(s.now())
		if token := c.params.get("ClientToken"); token != "" {
			if existing := w.reservationByToken(c.region, token); existing != "" {
				out.reservationXML = s.renderReservation(w, existing)
				return nil
			}
		}
		l, err := s.planLaunch(w, c)
		if err != nil {
			return err
		}
		reservation := newID("r")
		out.ReservationID, out.OwnerID = reservation, w.AccountID
		for n := range l.count {
			i := s.newInstance(w, c.region, reservation, n, l)
			jobs = append(jobs, provisioning{instanceID: i.ID, spec: s.machineSpec(w, i, l)})
			out.Instances = append(out.Instances, s.renderInstance(w, i))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		s.background(func(ctx context.Context) { s.provision(ctx, job) })
	}
	return out, nil
}

func (w *world) reservationByToken(region, token string) string {
	for _, i := range w.Instances {
		if i.Region == region && i.ClientToken == token {
			return i.ReservationID
		}
	}
	return ""
}

func (s *Simulation) newInstance(w *world, region, reservation string, index int, l launch) *instance {
	i := &instance{
		ID: newID("i"), Region: region, ReservationID: reservation, LaunchIndex: index, ImageID: l.image.ID,
		InstanceType: l.instanceType.name, KeyName: l.keyName, SubnetID: l.subnet.ID, VPCID: l.subnet.VPCID, AZ: l.subnet.AZ,
		SecurityGroupIDs: l.groups, UserData: l.userData, State: "pending", LaunchTime: s.now().UTC(),
		PublicIPOnLaunch: l.publicIP, ENIID: newID("eni"), ENIAttachmentID: newID("eni-attach"), MAC: newMAC(),
		VolumeID: newID("vol"), ClientToken: l.clientToken, DisableAPITermination: l.disableTerm, DisableAPIStop: l.disableStop,
		ShutdownBehavior: l.shutdown, EBSOptimized: l.ebsOptimized, Monitoring: l.monitoring, SourceDestCheck: true,
		CPUCredits: l.cpuCredits, HTTPTokens: l.metadata.tokens, HTTPHopLimit: l.metadata.hopLimit,
		HTTPEndpoint: l.metadata.endpoint, MetadataTags: l.metadata.tags, Busy: true,
	}
	w.Instances = append(w.Instances, i)
	w.Volumes = append(w.Volumes, &volume{
		ID: i.VolumeID, Region: region, AZ: i.AZ, Size: l.rootSize, Type: l.rootType, SnapshotID: l.image.SnapshotID,
		CreateTime: i.LaunchTime, InstanceID: i.ID, Device: l.image.RootDeviceName, DeleteOnTermination: true,
	})
	for _, t := range l.tags["instance"] {
		w.setTag(i.ID, t)
	}
	for _, t := range l.tags["volume"] {
		w.setTag(i.VolumeID, t)
	}
	for _, t := range l.tags["network-interface"] {
		w.setTag(i.ENIID, t)
	}
	return i
}

func newMAC() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4])
}

func (s *Simulation) machineSpec(w *world, i *instance, l launch) machines.Spec {
	userData, _ := base64.StdEncoding.DecodeString(i.UserData)
	var keys []string
	if l.publicKey != "" {
		keys = []string{l.publicKey}
	}
	doc := instanceDocument{
		accountID: w.AccountID, instance: *i, image: l.image.ID, publicKey: l.publicKey,
		groupNames: groupNames(w, i), userData: string(userData),
	}
	return machines.Spec{
		Name:     fmt.Sprintf("stagely-sim-aws-%s-%s", w.AccountID, i.ID),
		Hostname: i.ID,
		Cloud:    cloudLabel, Account: w.AccountID, Resource: i.ID,
		Image: l.imageRef,
		Seed: machines.Seed{
			InstanceID: i.ID, PublicKeys: keys, UserData: string(userData),
			Metadata: func(privateIPv4, publicIPv4 string) map[string]string {
				if !i.PublicIPOnLaunch {
					publicIPv4 = ""
				}
				return doc.metadata(privateIPv4, publicIPv4)
			},
		},
		Firewall: policyFor(w, i),
	}
}

func groupNames(w *world, i *instance) []string {
	var names []string
	for _, id := range i.SecurityGroupIDs {
		if g := w.securityGroup(i.Region, id); g != nil {
			names = append(names, g.Name)
		}
	}
	return names
}

// provision creates the instance's machine. An instance terminated while
// its machine was created is destroyed once the machine exists.
func (s *Simulation) provision(ctx context.Context, job provisioning) {
	m, err := s.machines.Create(ctx, job.spec)
	if err != nil {
		s.log.Error("create instance machine", "instance", job.instanceID, "error", err)
	}
	s.finish(job.instanceID, func(i *instance) {
		switch {
		case err != nil:
			i.State, i.StateReason, i.TransitionReason = "terminated", "Server.InternalError", "Server.InternalError: the machine could not be created"
			i.TerminatedAt = s.now().UTC()
		case i.State == "pending":
			i.State, i.Machine, i.PrivateIP, i.MachinePublicIP = "running", m.ID, m.PrivateIP, m.PublicIP
		default:
			i.Machine, i.PrivateIP, i.MachinePublicIP = m.ID, m.PrivateIP, m.PublicIP
		}
	})
}

// finish records the result of an instance's machine work and starts the
// destruction that a termination asked for meanwhile.
func (s *Simulation) finish(id string, update func(i *instance)) {
	var destroy *destruction
	err := s.store.Update(func(w *world) error {
		i := w.instance(id)
		if i == nil {
			return nil
		}
		update(i)
		i.Busy = false
		if i.State == "shutting-down" {
			i.Busy = true
			destroy = &destruction{instanceID: i.ID, machine: i.Machine}
		}
		return nil
	})
	if err != nil {
		s.log.Error("record instance state", "instance", id, "error", err)
	}
	s.firewalls.kick()
	if destroy != nil {
		job := *destroy
		s.background(func(ctx context.Context) { s.destroy(ctx, job) })
	}
}

type destruction struct {
	instanceID string
	machine    string
}

// destroy deletes the machine of a terminated instance. The instance becomes
// terminated only when Docker confirms the machine is gone.
func (s *Simulation) destroy(ctx context.Context, job destruction) {
	if job.machine != "" {
		if err := s.machines.Delete(ctx, job.machine); err != nil {
			s.log.Error("delete instance machine", "instance", job.instanceID, "error", err)
			_ = s.store.Update(func(w *world) error {
				if i := w.instance(job.instanceID); i != nil {
					i.Busy = false
				}
				return nil
			})
			return
		}
	}
	err := s.store.Update(func(w *world) error {
		i := w.instance(job.instanceID)
		if i == nil {
			return nil
		}
		i.State, i.Busy, i.Machine, i.TerminatedAt = "terminated", false, "", s.now().UTC()
		w.releaseInstance(i)
		return nil
	})
	if err != nil {
		s.log.Error("record terminated instance", "instance", job.instanceID, "error", err)
	}
	s.firewalls.forget(job.instanceID)
	s.firewalls.kick()
}

// releaseInstance frees what a terminated instance held: its Elastic IP
// association and its root volume.
func (w *world) releaseInstance(i *instance) {
	for _, a := range w.Addresses {
		if a.InstanceID == i.ID {
			a.InstanceID, a.AssociationID = "", ""
		}
	}
	w.Volumes = slices.DeleteFunc(w.Volumes, func(v *volume) bool { return v.ID == i.VolumeID && v.DeleteOnTermination })
	w.forget(i.VolumeID)
	w.forget(i.ENIID)
}

// pruneTerminated forgets instances terminated longer ago than EC2 lists
// them.
func (w *world) pruneTerminated(now time.Time) {
	w.Instances = slices.DeleteFunc(w.Instances, func(i *instance) bool {
		gone := i.State == "terminated" && !i.TerminatedAt.IsZero() && now.Sub(i.TerminatedAt) > terminatedRetention
		if gone {
			w.forget(i.ID)
		}
		return gone
	})
}

type describeInstancesResponse struct {
	ec2Meta
	Reservations []reservationXML `xml:"reservationSet>item"`
}

func (s *Simulation) describeInstances(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeInstancesResponse{}
	err := s.store.Update(func(w *world) error {
		w.pruneTerminated(s.now())
		found, err := selectIDs(regional(w.Instances, c.region), func(i *instance) string { return i.ID }, c.params.list("InstanceId"), notFoundByID)
		if err != nil {
			return err
		}
		for _, i := range found {
			match, err := matchFilters(filters, instanceFields(w, i), w.tagsOf(i.ID))
			if err != nil {
				return err
			}
			if !match {
				continue
			}
			j := slices.IndexFunc(out.Reservations, func(r reservationXML) bool { return r.ReservationID == i.ReservationID })
			if j < 0 {
				out.Reservations = append(out.Reservations, reservationXML{ReservationID: i.ReservationID, OwnerID: w.AccountID})
				j = len(out.Reservations) - 1
			}
			out.Reservations[j].Instances = append(out.Reservations[j].Instances, s.renderInstance(w, i))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func instanceFields(w *world, i *instance) fieldFunc {
	return func(name string) ([]string, bool) {
		switch name {
		case "instance-id":
			return values(i.ID), true
		case "instance-state-name":
			return values(i.State), true
		case "instance-state-code":
			return values(fmt.Sprint(stateCodes[i.State])), true
		case "vpc-id":
			return values(i.VPCID), true
		case "subnet-id":
			return values(i.SubnetID), true
		case "image-id":
			return values(i.ImageID), true
		case "instance-type":
			return values(i.InstanceType), true
		case "key-name":
			return values(i.KeyName), true
		case "availability-zone":
			return values(i.AZ), true
		case "reservation-id":
			return values(i.ReservationID), true
		case "private-ip-address":
			return values(i.PrivateIP), true
		case "ip-address":
			return values(w.publicIP(i)), true
		case "client-token":
			return values(i.ClientToken), true
		case "instance.group-id", "group-id":
			return i.SecurityGroupIDs, true
		case "instance.group-name", "group-name":
			return groupNames(w, i), true
		case "network-interface.network-interface-id":
			return values(i.ENIID), true
		case "owner-id":
			return values(w.AccountID), true
		}
		return nil, false
	}
}

type stateChangeXML struct {
	InstanceID    string           `xml:"instanceId"`
	CurrentState  instanceStateXML `xml:"currentState"`
	PreviousState instanceStateXML `xml:"previousState"`
}

type stateChangeResponse struct {
	ec2Meta
	Instances []stateChangeXML `xml:"instancesSet>item"`
}

func stateOf(name string) instanceStateXML { return instanceStateXML{stateCodes[name], name} }

// changeStates applies change to each instance InstanceId.N names, all or
// none, and starts the machine work it returns.
func (s *Simulation) changeStates(c *ec2Call, change func(w *world, i *instance) (func(context.Context), error)) (ec2Response, error) {
	ids := c.params.list("InstanceId")
	if len(ids) == 0 {
		return nil, errMissingParameter("InstanceId")
	}
	out := &stateChangeResponse{}
	var ops []func(context.Context)
	err := s.store.Update(func(w *world) error {
		var targets []*instance
		for _, id := range ids {
			i := w.instance(id)
			if i == nil || i.Region != c.region {
				return notFoundByID(id)
			}
			targets = append(targets, i)
		}
		for _, i := range targets {
			previous := i.State
			op, err := change(w, i)
			if err != nil {
				return err
			}
			if op != nil {
				i.Busy = true
				ops = append(ops, op)
			}
			out.Instances = append(out.Instances, stateChangeXML{i.ID, stateOf(i.State), stateOf(previous)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		s.background(op)
	}
	s.firewalls.kick()
	return out, nil
}

func (s *Simulation) terminateInstances(c *ec2Call) (ec2Response, error) {
	return s.changeStates(c, func(_ *world, i *instance) (func(context.Context), error) {
		if i.State == "terminated" || i.State == "shutting-down" {
			return nil, nil
		}
		if i.DisableAPITermination {
			return nil, clientError("OperationNotPermitted", "The instance '%s' may not be terminated. Modify its 'disableApiTermination' instance attribute and try again.", i.ID)
		}
		i.State, i.StateReason = "shutting-down", "Client.UserInitiatedShutdown"
		i.TransitionReason = "User initiated (" + s.now().UTC().Format("2006-01-02 15:04:05 GMT") + ")"
		if i.Busy {
			return nil, nil
		}
		job := destruction{instanceID: i.ID, machine: i.Machine}
		return func(ctx context.Context) { s.destroy(ctx, job) }, nil
	})
}

func (s *Simulation) stopInstances(c *ec2Call) (ec2Response, error) {
	if v, _ := c.params.boolean("Hibernate"); v {
		return nil, errUnsupported("Hibernation")
	}
	force, _ := c.params.boolean("Force")
	return s.changeStates(c, func(_ *world, i *instance) (func(context.Context), error) {
		switch {
		case i.State == "stopped" || i.State == "stopping":
			return nil, nil
		case i.State != "running" || i.Busy:
			return nil, clientError("IncorrectInstanceState", "This instance '%s' is not in a state from which it can be stopped.", i.ID)
		case i.DisableAPIStop:
			return nil, clientError("OperationNotPermitted", "The instance '%s' may not be stopped. Modify its 'disableApiStop' instance attribute and try again.", i.ID)
		}
		i.State, i.StateReason = "stopping", "Client.UserInitiatedShutdown"
		i.TransitionReason = "User initiated (" + s.now().UTC().Format("2006-01-02 15:04:05 GMT") + ")"
		id, machine := i.ID, i.Machine
		return func(ctx context.Context) {
			err := s.machines.Stop(ctx, machine, !force)
			if err != nil {
				s.log.Error("stop instance machine", "instance", id, "error", err)
			}
			s.finish(id, func(i *instance) {
				if i.State != "stopping" {
					return
				}
				i.State = "stopped"
				if err != nil {
					i.State = "running"
				}
			})
		}, nil
	})
}

func (s *Simulation) startInstances(c *ec2Call) (ec2Response, error) {
	return s.changeStates(c, func(_ *world, i *instance) (func(context.Context), error) {
		switch {
		case i.State == "running" || i.State == "pending":
			return nil, nil
		case i.State != "stopped" || i.Busy:
			return nil, clientError("IncorrectInstanceState", "The instance '%s' is not in a state from which it can be started.", i.ID)
		}
		i.State, i.StateReason, i.TransitionReason = "pending", "", ""
		id, machine := i.ID, i.Machine
		return func(ctx context.Context) {
			m, err := s.machines.Start(ctx, machine)
			if err != nil {
				s.log.Error("start instance machine", "instance", id, "error", err)
			}
			s.firewalls.forget(id)
			s.finish(id, func(i *instance) {
				if i.State != "pending" {
					return
				}
				i.State = "stopped"
				if err == nil {
					i.State, i.PrivateIP, i.MachinePublicIP = "running", m.PrivateIP, m.PublicIP
				}
			})
		}, nil
	})
}

type attributeString struct {
	Value string `xml:"value"`
}

type describeInstanceAttributeResponse struct {
	ec2Meta
	InstanceID            string               `xml:"instanceId"`
	InstanceType          *attributeString     `xml:"instanceType,omitempty"`
	UserData              *attributeString     `xml:"userData,omitempty"`
	DisableAPITermination *attributeBool       `xml:"disableApiTermination,omitempty"`
	DisableAPIStop        *attributeBool       `xml:"disableApiStop,omitempty"`
	ShutdownBehavior      *attributeString     `xml:"instanceInitiatedShutdownBehavior,omitempty"`
	RootDeviceName        *attributeString     `xml:"rootDeviceName,omitempty"`
	SourceDestCheck       *attributeBool       `xml:"sourceDestCheck,omitempty"`
	EBSOptimized          *attributeBool       `xml:"ebsOptimized,omitempty"`
	ENASupport            *attributeBool       `xml:"enaSupport,omitempty"`
	GroupSet              []groupIdentifierXML `xml:"groupSet>item"`
}

func (s *Simulation) describeInstanceAttribute(c *ec2Call) (ec2Response, error) {
	id, attribute := c.params.get("InstanceId"), c.params.get("Attribute")
	out := &describeInstanceAttributeResponse{InstanceID: id}
	var err error
	s.store.Read(func(w *world) {
		i := w.instance(id)
		if i == nil || i.Region != c.region {
			err = notFoundByID(id)
			return
		}
		switch attribute {
		case "instanceType":
			out.InstanceType = &attributeString{i.InstanceType}
		case "userData":
			out.UserData = &attributeString{i.UserData}
		case "disableApiTermination":
			out.DisableAPITermination = &attributeBool{i.DisableAPITermination}
		case "disableApiStop":
			out.DisableAPIStop = &attributeBool{i.DisableAPIStop}
		case "instanceInitiatedShutdownBehavior":
			out.ShutdownBehavior = &attributeString{i.ShutdownBehavior}
		case "rootDeviceName":
			out.RootDeviceName = &attributeString{rootDeviceName}
		case "sourceDestCheck":
			out.SourceDestCheck = &attributeBool{i.SourceDestCheck}
		case "ebsOptimized":
			out.EBSOptimized = &attributeBool{i.EBSOptimized}
		case "enaSupport":
			out.ENASupport = &attributeBool{true}
		case "groupSet":
			out.GroupSet = renderGroups(w, i)
		default:
			err = errUnsupported("Instance attribute %s", attribute)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) modifyInstanceAttribute(c *ec2Call) (ec2Response, error) {
	id := c.params.get("InstanceId")
	err := s.store.Update(func(w *world) error {
		i := w.instance(id)
		if i == nil || i.Region != c.region {
			return notFoundByID(id)
		}
		p := c.params
		if (p.has("UserData") || p.has("InstanceType")) && i.State != "stopped" {
			return clientError("IncorrectInstanceState", "The instance '%s' is not in the 'stopped' state.", id)
		}
		if v := p.get("UserData.Value"); p.has("UserData") {
			if _, err := launchUserData(v); err != nil {
				return err
			}
			i.UserData = v
		}
		if v := p.get("InstanceType.Value"); v != "" {
			if _, ok := s.catalog.instanceType(v); !ok {
				return errInvalidParameter("Invalid value '%s' for InstanceType.", v)
			}
			i.InstanceType = v
		}
		if v, ok := p.boolean("DisableApiTermination.Value"); ok {
			i.DisableAPITermination = v
		}
		if v, ok := p.boolean("DisableApiStop.Value"); ok {
			i.DisableAPIStop = v
		}
		if v := p.get("InstanceInitiatedShutdownBehavior.Value"); v != "" {
			i.ShutdownBehavior = v
		}
		if v, ok := p.boolean("SourceDestCheck.Value"); ok {
			i.SourceDestCheck = v
		}
		if groups := p.list("GroupId"); len(groups) > 0 {
			for _, g := range groups {
				if sg := w.securityGroup(c.region, g); sg == nil || sg.VPCID != i.VPCID {
					return notFoundByID(g)
				}
			}
			i.SecurityGroupIDs = groups
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.firewalls.kick()
	return ok()
}

type creditSpecificationXML struct {
	InstanceID string `xml:"instanceId"`
	CPUCredits string `xml:"cpuCredits"`
}

type describeCreditSpecificationsResponse struct {
	ec2Meta
	Specifications []creditSpecificationXML `xml:"instanceCreditSpecificationSet>item"`
}

func (s *Simulation) describeInstanceCreditSpecifications(c *ec2Call) (ec2Response, error) {
	if len(c.params.filters()) > 0 {
		return nil, errUnsupported("Filters on DescribeInstanceCreditSpecifications")
	}
	out := &describeCreditSpecificationsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var found []*instance
		found, err = selectIDs(regional(w.Instances, c.region), func(i *instance) string { return i.ID }, c.params.list("InstanceId"), notFoundByID)
		for _, i := range found {
			if i.CPUCredits != "" && i.State != "terminated" {
				out.Specifications = append(out.Specifications, creditSpecificationXML{i.ID, i.CPUCredits})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// instanceDNS returns the private and public DNS names EC2 gives the
// addresses.
func instanceDNS(region, private, public string) (string, string) {
	privateDomain, publicDomain := regionDomain(region)
	var privateName, publicName string
	if private != "" {
		privateName = "ip-" + strings.ReplaceAll(private, ".", "-") + "." + privateDomain
	}
	if public != "" {
		publicName = "ec2-" + strings.ReplaceAll(public, ".", "-") + "." + publicDomain
	}
	return privateName, publicName
}

type uefiDataResponse struct {
	ec2Meta
	InstanceID string `xml:"instanceId"`
	UEFIData   string `xml:"uefiData"`
}

// getInstanceUEFIData answers that an instance has no UEFI variable store:
// simulated instances boot in legacy BIOS mode.
func (s *Simulation) getInstanceUEFIData(c *ec2Call) (ec2Response, error) {
	id := c.params.get("InstanceId")
	if !strings.HasPrefix(id, "i-") {
		return nil, clientError("InvalidInstanceID.Malformed", "Invalid id: \"%s\"", id)
	}
	var found bool
	s.store.Read(func(w *world) {
		i := w.instance(id)
		found = i != nil && i.Region == c.region
	})
	if !found {
		return nil, notFoundByID(id)
	}
	return &uefiDataResponse{InstanceID: id}, nil
}
