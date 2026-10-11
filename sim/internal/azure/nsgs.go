package azure

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	minRulePriority = 100
	maxRulePriority = 4096
)

type nsg struct {
	meta
	Rules []ruleJSON `json:"rules"`
}

type ruleJSON struct {
	ID         string         `json:"id,omitempty"`
	Name       string         `json:"name"`
	Etag       string         `json:"etag,omitempty"`
	Type       string         `json:"type,omitempty"`
	Properties ruleProperties `json:"properties"`
}

type ruleProperties struct {
	Description                          string   `json:"description,omitempty"`
	Protocol                             string   `json:"protocol"`
	SourcePortRange                      string   `json:"sourcePortRange,omitempty"`
	DestinationPortRange                 string   `json:"destinationPortRange,omitempty"`
	SourceAddressPrefix                  string   `json:"sourceAddressPrefix,omitempty"`
	DestinationAddressPrefix             string   `json:"destinationAddressPrefix,omitempty"`
	SourcePortRanges                     []string `json:"sourcePortRanges"`
	DestinationPortRanges                []string `json:"destinationPortRanges"`
	SourceAddressPrefixes                []string `json:"sourceAddressPrefixes"`
	DestinationAddressPrefixes           []string `json:"destinationAddressPrefixes"`
	SourceApplicationSecurityGroups      []any    `json:"sourceApplicationSecurityGroups,omitempty"`
	DestinationApplicationSecurityGroups []any    `json:"destinationApplicationSecurityGroups,omitempty"`
	Access                               string   `json:"access"`
	Priority                             int      `json:"priority"`
	Direction                            string   `json:"direction"`
	ProvisioningState                    string   `json:"provisioningState,omitempty"`
}

func (p ruleProperties) sources() []string {
	return append(nonEmpty(p.SourceAddressPrefix), p.SourceAddressPrefixes...)
}

func (p ruleProperties) destinations() []string {
	return append(nonEmpty(p.DestinationAddressPrefix), p.DestinationAddressPrefixes...)
}

func (p ruleProperties) sourcePorts() []string {
	return append(nonEmpty(p.SourcePortRange), p.SourcePortRanges...)
}

func (p ruleProperties) destinationPorts() []string {
	return append(nonEmpty(p.DestinationPortRange), p.DestinationPortRanges...)
}

func nonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

type nsgProperties struct {
	SecurityRules        []ruleJSON     `json:"securityRules"`
	DefaultSecurityRules []ruleJSON     `json:"defaultSecurityRules,omitempty"`
	Subnets              []*subResource `json:"subnets,omitempty"`
	NetworkInterfaces    []*subResource `json:"networkInterfaces,omitempty"`
	ResourceGUID         string         `json:"resourceGuid,omitempty"`
	ProvisioningState    string         `json:"provisioningState,omitempty"`
}

// defaultRules are the rules every network security group has, below the
// ones its owner adds.
var defaultRules = []ruleJSON{
	defaultRule("AllowVnetInBound", "Allow inbound traffic from all VMs in VNET", "VirtualNetwork", "VirtualNetwork", "Allow", 65000, "Inbound"),
	defaultRule("AllowAzureLoadBalancerInBound", "Allow inbound traffic from azure load balancer", "AzureLoadBalancer", "*", "Allow", 65001, "Inbound"),
	defaultRule("DenyAllInBound", "Deny all inbound traffic", "*", "*", "Deny", 65500, "Inbound"),
	defaultRule("AllowVnetOutBound", "Allow outbound traffic from all VMs to all VMs in VNET", "VirtualNetwork", "VirtualNetwork", "Allow", 65000, "Outbound"),
	defaultRule("AllowInternetOutBound", "Allow outbound traffic from all VMs to Internet", "*", "Internet", "Allow", 65001, "Outbound"),
	defaultRule("DenyAllOutBound", "Deny all outbound traffic", "*", "*", "Deny", 65500, "Outbound"),
}

func defaultRule(name, description, source, destination, access string, priority int, direction string) ruleJSON {
	return ruleJSON{Name: name, Properties: ruleProperties{
		Description: description, Protocol: "*", SourcePortRange: "*", DestinationPortRange: "*",
		SourceAddressPrefix: source, DestinationAddressPrefix: destination, Access: access, Priority: priority, Direction: direction,
	}}
}

func (s *Simulation) renderRules(n *nsg, rules []ruleJSON, kind string) []ruleJSON {
	out := make([]ruleJSON, 0, len(rules))
	for _, rule := range rules {
		rule.ID = s.resourceID(n.Group, typeNSG, n.Name, kind, rule.Name)
		rule.Type = "Microsoft.Network/networkSecurityGroups/" + kind
		if rule.Etag == "" {
			rule.Etag = n.Etag
		}
		props := &rule.Properties
		props.ProvisioningState = statusSucceeded
		props.SourcePortRanges, props.DestinationPortRanges = nonNil(props.SourcePortRanges), nonNil(props.DestinationPortRanges)
		props.SourceAddressPrefixes, props.DestinationAddressPrefixes = nonNil(props.SourceAddressPrefixes), nonNil(props.DestinationAddressPrefixes)
		out = append(out, rule)
	}
	return out
}

func (s *Simulation) renderNSG(sub *subscription, n *nsg) resourceJSON {
	props := nsgProperties{
		SecurityRules: s.renderRules(n, n.Rules, "securityRules"), DefaultSecurityRules: s.renderRules(n, defaultRules, "defaultSecurityRules"),
		ResourceGUID: n.GUID, ProvisioningState: statusSucceeded,
	}
	id := s.idFor(typeNSG, &n.meta)
	for _, v := range sub.VNets {
		for _, sn := range v.Subnets {
			if sameID(idOf(sn.Properties.NetworkSecurityGroup), id) {
				props.Subnets = append(props.Subnets, &subResource{ID: s.resourceID(v.Group, typeVNet, v.Name, "subnets", sn.Name)})
			}
		}
	}
	for _, nc := range sub.NICs {
		if sameID(idOf(nc.Properties.NetworkSecurityGroup), id) {
			props.NetworkInterfaces = append(props.NetworkInterfaces, &subResource{ID: s.idFor(typeNIC, &nc.meta)})
		}
	}
	return resourceJSON{ID: id, Name: n.Name, Type: typeNSG, Location: n.Location, Tags: cloneTags(n.Tags), Etag: n.Etag, Properties: props}
}

// serviceTags are the address tags the simulation understands. Each stands
// for addresses that the firewall computes; see firewall.go.
var serviceTags = []string{"*", "Any", "Internet", "VirtualNetwork", "AzureLoadBalancer"}

func checkAddress(rule, value string) error {
	if slices.ContainsFunc(serviceTags, func(t string) bool { return strings.EqualFold(t, value) }) {
		return nil
	}
	if p, err := netip.ParsePrefix(value); err == nil {
		if !p.Addr().Is4() {
			return errUnsupported("The IPv6 address %s in security rule %s", value, rule)
		}
		return nil
	}
	if a, err := netip.ParseAddr(value); err == nil {
		if !a.Is4() {
			return errUnsupported("The IPv6 address %s in security rule %s", value, rule)
		}
		return nil
	}
	if strings.ContainsAny(value, ".:/") {
		return errBadRequest("SecurityRuleInvalidAddressPrefix", "Security rule %s has invalid Address prefix. Value provided: %s", rule, value)
	}
	return errUnsupported("The service tag %s in security rule %s", value, rule)
}

// parsePortRange reads *, a port, or an inclusive range such as 8000-9000.
func parsePortRange(value string) (lo, hi int, ok bool) {
	if value == "*" {
		return 0, 65535, true
	}
	l, h, isRange := strings.Cut(value, "-")
	if !isRange {
		h = l
	}
	lo, err1 := strconv.Atoi(l)
	hi, err2 := strconv.Atoi(h)
	return lo, hi, err1 == nil && err2 == nil && lo >= 0 && hi <= 65535 && lo <= hi
}

// normalizeRule checks a rule and spells its enums as Azure does.
func normalizeRule(rule *ruleJSON) error {
	p := &rule.Properties
	name := rule.Name
	if !networkNamePattern.MatchString(name) {
		return errInvalidParameter("Security rule name '%s' is invalid.", name)
	}
	var ok bool
	if p.Protocol, ok = canonical(p.Protocol, "Tcp", "Udp", "Icmp", "*"); !ok {
		if _, other := canonical(p.Protocol, "Esp", "Ah"); other {
			return errUnsupported("The protocol %s in security rule %s", p.Protocol, name)
		}
		return errBadRequest("SecurityRuleInvalidProtocol", "Security rule %s has invalid Protocol. Value provided: %s. Allowed values: Tcp, Udp, Icmp, Esp, Ah, *.", name, p.Protocol)
	}
	if p.Access, ok = canonical(p.Access, "Allow", "Deny"); !ok {
		return errBadRequest("SecurityRuleInvalidAccess", "Security rule %s has invalid Access. Value provided: %s", name, p.Access)
	}
	if p.Direction, ok = canonical(p.Direction, "Inbound", "Outbound"); !ok {
		return errBadRequest("SecurityRuleInvalidDirection", "Security rule %s has invalid Direction. Value provided: %s", name, p.Direction)
	}
	if p.Priority < minRulePriority || p.Priority > maxRulePriority {
		return errBadRequest("SecurityRuleInvalidPriority", "Security rule has invalid Priority. Value provided: %d Allowed range %d-%d.", p.Priority, minRulePriority, maxRulePriority)
	}
	if len(p.SourceApplicationSecurityGroups)+len(p.DestinationApplicationSecurityGroups) > 0 {
		return errUnsupported("An application security group in security rule %s", name)
	}
	for _, v := range p.sourcePorts() {
		if lo, hi, ok := parsePortRange(v); !ok || lo != 0 || hi != 65535 {
			return errUnsupported("The source port range %s in security rule %s; use *", v, name)
		}
	}
	if len(p.destinationPorts()) == 0 || len(p.sources()) == 0 || len(p.destinations()) == 0 {
		return errBadRequest("SecurityRuleParameterMissing", "Security rule %s needs a destination port range and source and destination address prefixes.", name)
	}
	for _, v := range p.destinationPorts() {
		if _, _, ok := parsePortRange(v); !ok {
			return errBadRequest("SecurityRuleInvalidPortRange", "Security rule has invalid Port range. Value provided: %s. Value should be an integer OR integer range with '-' delimiter. Valid range 0-65535.", v)
		}
	}
	for _, v := range append(p.sources(), p.destinations()...) {
		if err := checkAddress(name, v); err != nil {
			return err
		}
	}
	return nil
}

// canonical returns value in the spelling of the allowed value it matches
// without case.
func canonical(value string, allowed ...string) (string, bool) {
	for _, a := range allowed {
		if strings.EqualFold(a, value) {
			return a, true
		}
	}
	return value, false
}

func checkRules(rules []ruleJSON) error {
	seen := map[string]string{}
	for i := range rules {
		if err := normalizeRule(&rules[i]); err != nil {
			return err
		}
		p := rules[i].Properties
		key := p.Direction + "/" + strconv.Itoa(p.Priority)
		if other, ok := seen[key]; ok {
			return errBadRequest("SecurityRuleConflict", "Security rule %s conflicts with rule %s. Rules cannot have the same Priority and Direction.", rules[i].Name, other)
		}
		seen[key] = rules[i].Name
	}
	return nil
}

func (s *Simulation) putNSG(r *http.Request, p params) (reply, error) {
	var req resourceRequest[nsgProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	if !networkNamePattern.MatchString(p["name"]) {
		return reply{}, errInvalidParameter("Resource name %s is invalid.", p["name"])
	}
	rules := slices.Clone(req.Properties.SecurityRules)
	if err := checkRules(rules); err != nil {
		return reply{}, err
	}
	var out resourceJSON
	var op *operation
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		n := lookup(sub.NSGs, g.Name, p["name"])
		if n == nil {
			n = &nsg{meta: meta{Group: g.Name, Name: p["name"], GUID: newUUID()}}
			sub.NSGs = append(sub.NSGs, n)
			status = http.StatusCreated
		} else if n.Location != location {
			return errBadRequest("InvalidResourceLocation", "The resource '%s' already exists in location '%s'.", n.Name, n.Location)
		}
		n.Location, n.Tags, n.Etag = location, cloneTags(req.Tags), etag()
		n.Rules = make([]ruleJSON, 0, len(rules))
		for _, rule := range rules {
			n.Rules = append(n.Rules, ruleJSON{Name: rule.Name, Properties: rule.Properties})
		}
		op = s.doneOperation(sub, "Microsoft.Network", location)
		out = s.renderNSG(sub, n)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	s.firewalls.kick()
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

func (s *Simulation) getNSG(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if n := lookup(sub.NSGs, p["group"], p["name"]); n != nil {
			j := s.renderNSG(sub, n)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeNSG, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listNSGs(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, n := range inGroup(sub.NSGs, p["group"]) {
			out = append(out, s.renderNSG(sub, n))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deleteNSG(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		n := lookup(sub.NSGs, p["group"], p["name"])
		if n == nil {
			return nil
		}
		rendered := s.renderNSG(sub, n).Properties.(nsgProperties)
		if users := append(rendered.Subnets, rendered.NetworkInterfaces...); len(users) > 0 {
			ids := make([]string, 0, len(users))
			for _, u := range users {
				ids = append(ids, u.ID)
			}
			return errBadRequest("InUseNetworkSecurityGroupCannotBeDeleted", "Network security group %s cannot be deleted because it is in use by the following resources: %s. In order to delete the Network security group, remove the association with the resource(s).", s.idFor(typeNSG, &n.meta), strings.Join(ids, ", "))
		}
		sub.NSGs = without(sub.NSGs, n.Group, n.Name)
		op = s.doneOperation(sub, "Microsoft.Network", n.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}

func (s *Simulation) nsgByID(sub *subscription, id string) *nsg {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typeNSG || r.child != "" {
		return nil
	}
	return lookup(sub.NSGs, r.group, r.name)
}

func (s *Simulation) missingReference(id, user string) *armError {
	return errBadRequest("InvalidResourceReference", "Resource %s referenced by resource %s was not found. Please make sure that the referenced resource exists, and that both resources are in the same region.", id, user)
}
