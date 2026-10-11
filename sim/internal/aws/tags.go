package aws

import (
	"slices"
	"strings"
)

const maxTags = 50

// resourceTypes maps an id prefix to the resource type that tag APIs report.
var resourceTypes = map[string]string{
	"i": "instance", "vpc": "vpc", "subnet": "subnet", "igw": "internet-gateway", "rtb": "route-table",
	"sg": "security-group", "eipalloc": "elastic-ip", "ami": "image", "snap": "snapshot", "vol": "volume",
	"key": "key-pair", "acl": "network-acl", "eni": "network-interface", "dopt": "dhcp-options",
}

func resourceType(id string) string {
	prefix, _, _ := strings.Cut(id, "-")
	return resourceTypes[prefix]
}

// exists reports whether a resource with the id exists in the region.
func (w *world) exists(region, id string) bool {
	switch resourceType(id) {
	case "instance":
		i := w.instance(id)
		return i != nil && i.Region == region
	case "vpc":
		return w.vpc(region, id) != nil
	case "subnet":
		return w.subnet(region, id) != nil
	case "internet-gateway":
		return w.internetGateway(region, id) != nil
	case "route-table":
		return w.routeTable(region, id) != nil
	case "security-group":
		return w.securityGroup(region, id) != nil
	case "elastic-ip":
		return w.address(region, id) != nil
	case "image":
		return w.image(region, id) != nil
	case "snapshot":
		return w.snapshot(region, id) != nil
	case "volume":
		v := w.volume(id)
		return v != nil && v.Region == region
	case "key-pair":
		return find(w.KeyPairs, func(k *keyPair) bool { return k.ID == id && k.Region == region }) != nil
	case "network-acl":
		return w.networkACL(region, id) != nil
	case "network-interface":
		return find(w.Instances, func(i *instance) bool { return i.ENIID == id && i.Region == region }) != nil
	}
	return false
}

// readTags reads a Tag.N list of keys and values.
func readTags(p params, name string) ([]tag, error) {
	var tags []tag
	for _, m := range p.members(name) {
		t := tag{Key: m.get("Key"), Value: m.get("Value")}
		if err := checkTag(t); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	if len(tags) > maxTags {
		return nil, clientError("TagLimitExceeded", "The maximum number of tags per resource is %d", maxTags)
	}
	return tags, nil
}

func checkTag(t tag) error {
	switch {
	case t.Key == "":
		return errInvalidParameter("Tag keys must not be empty")
	case len(t.Key) > 128 || len(t.Value) > 256:
		return errInvalidParameter("Tag keys can have up to 128 characters and values up to 256")
	case strings.HasPrefix(strings.ToLower(t.Key), "aws:"):
		return errInvalidParameter("Tag keys starting with 'aws:' are reserved for internal use")
	}
	return nil
}

// tagSpecifications reads TagSpecification.N and returns the tags for each
// resource type. A type outside allowed is an error.
func tagSpecifications(p params, allowed ...string) (map[string][]tag, error) {
	out := map[string][]tag{}
	for _, spec := range p.members("TagSpecification") {
		rt := spec.get("ResourceType")
		if !slices.Contains(allowed, rt) {
			return nil, errInvalidParameter("'%s' is not a valid taggable resource type for this operation.", rt)
		}
		tags, err := readTags(spec, "Tag")
		if err != nil {
			return nil, err
		}
		out[rt] = append(out[rt], tags...)
	}
	return out, nil
}

func (s *Simulation) ec2TagActions() map[string]ec2Action {
	return map[string]ec2Action{
		"CreateTags":   {s.createTags, []string{"ResourceId", "Tag"}},
		"DeleteTags":   {s.deleteTags, []string{"ResourceId", "Tag"}},
		"DescribeTags": {s.describeTags, []string{"Filter", "MaxResults", "NextToken"}},
	}
}

func (s *Simulation) createTags(c *ec2Call) (ec2Response, error) {
	ids := c.params.list("ResourceId")
	tags, err := readTags(c.params, "Tag")
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errMissingParameter("ResourceId")
	}
	err = s.store.Update(func(w *world) error {
		for _, id := range ids {
			if !w.exists(c.region, id) {
				return notFoundByID(id)
			}
		}
		for _, id := range ids {
			for _, t := range tags {
				w.setTag(id, t)
			}
			if len(w.Tags[id]) > maxTags {
				return clientError("TagLimitExceeded", "The maximum number of tags per resource is %d", maxTags)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.firewalls.kick()
	return ok()
}

func (s *Simulation) deleteTags(c *ec2Call) (ec2Response, error) {
	ids := c.params.list("ResourceId")
	var remove []tag
	var withValue []bool
	for _, m := range c.params.members("Tag") {
		remove = append(remove, tag{Key: m.get("Key"), Value: m.get("Value")})
		withValue = append(withValue, m.has("Value"))
	}
	err := s.store.Update(func(w *world) error {
		for _, id := range ids {
			if !w.exists(c.region, id) {
				return notFoundByID(id)
			}
		}
		for _, id := range ids {
			w.Tags[id] = slices.DeleteFunc(w.Tags[id], func(t tag) bool {
				if len(remove) == 0 {
					return true
				}
				for i, r := range remove {
					if r.Key == t.Key && (!withValue[i] || r.Value == t.Value) {
						return true
					}
				}
				return false
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

type tagDescription struct {
	Key          string `xml:"key"`
	ResourceID   string `xml:"resourceId"`
	ResourceType string `xml:"resourceType"`
	Value        string `xml:"value"`
}

type describeTagsResponse struct {
	ec2Meta
	Tags []tagDescription `xml:"tagSet>item"`
}

func (s *Simulation) describeTags(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeTagsResponse{}
	var err error
	s.store.Read(func(w *world) {
		ids := make([]string, 0, len(w.Tags))
		for id := range w.Tags {
			if w.exists(c.region, id) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			for _, t := range w.Tags[id] {
				d := tagDescription{Key: t.Key, ResourceID: id, ResourceType: resourceType(id), Value: t.Value}
				var match bool
				match, err = matchFilters(filters, func(name string) ([]string, bool) {
					switch name {
					case "resource-id":
						return values(d.ResourceID), true
					case "resource-type":
						return values(d.ResourceType), true
					case "key":
						return values(d.Key), true
					case "value":
						return values(d.Value), true
					}
					return nil, false
				}, w.Tags[id])
				if err != nil {
					return
				}
				if match {
					out.Tags = append(out.Tags, d)
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// notFoundByID returns the NotFound error that EC2 gives for an id of the
// resource's type.
func notFoundByID(id string) *apiError {
	codes := map[string]string{
		"instance": "InvalidInstanceID.NotFound", "vpc": "InvalidVpcID.NotFound", "subnet": "InvalidSubnetID.NotFound",
		"internet-gateway": "InvalidInternetGatewayID.NotFound", "route-table": "InvalidRouteTableID.NotFound",
		"security-group": "InvalidGroup.NotFound", "elastic-ip": "InvalidAllocationID.NotFound", "image": "InvalidAMIID.NotFound",
		"snapshot": "InvalidSnapshot.NotFound", "volume": "InvalidVolume.NotFound", "key-pair": "InvalidKeyPair.NotFound",
		"network-acl": "InvalidNetworkAclID.NotFound", "network-interface": "InvalidNetworkInterfaceID.NotFound",
	}
	code, ok := codes[resourceType(id)]
	if !ok {
		return clientError("InvalidID", "The ID '%s' is not valid", id)
	}
	return errNotFound(code, "The ID '%s' does not exist", id)
}
