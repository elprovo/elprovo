package aws

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const maxRulesPerGroup = 60

type securityGroup struct {
	ID          string       `json:"id"`
	Region      string       `json:"region"`
	VPCID       string       `json:"vpc_id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Ingress     []permission `json:"ingress"`
	Egress      []permission `json:"egress"`
}

// permission is one rule: a protocol and port range from or to one source,
// a CIDR block or a security group. EC2 merges the rules that share a
// protocol and ports into one IpPermission when it reports them.
type permission struct {
	RuleID      string   `json:"rule_id"`
	Protocol    string   `json:"protocol"`
	FromPort    int      `json:"from_port"`
	ToPort      int      `json:"to_port"`
	CIDRs       []string `json:"cidrs,omitempty"`
	IPv6CIDRs   []string `json:"ipv6_cidrs,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	Description string   `json:"description,omitempty"`
}

func allowAllEgress() permission {
	return permission{RuleID: newID("sgr"), Protocol: "-1", FromPort: -1, ToPort: -1, CIDRs: []string{"0.0.0.0/0"}}
}

// source returns the rule's one source, for comparing rules.
func (p permission) source() string {
	switch {
	case len(p.CIDRs) > 0:
		return p.CIDRs[0]
	case len(p.IPv6CIDRs) > 0:
		return p.IPv6CIDRs[0]
	case len(p.Groups) > 0:
		return p.Groups[0]
	}
	return ""
}

func (p permission) sameRule(o permission) bool {
	return p.Protocol == o.Protocol && p.FromPort == o.FromPort && p.ToPort == o.ToPort && p.source() == o.source()
}

func (s *Simulation) ec2SecurityGroupActions() map[string]ec2Action {
	rules := []string{"GroupId", "GroupName", "IpPermissions", "SecurityGroupRuleId", "TagSpecification"}
	return map[string]ec2Action{
		"CreateSecurityGroup":           {s.createSecurityGroup, []string{"GroupName", "GroupDescription", "VpcId", "TagSpecification"}},
		"DescribeSecurityGroups":        {s.describeSecurityGroups, []string{"GroupId", "GroupName", "Filter", "MaxResults", "NextToken"}},
		"DeleteSecurityGroup":           {s.deleteSecurityGroup, []string{"GroupId", "GroupName"}},
		"AuthorizeSecurityGroupIngress": {func(c *ec2Call) (ec2Response, error) { return s.authorize(c, false) }, rules},
		"AuthorizeSecurityGroupEgress":  {func(c *ec2Call) (ec2Response, error) { return s.authorize(c, true) }, rules},
		"RevokeSecurityGroupIngress":    {func(c *ec2Call) (ec2Response, error) { return s.revoke(c, false) }, rules},
		"RevokeSecurityGroupEgress":     {func(c *ec2Call) (ec2Response, error) { return s.revoke(c, true) }, rules},
		"DescribeSecurityGroupRules":    {s.describeSecurityGroupRules, []string{"SecurityGroupRuleId", "Filter", "MaxResults", "NextToken"}},
	}
}

type createSecurityGroupResponse struct {
	ec2Meta
	Return           bool   `xml:"return"`
	GroupID          string `xml:"groupId"`
	SecurityGroupARN string `xml:"securityGroupArn"`
	TagSet           []tag  `xml:"tagSet>item"`
}

func (s *Simulation) createSecurityGroup(c *ec2Call) (ec2Response, error) {
	name, description := c.params.get("GroupName"), c.params.get("GroupDescription")
	switch {
	case name == "":
		return nil, errMissingParameter("GroupName")
	case description == "":
		return nil, errMissingParameter("GroupDescription")
	case strings.HasPrefix(name, "sg-"):
		return nil, errInvalidParameter("Group names may not be in the format sg-*")
	case len(name) > 255 || len(description) > 255:
		return nil, errInvalidParameter("Group names and descriptions can have up to 255 characters")
	}
	specs, err := tagSpecifications(c.params, "security-group")
	if err != nil {
		return nil, err
	}
	out := &createSecurityGroupResponse{Return: true}
	err = s.store.Update(func(w *world) error {
		vpcID := c.params.get("VpcId")
		if vpcID == "" {
			if v := w.defaultVPC(c.region); v != nil {
				vpcID = v.ID
			}
		}
		if w.vpc(c.region, vpcID) == nil {
			return notFoundByID(vpcID)
		}
		if slices.ContainsFunc(w.SecurityGroups, func(g *securityGroup) bool { return g.VPCID == vpcID && g.Name == name }) {
			return clientError("InvalidGroup.Duplicate", "The security group '%s' already exists for VPC '%s'", name, vpcID)
		}
		g := &securityGroup{ID: newID("sg"), Region: c.region, VPCID: vpcID, Name: name, Description: description, Egress: []permission{allowAllEgress()}}
		w.SecurityGroups = append(w.SecurityGroups, g)
		for _, t := range specs["security-group"] {
			w.setTag(g.ID, t)
		}
		out.GroupID, out.SecurityGroupARN, out.TagSet = g.ID, securityGroupARN(w, g), w.tagsOf(g.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func securityGroupARN(w *world, g *securityGroup) string {
	return "arn:aws:ec2:" + g.Region + ":" + w.AccountID + ":security-group/" + g.ID
}

type ipRangeXML struct {
	CIDRIP      string `xml:"cidrIp"`
	Description string `xml:"description,omitempty"`
}

type ipv6RangeXML struct {
	CIDRIPv6    string `xml:"cidrIpv6"`
	Description string `xml:"description,omitempty"`
}

type groupPairXML struct {
	GroupID     string `xml:"groupId"`
	UserID      string `xml:"userId"`
	Description string `xml:"description,omitempty"`
}

type ipPermissionXML struct {
	IPProtocol string         `xml:"ipProtocol"`
	FromPort   *int           `xml:"fromPort,omitempty"`
	ToPort     *int           `xml:"toPort,omitempty"`
	Groups     []groupPairXML `xml:"groups>item"`
	IPRanges   []ipRangeXML   `xml:"ipRanges>item"`
	IPv6Ranges []ipv6RangeXML `xml:"ipv6Ranges>item"`
}

// renderPermissions merges the rules that share a protocol and ports, as EC2
// reports them.
func renderPermissions(w *world, rules []permission) []ipPermissionXML {
	var out []ipPermissionXML
	for _, r := range rules {
		i := slices.IndexFunc(out, func(p ipPermissionXML) bool {
			return p.IPProtocol == r.Protocol && (r.Protocol == "-1" || (*p.FromPort == r.FromPort && *p.ToPort == r.ToPort))
		})
		if i < 0 {
			p := ipPermissionXML{IPProtocol: r.Protocol}
			if r.Protocol != "-1" {
				from, to := r.FromPort, r.ToPort
				p.FromPort, p.ToPort = &from, &to
			}
			out = append(out, p)
			i = len(out) - 1
		}
		for _, cidr := range r.CIDRs {
			out[i].IPRanges = append(out[i].IPRanges, ipRangeXML{cidr, r.Description})
		}
		for _, cidr := range r.IPv6CIDRs {
			out[i].IPv6Ranges = append(out[i].IPv6Ranges, ipv6RangeXML{cidr, r.Description})
		}
		for _, g := range r.Groups {
			out[i].Groups = append(out[i].Groups, groupPairXML{g, w.AccountID, r.Description})
		}
	}
	return out
}

type securityGroupXML struct {
	OwnerID          string            `xml:"ownerId"`
	GroupID          string            `xml:"groupId"`
	GroupName        string            `xml:"groupName"`
	GroupDescription string            `xml:"groupDescription"`
	VPCID            string            `xml:"vpcId"`
	SecurityGroupARN string            `xml:"securityGroupArn"`
	IPPermissions    []ipPermissionXML `xml:"ipPermissions>item"`
	IPPermissionsOut []ipPermissionXML `xml:"ipPermissionsEgress>item"`
	TagSet           []tag             `xml:"tagSet>item"`
}

type describeSecurityGroupsResponse struct {
	ec2Meta
	SecurityGroups []securityGroupXML `xml:"securityGroupInfo>item"`
}

func (s *Simulation) describeSecurityGroups(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	names := c.params.list("GroupName")
	out := &describeSecurityGroupsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var groups []*securityGroup
		groups, err = selectIDs(regional(w.SecurityGroups, c.region), func(g *securityGroup) string { return g.ID }, c.params.list("GroupId"), notFoundByID)
		if err == nil && len(names) > 0 {
			def := w.defaultVPC(c.region)
			groups, err = selectIDs(groups, func(g *securityGroup) string {
				if def != nil && g.VPCID == def.ID {
					return g.Name
				}
				return ""
			}, names, func(name string) *apiError {
				return errNotFound("InvalidGroup.NotFound", "The security group '%s' does not exist in default VPC", name)
			})
		}
		for _, g := range groups {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "group-id":
					return values(g.ID), true
				case "group-name":
					return values(g.Name), true
				case "vpc-id":
					return values(g.VPCID), true
				case "description":
					return values(g.Description), true
				case "owner-id":
					return values(w.AccountID), true
				}
				return nil, false
			}, w.tagsOf(g.ID))
			if match {
				out.SecurityGroups = append(out.SecurityGroups, securityGroupXML{
					OwnerID: w.AccountID, GroupID: g.ID, GroupName: g.Name, GroupDescription: g.Description, VPCID: g.VPCID,
					SecurityGroupARN: securityGroupARN(w, g), IPPermissions: renderPermissions(w, g.Ingress),
					IPPermissionsOut: renderPermissions(w, g.Egress), TagSet: w.tagsOf(g.ID),
				})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// groupOf finds the security group a request names by GroupId, or by
// GroupName in the default VPC.
func groupOf(w *world, c *ec2Call) (*securityGroup, error) {
	if id := c.params.get("GroupId"); id != "" {
		if g := w.securityGroup(c.region, id); g != nil {
			return g, nil
		}
		return nil, notFoundByID(id)
	}
	name := c.params.get("GroupName")
	if name == "" {
		return nil, errMissingParameter("GroupId")
	}
	def := w.defaultVPC(c.region)
	if g := find(w.SecurityGroups, func(g *securityGroup) bool { return def != nil && g.VPCID == def.ID && g.Name == name }); g != nil {
		return g, nil
	}
	return nil, errNotFound("InvalidGroup.NotFound", "The security group '%s' does not exist in default VPC", name)
}

func (s *Simulation) deleteSecurityGroup(c *ec2Call) (ec2Response, error) {
	err := s.store.Update(func(w *world) error {
		g, err := groupOf(w, c)
		if err != nil {
			return err
		}
		if g.Name == "default" {
			return clientError("CannotDelete", "the specified group: \"%s\" name: \"default\" cannot be deleted by a user", g.ID)
		}
		if slices.ContainsFunc(w.Instances, func(i *instance) bool {
			return i.State != "terminated" && slices.Contains(i.SecurityGroupIDs, g.ID)
		}) {
			return clientError("DependencyViolation", "resource %s has a dependent object", g.ID)
		}
		for _, other := range w.SecurityGroups {
			if other != g && slices.ContainsFunc(append(slices.Clone(other.Ingress), other.Egress...), func(p permission) bool { return slices.Contains(p.Groups, g.ID) }) {
				return clientError("DependencyViolation", "resource %s has a dependent object", g.ID)
			}
		}
		w.SecurityGroups = slices.DeleteFunc(w.SecurityGroups, func(x *securityGroup) bool { return x == g })
		w.forget(g.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.firewalls.kick()
	return ok()
}

// readPermissions reads IpPermissions.N as single-source rules.
func readPermissions(w *world, c *ec2Call) ([]permission, error) {
	var out []permission
	for _, m := range c.params.members("IpPermissions") {
		protocol, err := normalizeProtocol(m.get("IpProtocol"))
		if err != nil {
			return nil, err
		}
		from, to := -1, -1
		if protocol != "-1" {
			if from, err = m.integer("FromPort", -1); err != nil {
				return nil, err
			}
			if to, err = m.integer("ToPort", -1); err != nil {
				return nil, err
			}
			if protocol != "icmp" && (from < 0 || to > 65535 || from > to) {
				return nil, errInvalidParameter("Invalid port range %d-%d for protocol %s", from, to, protocol)
			}
		}
		if len(m.members("PrefixListIds")) > 0 {
			return nil, errUnsupported("Prefix lists in security group rules")
		}
		base := permission{Protocol: protocol, FromPort: from, ToPort: to}
		for _, r := range m.members("IpRanges") {
			cidr := r.get("CidrIp")
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil || !prefix.Addr().Is4() {
				return nil, errInvalidParameter("CIDR block %s is malformed", cidr)
			}
			p := base
			p.CIDRs, p.Description = []string{prefix.Masked().String()}, r.get("Description")
			out = append(out, p)
		}
		for _, r := range m.members("Ipv6Ranges") {
			cidr := r.get("CidrIpv6")
			if prefix, err := netip.ParsePrefix(cidr); err != nil || !prefix.Addr().Is6() {
				return nil, errInvalidParameter("CIDR block %s is malformed", cidr)
			}
			p := base
			p.IPv6CIDRs, p.Description = []string{cidr}, r.get("Description")
			out = append(out, p)
		}
		for _, r := range m.members("Groups") {
			id := r.get("GroupId")
			if w.securityGroup(c.region, id) == nil {
				return nil, notFoundByID(id)
			}
			p := base
			p.Groups, p.Description = []string{id}, r.get("Description")
			out = append(out, p)
		}
	}
	return out, nil
}

func normalizeProtocol(p string) (string, error) {
	switch strings.ToLower(p) {
	case "tcp", "6":
		return "tcp", nil
	case "udp", "17":
		return "udp", nil
	case "icmp", "1":
		return "icmp", nil
	case "-1", "all":
		return "-1", nil
	case "":
		return "", errMissingParameter("IpProtocol")
	}
	return "", errUnsupported("Protocol %s", strconv.Quote(p))
}

type securityGroupRuleXML struct {
	SecurityGroupRuleID  string        `xml:"securityGroupRuleId"`
	GroupID              string        `xml:"groupId"`
	GroupOwnerID         string        `xml:"groupOwnerId"`
	IsEgress             bool          `xml:"isEgress"`
	IPProtocol           string        `xml:"ipProtocol"`
	FromPort             int           `xml:"fromPort"`
	ToPort               int           `xml:"toPort"`
	CIDRIPv4             string        `xml:"cidrIpv4,omitempty"`
	CIDRIPv6             string        `xml:"cidrIpv6,omitempty"`
	ReferencedGroup      *groupPairXML `xml:"referencedGroupInfo,omitempty"`
	Description          string        `xml:"description,omitempty"`
	SecurityGroupRuleARN string        `xml:"securityGroupRuleArn"`
	TagSet               []tag         `xml:"tagSet>item"`
}

func renderRule(w *world, g *securityGroup, p permission, egress bool) securityGroupRuleXML {
	out := securityGroupRuleXML{
		SecurityGroupRuleID: p.RuleID, GroupID: g.ID, GroupOwnerID: w.AccountID, IsEgress: egress, IPProtocol: p.Protocol,
		FromPort: p.FromPort, ToPort: p.ToPort, Description: p.Description, TagSet: w.tagsOf(p.RuleID),
		SecurityGroupRuleARN: "arn:aws:ec2:" + g.Region + ":" + w.AccountID + ":security-group-rule/" + p.RuleID,
	}
	switch {
	case len(p.CIDRs) > 0:
		out.CIDRIPv4 = p.CIDRs[0]
	case len(p.IPv6CIDRs) > 0:
		out.CIDRIPv6 = p.IPv6CIDRs[0]
	case len(p.Groups) > 0:
		out.ReferencedGroup = &groupPairXML{GroupID: p.Groups[0], UserID: w.AccountID}
	}
	return out
}

type authorizeResponse struct {
	ec2Meta
	Return bool                   `xml:"return"`
	Rules  []securityGroupRuleXML `xml:"securityGroupRuleSet>item"`
}

func (s *Simulation) authorize(c *ec2Call, egress bool) (ec2Response, error) {
	out := &authorizeResponse{Return: true}
	err := s.store.Update(func(w *world) error {
		g, err := groupOf(w, c)
		if err != nil {
			return err
		}
		rules, err := readPermissions(w, c)
		if err != nil {
			return err
		}
		if len(rules) == 0 {
			return errMissingParameter("IpPermissions")
		}
		list := &g.Ingress
		if egress {
			list = &g.Egress
		}
		for _, r := range rules {
			if slices.ContainsFunc(*list, r.sameRule) {
				return clientError("InvalidPermission.Duplicate", "the specified rule \"peer: %s, %s, ALLOW\" already exists", r.source(), strings.ToUpper(r.Protocol))
			}
		}
		if len(*list)+len(rules) > maxRulesPerGroup {
			return clientError("RulesPerSecurityGroupLimitExceeded", "The maximum number of rules per security group has been reached.")
		}
		specs, err := tagSpecifications(c.params, "security-group-rule")
		if err != nil {
			return err
		}
		for _, r := range rules {
			r.RuleID = newID("sgr")
			*list = append(*list, r)
			for _, t := range specs["security-group-rule"] {
				w.setTag(r.RuleID, t)
			}
			out.Rules = append(out.Rules, renderRule(w, g, r, egress))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.firewalls.kick()
	return out, nil
}

func (s *Simulation) revoke(c *ec2Call, egress bool) (ec2Response, error) {
	err := s.store.Update(func(w *world) error {
		g, err := groupOf(w, c)
		if err != nil {
			return err
		}
		list := &g.Ingress
		if egress {
			list = &g.Egress
		}
		for _, id := range c.params.list("SecurityGroupRuleId") {
			i := slices.IndexFunc(*list, func(p permission) bool { return p.RuleID == id })
			if i < 0 {
				return errNotFound("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", id)
			}
			w.forget(id)
			*list = slices.Delete(*list, i, i+1)
		}
		rules, err := readPermissions(w, c)
		if err != nil {
			return err
		}
		for _, r := range rules {
			i := slices.IndexFunc(*list, r.sameRule)
			if i < 0 {
				return clientError("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
			}
			w.forget((*list)[i].RuleID)
			*list = slices.Delete(*list, i, i+1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.firewalls.kick()
	return ok()
}

type describeSecurityGroupRulesResponse struct {
	ec2Meta
	Rules []securityGroupRuleXML `xml:"securityGroupRuleSet>item"`
}

func (s *Simulation) describeSecurityGroupRules(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	ids := c.params.list("SecurityGroupRuleId")
	out := &describeSecurityGroupRulesResponse{}
	var err error
	s.store.Read(func(w *world) {
		for _, g := range regional(w.SecurityGroups, c.region) {
			for _, egress := range []bool{false, true} {
				list := g.Ingress
				if egress {
					list = g.Egress
				}
				for _, p := range list {
					if len(ids) > 0 && !slices.Contains(ids, p.RuleID) {
						continue
					}
					var match bool
					match, err = matchFilters(filters, func(name string) ([]string, bool) {
						switch name {
						case "group-id":
							return values(g.ID), true
						case "security-group-rule-id":
							return values(p.RuleID), true
						}
						return nil, false
					}, w.tagsOf(p.RuleID))
					if err != nil {
						return
					}
					if match {
						out.Rules = append(out.Rules, renderRule(w, g, p, egress))
					}
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(out.Rules, func(r securityGroupRuleXML) bool { return r.SecurityGroupRuleID == id }) {
			return nil, errNotFound("InvalidSecurityGroupRuleId.NotFound", "The security group rule ID '%s' does not exist", id)
		}
	}
	return out, nil
}
