package aws

import (
	"net/netip"
	"slices"
)

type internetGateway struct {
	ID     string `json:"id"`
	Region string `json:"region"`
	VPCID  string `json:"vpc_id"`
}

type routeTable struct {
	ID           string             `json:"id"`
	Region       string             `json:"region"`
	VPCID        string             `json:"vpc_id"`
	ClientToken  string             `json:"client_token,omitempty"`
	Routes       []route            `json:"routes"`
	Associations []routeAssociation `json:"associations"`
}

type route struct {
	Destination string `json:"destination"`
	GatewayID   string `json:"gateway_id"`
	Origin      string `json:"origin"`
}

type routeAssociation struct {
	ID       string `json:"id"`
	SubnetID string `json:"subnet_id,omitempty"`
	Main     bool   `json:"main,omitempty"`
}

func (t *routeTable) main() bool {
	return slices.ContainsFunc(t.Associations, func(a routeAssociation) bool { return a.Main })
}

func (w *world) internetGateway(region, id string) *internetGateway {
	return find(w.InternetGateways, func(g *internetGateway) bool { return g.ID == id && g.Region == region })
}

func (w *world) routeTable(region, id string) *routeTable {
	return find(w.RouteTables, func(t *routeTable) bool { return t.ID == id && t.Region == region })
}

func (w *world) mainRouteTable(vpcID string) *routeTable {
	return find(w.RouteTables, func(t *routeTable) bool { return t.VPCID == vpcID && t.main() })
}

func (s *Simulation) ec2RouteActions() map[string]ec2Action {
	return map[string]ec2Action{
		"CreateInternetGateway":    {s.createInternetGateway, []string{"TagSpecification"}},
		"DescribeInternetGateways": {s.describeInternetGateways, []string{"InternetGatewayId", "Filter", "MaxResults", "NextToken"}},
		"AttachInternetGateway":    {s.attachInternetGateway, []string{"InternetGatewayId", "VpcId"}},
		"DetachInternetGateway":    {s.detachInternetGateway, []string{"InternetGatewayId", "VpcId"}},
		"DeleteInternetGateway":    {s.deleteInternetGateway, []string{"InternetGatewayId"}},
		"CreateRouteTable":         {s.createRouteTable, []string{"VpcId", "TagSpecification", "ClientToken"}},
		"DescribeRouteTables":      {s.describeRouteTables, []string{"RouteTableId", "Filter", "MaxResults", "NextToken"}},
		"DeleteRouteTable":         {s.deleteRouteTable, []string{"RouteTableId"}},
		"CreateRoute":              {s.createRoute, []string{"RouteTableId", "DestinationCidrBlock", "GatewayId"}},
		"ReplaceRoute":             {s.replaceRoute, []string{"RouteTableId", "DestinationCidrBlock", "GatewayId"}},
		"DeleteRoute":              {s.deleteRoute, []string{"RouteTableId", "DestinationCidrBlock"}},
		"AssociateRouteTable":      {s.associateRouteTable, []string{"RouteTableId", "SubnetId"}},
		"DisassociateRouteTable":   {s.disassociateRouteTable, []string{"AssociationId"}},
	}
}

type attachmentXML struct {
	State string `xml:"state"`
	VPCID string `xml:"vpcId"`
}

type internetGatewayXML struct {
	InternetGatewayID string          `xml:"internetGatewayId"`
	OwnerID           string          `xml:"ownerId"`
	Attachments       []attachmentXML `xml:"attachmentSet>item"`
	TagSet            []tag           `xml:"tagSet>item"`
}

func renderInternetGateway(w *world, g *internetGateway) internetGatewayXML {
	out := internetGatewayXML{InternetGatewayID: g.ID, OwnerID: w.AccountID, TagSet: w.tagsOf(g.ID)}
	if g.VPCID != "" {
		out.Attachments = []attachmentXML{{"available", g.VPCID}}
	}
	return out
}

type createInternetGatewayResponse struct {
	ec2Meta
	InternetGateway internetGatewayXML `xml:"internetGateway"`
}

func (s *Simulation) createInternetGateway(c *ec2Call) (ec2Response, error) {
	specs, err := tagSpecifications(c.params, "internet-gateway")
	if err != nil {
		return nil, err
	}
	out := &createInternetGatewayResponse{}
	err = s.store.Update(func(w *world) error {
		g := &internetGateway{ID: newID("igw"), Region: c.region}
		w.InternetGateways = append(w.InternetGateways, g)
		for _, t := range specs["internet-gateway"] {
			w.setTag(g.ID, t)
		}
		out.InternetGateway = renderInternetGateway(w, g)
		return nil
	})
	return out, err
}

type describeInternetGatewaysResponse struct {
	ec2Meta
	InternetGateways []internetGatewayXML `xml:"internetGatewaySet>item"`
}

func (s *Simulation) describeInternetGateways(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeInternetGatewaysResponse{}
	var err error
	s.store.Read(func(w *world) {
		var gateways []*internetGateway
		gateways, err = selectIDs(regional(w.InternetGateways, c.region), func(g *internetGateway) string { return g.ID }, c.params.list("InternetGatewayId"), notFoundByID)
		for _, g := range gateways {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "internet-gateway-id":
					return values(g.ID), true
				case "attachment.vpc-id":
					return values(g.VPCID), true
				case "attachment.state":
					if g.VPCID == "" {
						return nil, true
					}
					return values("available"), true
				case "owner-id":
					return values(w.AccountID), true
				}
				return nil, false
			}, w.tagsOf(g.ID))
			if match {
				out.InternetGateways = append(out.InternetGateways, renderInternetGateway(w, g))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) attachInternetGateway(c *ec2Call) (ec2Response, error) {
	id, vpcID := c.params.get("InternetGatewayId"), c.params.get("VpcId")
	err := s.store.Update(func(w *world) error {
		g := w.internetGateway(c.region, id)
		if g == nil {
			return notFoundByID(id)
		}
		if w.vpc(c.region, vpcID) == nil {
			return notFoundByID(vpcID)
		}
		if g.VPCID != "" {
			return clientError("Resource.AlreadyAssociated", "resource %s is already attached to network %s", id, g.VPCID)
		}
		if slices.ContainsFunc(w.InternetGateways, func(o *internetGateway) bool { return o.VPCID == vpcID }) {
			return clientError("Resource.AlreadyAssociated", "network %s already has an internet gateway attached", vpcID)
		}
		g.VPCID = vpcID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

func (s *Simulation) detachInternetGateway(c *ec2Call) (ec2Response, error) {
	id, vpcID := c.params.get("InternetGatewayId"), c.params.get("VpcId")
	err := s.store.Update(func(w *world) error {
		g := w.internetGateway(c.region, id)
		if g == nil {
			return notFoundByID(id)
		}
		if g.VPCID != vpcID {
			return clientError("Gateway.NotAttached", "resource %s is not attached to network %s", id, vpcID)
		}
		if slices.ContainsFunc(w.Addresses, func(a *address) bool { return a.InstanceID != "" && w.instanceVPC(a.InstanceID) == vpcID }) {
			return clientError("DependencyViolation", "Network %s has some mapped public address(es). Please unmap those public address(es) before detaching the gateway.", vpcID)
		}
		g.VPCID = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

func (w *world) instanceVPC(id string) string {
	if i := w.instance(id); i != nil {
		return i.VPCID
	}
	return ""
}

func (s *Simulation) deleteInternetGateway(c *ec2Call) (ec2Response, error) {
	id := c.params.get("InternetGatewayId")
	err := s.store.Update(func(w *world) error {
		g := w.internetGateway(c.region, id)
		if g == nil {
			return notFoundByID(id)
		}
		if g.VPCID != "" {
			return clientError("DependencyViolation", "The internetGateway '%s' has dependencies and cannot be deleted.", id)
		}
		w.InternetGateways = slices.DeleteFunc(w.InternetGateways, func(x *internetGateway) bool { return x == g })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

type routeXML struct {
	DestinationCIDRBlock string `xml:"destinationCidrBlock"`
	GatewayID            string `xml:"gatewayId"`
	State                string `xml:"state"`
	Origin               string `xml:"origin"`
}

type routeAssociationXML struct {
	RouteTableAssociationID string     `xml:"routeTableAssociationId"`
	RouteTableID            string     `xml:"routeTableId"`
	SubnetID                string     `xml:"subnetId,omitempty"`
	Main                    bool       `xml:"main"`
	AssociationState        stateValue `xml:"associationState"`
}

type routeTableXML struct {
	RouteTableID string                `xml:"routeTableId"`
	VPCID        string                `xml:"vpcId"`
	OwnerID      string                `xml:"ownerId"`
	Routes       []routeXML            `xml:"routeSet>item"`
	Associations []routeAssociationXML `xml:"associationSet>item"`
	TagSet       []tag                 `xml:"tagSet>item"`
}

func renderRouteTable(w *world, t *routeTable) routeTableXML {
	out := routeTableXML{RouteTableID: t.ID, VPCID: t.VPCID, OwnerID: w.AccountID, TagSet: w.tagsOf(t.ID)}
	for _, r := range t.Routes {
		state := "active"
		if r.GatewayID != "local" && w.internetGateway(t.Region, r.GatewayID) == nil {
			state = "blackhole"
		}
		out.Routes = append(out.Routes, routeXML{r.Destination, r.GatewayID, state, r.Origin})
	}
	for _, a := range t.Associations {
		out.Associations = append(out.Associations, routeAssociationXML{a.ID, t.ID, a.SubnetID, a.Main, stateValue{"associated"}})
	}
	return out
}

type createRouteTableResponse struct {
	ec2Meta
	RouteTable routeTableXML `xml:"routeTable"`
}

func (s *Simulation) createRouteTable(c *ec2Call) (ec2Response, error) {
	vpcID := c.params.get("VpcId")
	specs, err := tagSpecifications(c.params, "route-table")
	if err != nil {
		return nil, err
	}
	out := &createRouteTableResponse{}
	token := c.params.get("ClientToken")
	err = s.store.Update(func(w *world) error {
		if t := find(w.RouteTables, func(t *routeTable) bool { return token != "" && t.Region == c.region && t.ClientToken == token }); t != nil {
			out.RouteTable = renderRouteTable(w, t)
			return nil
		}
		v := w.vpc(c.region, vpcID)
		if v == nil {
			return notFoundByID(vpcID)
		}
		t := &routeTable{ID: newID("rtb"), Region: c.region, VPCID: vpcID, ClientToken: token, Routes: []route{{Destination: v.CIDR, GatewayID: "local", Origin: "CreateRouteTable"}}}
		w.RouteTables = append(w.RouteTables, t)
		for _, tg := range specs["route-table"] {
			w.setTag(t.ID, tg)
		}
		out.RouteTable = renderRouteTable(w, t)
		return nil
	})
	return out, err
}

type describeRouteTablesResponse struct {
	ec2Meta
	RouteTables []routeTableXML `xml:"routeTableSet>item"`
}

func (s *Simulation) describeRouteTables(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeRouteTablesResponse{}
	var err error
	s.store.Read(func(w *world) {
		var tables []*routeTable
		tables, err = selectIDs(regional(w.RouteTables, c.region), func(t *routeTable) string { return t.ID }, c.params.list("RouteTableId"), notFoundByID)
		for _, t := range tables {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				var out []string
				switch name {
				case "route-table-id":
					return values(t.ID), true
				case "vpc-id":
					return values(t.VPCID), true
				case "owner-id":
					return values(w.AccountID), true
				case "association.main":
					return boolString(t.main()), true
				case "association.subnet-id":
					for _, a := range t.Associations {
						out = append(out, a.SubnetID)
					}
					return out, true
				case "association.route-table-association-id":
					for _, a := range t.Associations {
						out = append(out, a.ID)
					}
					return out, true
				case "route.gateway-id":
					for _, r := range t.Routes {
						out = append(out, r.GatewayID)
					}
					return out, true
				case "route.destination-cidr-block":
					for _, r := range t.Routes {
						out = append(out, r.Destination)
					}
					return out, true
				}
				return nil, false
			}, w.tagsOf(t.ID))
			if match {
				out.RouteTables = append(out.RouteTables, renderRouteTable(w, t))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) deleteRouteTable(c *ec2Call) (ec2Response, error) {
	id := c.params.get("RouteTableId")
	err := s.store.Update(func(w *world) error {
		t := w.routeTable(c.region, id)
		if t == nil {
			return notFoundByID(id)
		}
		if len(t.Associations) > 0 {
			return clientError("DependencyViolation", "The routeTable '%s' has dependencies and cannot be deleted.", id)
		}
		w.RouteTables = slices.DeleteFunc(w.RouteTables, func(x *routeTable) bool { return x == t })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

// routeRequest reads and checks a route's table, destination and target.
func routeRequest(w *world, c *ec2Call) (*routeTable, string, error) {
	id, dest := c.params.get("RouteTableId"), c.params.get("DestinationCidrBlock")
	t := w.routeTable(c.region, id)
	if t == nil {
		return nil, "", notFoundByID(id)
	}
	prefix, err := netip.ParsePrefix(dest)
	if err != nil || !prefix.Addr().Is4() {
		return nil, "", errInvalidParameter("Value (%s) for parameter destinationCidrBlock is invalid. This is not a valid CIDR block.", dest)
	}
	return t, prefix.Masked().String(), nil
}

func checkGateway(w *world, c *ec2Call, t *routeTable) error {
	gw := c.params.get("GatewayId")
	if gw == "" {
		return errUnsupported("Route targets other than an internet gateway")
	}
	g := w.internetGateway(c.region, gw)
	if g == nil {
		return notFoundByID(gw)
	}
	if g.VPCID != t.VPCID {
		return errInvalidParameter("route table %s and network gateway %s belong to different networks", t.ID, gw)
	}
	return nil
}

func (s *Simulation) createRoute(c *ec2Call) (ec2Response, error) {
	err := s.store.Update(func(w *world) error {
		t, dest, err := routeRequest(w, c)
		if err != nil {
			return err
		}
		if err := checkGateway(w, c, t); err != nil {
			return err
		}
		if slices.ContainsFunc(t.Routes, func(r route) bool { return r.Destination == dest }) {
			return clientError("RouteAlreadyExists", "The route identified by %s already exists.", dest)
		}
		t.Routes = append(t.Routes, route{Destination: dest, GatewayID: c.params.get("GatewayId"), Origin: "CreateRoute"})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

func (s *Simulation) replaceRoute(c *ec2Call) (ec2Response, error) {
	err := s.store.Update(func(w *world) error {
		t, dest, err := routeRequest(w, c)
		if err != nil {
			return err
		}
		if err := checkGateway(w, c, t); err != nil {
			return err
		}
		i := slices.IndexFunc(t.Routes, func(r route) bool { return r.Destination == dest && r.GatewayID != "local" })
		if i < 0 {
			return clientError("InvalidRoute.NotFound", "There is no route defined for '%s' in the route table.", dest)
		}
		t.Routes[i].GatewayID = c.params.get("GatewayId")
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

func (s *Simulation) deleteRoute(c *ec2Call) (ec2Response, error) {
	err := s.store.Update(func(w *world) error {
		t, dest, err := routeRequest(w, c)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(t.Routes, func(r route) bool { return r.Destination == dest })
		switch {
		case i < 0:
			return clientError("InvalidRoute.NotFound", "There is no route defined for '%s' in the route table.", dest)
		case t.Routes[i].GatewayID == "local":
			return clientError("InvalidParameterValue", "cannot remove local route %s in route table %s", dest, t.ID)
		}
		t.Routes = slices.Delete(t.Routes, i, i+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

type associateRouteTableResponse struct {
	ec2Meta
	AssociationID    string     `xml:"associationId"`
	AssociationState stateValue `xml:"associationState"`
}

func (s *Simulation) associateRouteTable(c *ec2Call) (ec2Response, error) {
	id, subnetID := c.params.get("RouteTableId"), c.params.get("SubnetId")
	out := &associateRouteTableResponse{AssociationState: stateValue{"associated"}}
	err := s.store.Update(func(w *world) error {
		t := w.routeTable(c.region, id)
		if t == nil {
			return notFoundByID(id)
		}
		sn := w.subnet(c.region, subnetID)
		if sn == nil {
			return notFoundByID(subnetID)
		}
		if sn.VPCID != t.VPCID {
			return errInvalidParameter("Route table %s and subnet %s belong to different networks", id, subnetID)
		}
		for _, other := range w.RouteTables {
			if slices.ContainsFunc(other.Associations, func(a routeAssociation) bool { return a.SubnetID == subnetID }) {
				return clientError("Resource.AlreadyAssociated", "the specified association for route table %s conflicts with an existing association", id)
			}
		}
		out.AssociationID = newID("rtbassoc")
		t.Associations = append(t.Associations, routeAssociation{ID: out.AssociationID, SubnetID: subnetID})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) disassociateRouteTable(c *ec2Call) (ec2Response, error) {
	id := c.params.get("AssociationId")
	err := s.store.Update(func(w *world) error {
		for _, t := range regional(w.RouteTables, c.region) {
			i := slices.IndexFunc(t.Associations, func(a routeAssociation) bool { return a.ID == id })
			if i < 0 {
				continue
			}
			if t.Associations[i].Main {
				return errInvalidParameter("cannot disassociate the main route table association %s", id)
			}
			t.Associations = slices.Delete(t.Associations, i, i+1)
			return nil
		}
		return clientError("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
	})
	if err != nil {
		return nil, err
	}
	return ok()
}
