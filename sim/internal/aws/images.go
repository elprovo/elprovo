package aws

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"
)

var imageNamePattern = regexp.MustCompile(`^[a-zA-Z0-9()\[\] ./\-'@_]{3,128}$`)

type image struct {
	ID               string    `json:"id"`
	Region           string    `json:"region"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	OwnerID          string    `json:"owner_id"`
	Public           bool      `json:"public"`
	State            string    `json:"state"`
	StateReason      string    `json:"state_reason,omitempty"`
	CreationDate     time.Time `json:"creation_date"`
	SnapshotID       string    `json:"snapshot_id"`
	VolumeSize       int       `json:"volume_size"`
	RootDeviceName   string    `json:"root_device_name"`
	SourceInstanceID string    `json:"source_instance_id,omitempty"`
}

// snapshot is a volume snapshot. ImageRef names the Docker image that holds
// a snapshot the simulation took; stock images' snapshots have none and boot
// the base machine image.
type snapshot struct {
	ID          string    `json:"id"`
	Region      string    `json:"region"`
	OwnerID     string    `json:"owner_id"`
	VolumeID    string    `json:"volume_id"`
	Description string    `json:"description"`
	VolumeSize  int       `json:"volume_size"`
	State       string    `json:"state"`
	StartTime   time.Time `json:"start_time"`
	ImageRef    string    `json:"image_ref,omitempty"`
}

type volume struct {
	ID                  string    `json:"id"`
	Region              string    `json:"region"`
	AZ                  string    `json:"az"`
	Size                int       `json:"size"`
	Type                string    `json:"type"`
	SnapshotID          string    `json:"snapshot_id"`
	CreateTime          time.Time `json:"create_time"`
	InstanceID          string    `json:"instance_id,omitempty"`
	Device              string    `json:"device,omitempty"`
	DeleteOnTermination bool      `json:"delete_on_termination"`
}

// addStockImages adds the region's public stock images, each with its
// snapshot, unless the state has them already.
func addStockImages(w *world, region string) {
	for _, img := range stockImages {
		id := stockImageID(img, region)
		if w.image(region, id) != nil {
			continue
		}
		created, _ := time.Parse("2006-01-02T15:04:05.000Z", img.creationDate)
		snap := stockSnapshotID(img, region)
		w.Images = append(w.Images, &image{
			ID: id, Region: region, Name: img.name, Description: img.description, OwnerID: canonicalOwner, Public: true,
			State: "available", CreationDate: created, SnapshotID: snap, VolumeSize: 8, RootDeviceName: rootDeviceName,
		})
		w.Snapshots = append(w.Snapshots, &snapshot{
			ID: snap, Region: region, OwnerID: canonicalOwner, VolumeID: "vol-ffffffff", VolumeSize: 8, State: "completed", StartTime: created,
		})
	}
}

func (s *Simulation) ec2ImageActions() map[string]ec2Action {
	return map[string]ec2Action{
		"CreateImage":       {s.createImage, []string{"InstanceId", "Name", "Description", "NoReboot", "TagSpecification"}},
		"DescribeImages":    {s.describeImages, []string{"ImageId", "Owner", "ExecutableBy", "Filter", "IncludeDeprecated", "IncludeDisabled", "MaxResults", "NextToken"}},
		"DeregisterImage":   {s.deregisterImage, []string{"ImageId", "DeleteAssociatedSnapshots"}},
		"DescribeSnapshots": {s.describeSnapshots, []string{"SnapshotId", "Owner", "RestorableBy", "Filter", "MaxResults", "NextToken"}},
		"DeleteSnapshot":    {s.deleteSnapshot, []string{"SnapshotId"}},
		"DescribeVolumes":   {s.describeVolumes, []string{"VolumeId", "Filter", "MaxResults", "NextToken"}},
	}
}

type createImageResponse struct {
	ec2Meta
	ImageID string `xml:"imageId"`
}

// createImage commits the instance's machine into a Docker image behind a
// new snapshot, and registers an AMI on it. The AMI is pending until the
// commit completes. The instance is not rebooted.
func (s *Simulation) createImage(c *ec2Call) (ec2Response, error) {
	id, name := c.params.get("InstanceId"), c.params.get("Name")
	if !imageNamePattern.MatchString(name) {
		return nil, clientError("InvalidAMIName.Malformed", "AMI names must be between 3 and 128 characters long, and may contain letters, numbers, '(', ')', '.', '-', '/' and '_'")
	}
	specs, err := tagSpecifications(c.params, "image", "snapshot")
	if err != nil {
		return nil, err
	}
	out := &createImageResponse{}
	var job func(context.Context)
	err = s.store.Update(func(w *world) error {
		i := w.instance(id)
		if i == nil || i.Region != c.region {
			return notFoundByID(id)
		}
		if (i.State != "running" && i.State != "stopped") || i.Busy || i.Machine == "" {
			return clientError("IncorrectInstanceState", "Instance '%s' is not in a state from which it can be imaged.", id)
		}
		if slices.ContainsFunc(w.Images, func(img *image) bool { return img.Region == c.region && img.OwnerID == w.AccountID && img.Name == name }) {
			return clientError("InvalidAMIName.Duplicate", "AMI name %s is already in use by AMI %s", name, id)
		}
		vol := w.volume(i.VolumeID)
		size := 8
		if vol != nil {
			size = vol.Size
		}
		img := &image{
			ID: newID("ami"), Region: c.region, Name: name, Description: c.params.get("Description"), OwnerID: w.AccountID,
			State: "pending", CreationDate: s.now().UTC(), SnapshotID: newID("snap"), VolumeSize: size, RootDeviceName: rootDeviceName, SourceInstanceID: id,
		}
		snap := &snapshot{
			ID: img.SnapshotID, Region: c.region, OwnerID: w.AccountID, VolumeID: i.VolumeID, VolumeSize: size, State: "pending",
			StartTime: s.now().UTC(), Description: fmt.Sprintf("Created by CreateImage(%s) for %s", id, img.ID),
			ImageRef: fmt.Sprintf("elprovo-sim-aws-image:%s-%s", w.AccountID, img.SnapshotID),
		}
		w.Snapshots = append(w.Snapshots, snap)
		w.Images = append(w.Images, img)
		for _, t := range specs["image"] {
			w.setTag(img.ID, t)
		}
		for _, t := range specs["snapshot"] {
			w.setTag(snap.ID, t)
		}
		i.Busy = true
		out.ImageID = img.ID
		machine, imageID, snapID, ref := i.Machine, img.ID, snap.ID, snap.ImageRef
		job = func(ctx context.Context) { s.takeImage(ctx, id, machine, imageID, snapID, ref) }
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.background(job)
	return out, nil
}

func (s *Simulation) takeImage(ctx context.Context, instanceID, machine, imageID, snapID, ref string) {
	size, err := s.machines.Commit(ctx, machine, ref)
	if err != nil {
		s.log.Error("commit instance machine", "instance", instanceID, "error", err)
	}
	updateErr := s.store.Update(func(w *world) error {
		for _, img := range w.Images {
			if img.ID != imageID {
				continue
			}
			img.State = "available"
			if err != nil {
				img.State, img.StateReason = "failed", "Server.InternalError"
			}
		}
		for _, snap := range w.Snapshots {
			if snap.ID != snapID {
				continue
			}
			snap.State = "completed"
			if err != nil {
				snap.State = "error"
			} else {
				snap.VolumeSize = max(snap.VolumeSize, int(math.Ceil(float64(size)/(1<<30))))
			}
		}
		return nil
	})
	if updateErr != nil {
		s.log.Error("record image", "image", imageID, "error", updateErr)
	}
	s.finish(instanceID, func(*instance) {})
}

type ebsDeviceXML struct {
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
	SnapshotID          string `xml:"snapshotId"`
	VolumeSize          int    `xml:"volumeSize"`
	VolumeType          string `xml:"volumeType"`
	Encrypted           bool   `xml:"encrypted"`
	IOPS                int    `xml:"iops"`
	Throughput          int    `xml:"throughput"`
}

type blockDeviceXML struct {
	DeviceName string       `xml:"deviceName"`
	EBS        ebsDeviceXML `xml:"ebs"`
}

type imageXML struct {
	ImageID            string           `xml:"imageId"`
	ImageLocation      string           `xml:"imageLocation"`
	ImageState         string           `xml:"imageState"`
	ImageOwnerID       string           `xml:"imageOwnerId"`
	CreationDate       string           `xml:"creationDate"`
	IsPublic           bool             `xml:"isPublic"`
	Architecture       string           `xml:"architecture"`
	ImageType          string           `xml:"imageType"`
	SriovNetSupport    string           `xml:"sriovNetSupport"`
	ENASupport         bool             `xml:"enaSupport"`
	PlatformDetails    string           `xml:"platformDetails"`
	UsageOperation     string           `xml:"usageOperation"`
	Name               string           `xml:"name"`
	Description        string           `xml:"description,omitempty"`
	RootDeviceType     string           `xml:"rootDeviceType"`
	RootDeviceName     string           `xml:"rootDeviceName"`
	BlockDeviceMapping []blockDeviceXML `xml:"blockDeviceMapping>item"`
	VirtualizationType string           `xml:"virtualizationType"`
	Hypervisor         string           `xml:"hypervisor"`
	BootMode           string           `xml:"bootMode"`
	SourceInstanceID   string           `xml:"sourceInstanceId,omitempty"`
	StateReason        *stateReasonXML  `xml:"stateReason,omitempty"`
	TagSet             []tag            `xml:"tagSet>item"`
}

func renderImage(w *world, img *image) imageXML {
	out := imageXML{
		ImageID: img.ID, ImageLocation: img.OwnerID + "/" + img.Name, ImageState: img.State, ImageOwnerID: img.OwnerID,
		CreationDate: timestamp(img.CreationDate), IsPublic: img.Public, Architecture: "x86_64", ImageType: "machine",
		SriovNetSupport: "simple", ENASupport: true, PlatformDetails: "Linux/UNIX", UsageOperation: "RunInstances",
		Name: img.Name, Description: img.Description, RootDeviceType: "ebs", RootDeviceName: img.RootDeviceName,
		BlockDeviceMapping: []blockDeviceXML{{img.RootDeviceName, ebsDeviceXML{true, img.SnapshotID, img.VolumeSize, "gp3", false, 3000, 125}}},
		VirtualizationType: "hvm", Hypervisor: "xen", BootMode: "uefi-preferred", SourceInstanceID: img.SourceInstanceID,
		TagSet: w.tagsOf(img.ID),
	}
	if img.StateReason != "" {
		out.StateReason = &stateReasonXML{img.StateReason, img.StateReason}
	}
	return out
}

type describeImagesResponse struct {
	ec2Meta
	Images []imageXML `xml:"imagesSet>item"`
}

func (s *Simulation) describeImages(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	owners := c.params.list("Owner")
	executableBy := c.params.list("ExecutableBy")
	out := &describeImagesResponse{}
	var err error
	s.store.Read(func(w *world) {
		var images []*image
		images, err = selectIDs(regional(w.Images, c.region), func(img *image) string { return img.ID }, c.params.list("ImageId"), func(id string) *apiError {
			return errNotFound("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", id)
		})
		for _, img := range images {
			if err != nil {
				return
			}
			if !img.Public && img.OwnerID != w.AccountID {
				continue
			}
			if len(owners) > 0 && !slices.ContainsFunc(owners, func(o string) bool { return o == img.OwnerID || (o == "self" && img.OwnerID == w.AccountID) }) {
				continue
			}
			if len(executableBy) > 0 && !slices.ContainsFunc(executableBy, func(e string) bool {
				return (e == "all" && img.Public) || ((e == "self" || e == w.AccountID) && img.OwnerID == w.AccountID)
			}) {
				continue
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "name":
					return values(img.Name), true
				case "image-id":
					return values(img.ID), true
				case "state":
					return values(img.State), true
				case "owner-id":
					return values(img.OwnerID), true
				case "owner-alias":
					return nil, true
				case "architecture":
					return values("x86_64"), true
				case "virtualization-type":
					return values("hvm"), true
				case "root-device-type":
					return values("ebs"), true
				case "root-device-name":
					return values(img.RootDeviceName), true
				case "image-type":
					return values("machine"), true
				case "is-public":
					return boolString(img.Public), true
				case "description":
					return values(img.Description), true
				case "ena-support":
					return boolString(true), true
				case "block-device-mapping.snapshot-id":
					return values(img.SnapshotID), true
				case "block-device-mapping.volume-type":
					return values("gp3"), true
				}
				return nil, false
			}, w.tagsOf(img.ID))
			if match {
				out.Images = append(out.Images, renderImage(w, img))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type deregisterImageResponse struct {
	ec2Meta
	Return bool `xml:"return"`
}

// deregisterImage removes an AMI the account owns. Its snapshot stays unless
// DeleteAssociatedSnapshots asks otherwise, as in EC2.
func (s *Simulation) deregisterImage(c *ec2Call) (ec2Response, error) {
	id := c.params.get("ImageId")
	deleteSnapshots, _ := c.params.boolean("DeleteAssociatedSnapshots")
	var ref string
	err := s.store.Update(func(w *world) error {
		img := w.image(c.region, id)
		if img == nil {
			return errNotFound("InvalidAMIID.NotFound", "The image id '[%s]' does not exist", id)
		}
		if img.OwnerID != w.AccountID {
			return &apiError{401, "AuthFailure", "Not authorized for image:" + id}
		}
		if img.State == "pending" {
			return clientError("InvalidAMIID.Unavailable", "The image '%s' is still being created", id)
		}
		w.Images = slices.DeleteFunc(w.Images, func(x *image) bool { return x == img })
		w.forget(id)
		if snap := w.snapshot(c.region, img.SnapshotID); deleteSnapshots && snap != nil && snap.OwnerID == w.AccountID {
			ref = snap.ImageRef
			w.Snapshots = slices.DeleteFunc(w.Snapshots, func(x *snapshot) bool { return x == snap })
			w.forget(snap.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ref != "" {
		if err := s.machines.RemoveImage(c.ctx, ref); err != nil {
			return nil, err
		}
	}
	return &deregisterImageResponse{Return: true}, nil
}

type snapshotXML struct {
	SnapshotID  string `xml:"snapshotId"`
	VolumeID    string `xml:"volumeId"`
	Status      string `xml:"status"`
	StartTime   string `xml:"startTime"`
	Progress    string `xml:"progress"`
	OwnerID     string `xml:"ownerId"`
	VolumeSize  int    `xml:"volumeSize"`
	Description string `xml:"description"`
	Encrypted   bool   `xml:"encrypted"`
	StorageTier string `xml:"storageTier"`
	TagSet      []tag  `xml:"tagSet>item"`
}

type describeSnapshotsResponse struct {
	ec2Meta
	Snapshots []snapshotXML `xml:"snapshotSet>item"`
}

func (s *Simulation) describeSnapshots(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	ids := c.params.list("SnapshotId")
	owners := c.params.list("Owner")
	out := &describeSnapshotsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var snaps []*snapshot
		snaps, err = selectIDs(regional(w.Snapshots, c.region), func(sn *snapshot) string { return sn.ID }, ids, notFoundByID)
		for _, sn := range snaps {
			if err != nil {
				return
			}
			if len(ids) == 0 && sn.OwnerID != w.AccountID {
				continue
			}
			if len(owners) > 0 && !slices.ContainsFunc(owners, func(o string) bool { return o == sn.OwnerID || (o == "self" && sn.OwnerID == w.AccountID) }) {
				continue
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "snapshot-id":
					return values(sn.ID), true
				case "volume-id":
					return values(sn.VolumeID), true
				case "status":
					return values(sn.State), true
				case "owner-id":
					return values(sn.OwnerID), true
				case "description":
					return values(sn.Description), true
				}
				return nil, false
			}, w.tagsOf(sn.ID))
			if !match {
				continue
			}
			progress := "100%"
			if sn.State == "pending" {
				progress = "0%"
			}
			out.Snapshots = append(out.Snapshots, snapshotXML{
				SnapshotID: sn.ID, VolumeID: sn.VolumeID, Status: sn.State, StartTime: timestamp(sn.StartTime), Progress: progress,
				OwnerID: sn.OwnerID, VolumeSize: sn.VolumeSize, Description: sn.Description, StorageTier: "standard", TagSet: w.tagsOf(sn.ID),
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deleteSnapshot deletes a snapshot the account owns, with its Docker
// image. A snapshot behind a registered AMI is in use.
func (s *Simulation) deleteSnapshot(c *ec2Call) (ec2Response, error) {
	id := c.params.get("SnapshotId")
	var ref string
	err := s.store.Update(func(w *world) error {
		snap := w.snapshot(c.region, id)
		if snap == nil {
			return notFoundByID(id)
		}
		if snap.OwnerID != w.AccountID {
			return &apiError{401, "AuthFailure", "Not authorized for snapshot " + id}
		}
		if img := find(w.Images, func(img *image) bool { return img.SnapshotID == id }); img != nil {
			return clientError("InvalidSnapshot.InUse", "The snapshot %s is currently in use by %s", id, img.ID)
		}
		ref = snap.ImageRef
		w.Snapshots = slices.DeleteFunc(w.Snapshots, func(x *snapshot) bool { return x == snap })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ref != "" {
		if err := s.machines.RemoveImage(c.ctx, ref); err != nil {
			return nil, err
		}
	}
	return ok()
}

type volumeAttachmentXML struct {
	VolumeID            string `xml:"volumeId"`
	InstanceID          string `xml:"instanceId"`
	Device              string `xml:"device"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type volumeXML struct {
	VolumeID           string                `xml:"volumeId"`
	Size               int                   `xml:"size"`
	SnapshotID         string                `xml:"snapshotId"`
	AvailabilityZone   string                `xml:"availabilityZone"`
	AvailabilityZoneID string                `xml:"availabilityZoneId"`
	Status             string                `xml:"status"`
	CreateTime         string                `xml:"createTime"`
	Attachments        []volumeAttachmentXML `xml:"attachmentSet>item"`
	VolumeType         string                `xml:"volumeType"`
	IOPS               int                   `xml:"iops"`
	Throughput         int                   `xml:"throughput"`
	Encrypted          bool                  `xml:"encrypted"`
	MultiAttachEnabled bool                  `xml:"multiAttachEnabled"`
	TagSet             []tag                 `xml:"tagSet>item"`
}

type describeVolumesResponse struct {
	ec2Meta
	Volumes []volumeXML `xml:"volumeSet>item"`
}

func (s *Simulation) describeVolumes(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeVolumesResponse{}
	var err error
	s.store.Read(func(w *world) {
		var vols []*volume
		vols, err = selectIDs(regional(w.Volumes, c.region), func(v *volume) string { return v.ID }, c.params.list("VolumeId"), notFoundByID)
		for _, v := range vols {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "volume-id":
					return values(v.ID), true
				case "attachment.instance-id":
					return values(v.InstanceID), true
				case "attachment.device":
					return values(v.Device), true
				case "attachment.delete-on-termination":
					return boolString(v.DeleteOnTermination), true
				case "availability-zone":
					return values(v.AZ), true
				case "status":
					return values("in-use"), true
				case "snapshot-id":
					return values(v.SnapshotID), true
				case "volume-type":
					return values(v.Type), true
				}
				return nil, false
			}, w.tagsOf(v.ID))
			if !match {
				continue
			}
			created := timestamp(v.CreateTime)
			out.Volumes = append(out.Volumes, volumeXML{
				VolumeID: v.ID, Size: v.Size, SnapshotID: v.SnapshotID, AvailabilityZone: v.AZ, AvailabilityZoneID: zoneID(v.AZ),
				Status: "in-use", CreateTime: created, VolumeType: v.Type, IOPS: 3000, Throughput: 125, TagSet: w.tagsOf(v.ID),
				Attachments: []volumeAttachmentXML{{v.ID, v.InstanceID, v.Device, "attached", created, v.DeleteOnTermination}},
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
