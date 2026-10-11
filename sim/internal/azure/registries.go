package azure

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var registryNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]{5,50}$`)

// registry is a container registry's management record. The registry
// itself is a CNCF Distribution registry that the operator runs and names
// <registry name>.<-azure-registry-domain> on the simulated network.
type registry struct {
	meta
	SKU        string             `json:"sku"`
	Properties registryProperties `json:"properties"`
	CreatedAt  time.Time          `json:"created_at"`
	Tokens     []*registryToken   `json:"tokens,omitempty"`
}

type registryProperties struct {
	AdminUserEnabled                 *bool           `json:"adminUserEnabled"`
	PublicNetworkAccess              string          `json:"publicNetworkAccess,omitempty"`
	ZoneRedundancy                   string          `json:"zoneRedundancy,omitempty"`
	AnonymousPullEnabled             bool            `json:"anonymousPullEnabled"`
	DataEndpointEnabled              bool            `json:"dataEndpointEnabled"`
	NetworkRuleBypassOptions         string          `json:"networkRuleBypassOptions,omitempty"`
	RoleAssignmentMode               string          `json:"roleAssignmentMode,omitempty"`
	NetworkRuleBypassAllowedForTasks bool            `json:"networkRuleBypassAllowedForTasks"`
	Policies                         json.RawMessage `json:"policies,omitempty"`
	Encryption                       json.RawMessage `json:"encryption,omitempty"`
	NetworkRuleSet                   json.RawMessage `json:"networkRuleSet,omitempty"`
	LoginServer                      string          `json:"loginServer,omitempty"`
	CreationDate                     string          `json:"creationDate,omitempty"`
	ProvisioningState                string          `json:"provisioningState,omitempty"`
	DataEndpointHostNames            []string        `json:"dataEndpointHostNames,omitempty"`
	PrivateEndpointConnections       []any           `json:"privateEndpointConnections,omitempty"`
}

func (s *Simulation) loginServer(name string) string {
	return strings.ToLower(name) + "." + s.cfg.registry.domain
}

func (s *Simulation) renderRegistry(reg *registry) resourceJSON {
	props := reg.Properties
	props.LoginServer, props.CreationDate, props.ProvisioningState = s.loginServer(reg.Name), timestamp(reg.CreatedAt), statusSucceeded
	props.DataEndpointHostNames, props.PrivateEndpointConnections = []string{}, []any{}
	return resourceJSON{
		ID: s.idFor(typeRegistry, &reg.meta), Name: reg.Name, Type: typeRegistry, Location: reg.Location, Tags: cloneTags(reg.Tags),
		SKU: sku{Name: reg.SKU, Tier: reg.SKU}, Properties: props,
	}
}

func (s *Simulation) putRegistry(r *http.Request, p params) (reply, error) {
	var req resourceRequest[registryProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	props := req.Properties
	tier := ""
	if req.SKU != nil {
		tier = req.SKU.Name
	}
	var ok bool
	switch {
	case !registryNamePattern.MatchString(p["name"]):
		return reply{}, errBadRequest("RegistryNameInvalid", "The specified resource name '%s' is invalid. Registry names may contain only alpha numeric characters and must be between 5 and 50 characters.", p["name"])
	case len(req.Identity) > 0 && !strings.Contains(string(req.Identity), `"None"`) && string(req.Identity) != "null":
		return reply{}, errUnsupported("A managed identity on a container registry")
	case len(props.NetworkRuleSet) > 0 && string(props.NetworkRuleSet) != "null":
		return reply{}, errUnsupported("A network rule set on a container registry")
	case props.DataEndpointEnabled:
		return reply{}, errUnsupported("Dedicated data endpoints")
	}
	if tier, ok = canonical(tier, "Basic", "Standard", "Premium"); !ok {
		return reply{}, errBadRequest("InvalidSku", "The SKU '%s' is invalid. Use Basic, Standard or Premium.", tier)
	}
	if props.AdminUserEnabled == nil {
		disabled := false
		props.AdminUserEnabled = &disabled
	}
	defaults := map[*string]string{
		&props.PublicNetworkAccess: "Enabled", &props.ZoneRedundancy: "Disabled", &props.NetworkRuleBypassOptions: "AzureServices",
		&props.RoleAssignmentMode: "LegacyRegistryPermissions",
	}
	for field, value := range defaults {
		if *field == "" {
			*field = value
		}
	}
	if len(props.Policies) == 0 || string(props.Policies) == "null" {
		props.Policies = json.RawMessage(`{"quarantinePolicy":{"status":"disabled"},"trustPolicy":{"type":"Notary","status":"disabled"},"retentionPolicy":{"days":7,"status":"disabled"},"exportPolicy":{"status":"enabled"},"azureADAuthenticationAsArmPolicy":{"status":"enabled"}}`)
	}
	if len(props.Encryption) == 0 || string(props.Encryption) == "null" {
		props.Encryption = json.RawMessage(`{"status":"disabled"}`)
	}
	props.LoginServer, props.CreationDate, props.ProvisioningState, props.NetworkRuleSet = "", "", "", nil
	var out resourceJSON
	var op *operation
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		reg := lookup(sub.Registries, g.Name, p["name"])
		if reg == nil {
			for _, other := range sub.Registries {
				if strings.EqualFold(other.Name, p["name"]) {
					return errConflict("AlreadyInUse", "The registry DNS name %s is already in use.", s.loginServer(p["name"]))
				}
			}
			reg = &registry{meta: meta{Group: g.Name, Name: p["name"], GUID: newUUID()}, CreatedAt: s.now().UTC()}
			sub.Registries = append(sub.Registries, reg)
			status = http.StatusCreated
		} else if reg.Location != location {
			return errBadRequest("InvalidResourceLocation", "The resource '%s' already exists in location '%s'.", reg.Name, reg.Location)
		}
		reg.Location, reg.Tags, reg.SKU, reg.Properties = location, cloneTags(req.Tags), tier, props
		op = s.doneOperation(sub, "Microsoft.ContainerRegistry", location)
		out = s.renderRegistry(reg)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// patchRegistry changes the registry's tags, admin user and policies, as the
// Terraform provider's update does.
func (s *Simulation) patchRegistry(r *http.Request, p params) (reply, error) {
	var req struct {
		Tags       map[string]string   `json:"tags"`
		SKU        *sku                `json:"sku"`
		Properties *registryProperties `json:"properties"`
	}
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	var out resourceJSON
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if reg == nil {
			return errNotFound(typeRegistry, p["name"], p["group"])
		}
		if req.Tags != nil {
			reg.Tags = cloneTags(req.Tags)
		}
		if req.SKU != nil {
			tier, ok := canonical(req.SKU.Name, "Basic", "Standard", "Premium")
			if !ok {
				return errBadRequest("InvalidSku", "The SKU '%s' is invalid. Use Basic, Standard or Premium.", req.SKU.Name)
			}
			reg.SKU = tier
		}
		if q := req.Properties; q != nil {
			if q.AdminUserEnabled != nil {
				reg.Properties.AdminUserEnabled = q.AdminUserEnabled
			}
			if len(q.Policies) > 0 && string(q.Policies) != "null" {
				reg.Properties.Policies = q.Policies
			}
			reg.Properties.AnonymousPullEnabled = q.AnonymousPullEnabled
		}
		op = s.doneOperation(sub, "Microsoft.ContainerRegistry", reg.Location)
		out = s.renderRegistry(reg)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: http.StatusOK, body: out, headers: s.asyncHeaders(r, op)}, nil
}

func (s *Simulation) getRegistry(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if reg := lookup(sub.Registries, p["group"], p["name"]); reg != nil {
			j := s.renderRegistry(reg)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeRegistry, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listRegistryReplications(r *http.Request, p params) (reply, error) {
	if _, err := s.getRegistry(r, p); err != nil {
		return reply{}, err
	}
	return listReply(r, []any{})
}

// listRegistryCredentials returns the admin user's credentials: the
// registry's name and the password of -azure-registry-password, twice.
func (s *Simulation) listRegistryCredentials(_ *http.Request, p params) (reply, error) {
	var name string
	var enabled, found bool
	s.store.Read(func(sub *subscription) {
		if reg := lookup(sub.Registries, p["group"], p["name"]); reg != nil {
			name, found = reg.Name, true
			enabled = reg.Properties.AdminUserEnabled != nil && *reg.Properties.AdminUserEnabled
		}
	})
	switch {
	case !found:
		return reply{}, errNotFound(typeRegistry, p["name"], p["group"])
	case !enabled:
		return reply{}, errBadRequest("UnAuthorizedForCredentialOperations", "Cannot perform credential operations for %s as admin user is disabled. Kindly enable admin user as per docs: https://docs.microsoft.com/en-us/azure/container-registry/container-registry-authentication#admin-account", s.resourceID(p["group"], typeRegistry, name))
	}
	return okReply(map[string]any{
		"username": name,
		"passwords": []map[string]string{
			{"name": "password", "value": s.cfg.registry.password}, {"name": "password2", "value": s.cfg.registry.password},
		},
	}), nil
}

func (s *Simulation) checkRegistryName(r *http.Request, _ params) (reply, error) {
	var req struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	if !registryNamePattern.MatchString(req.Name) {
		return okReply(map[string]any{"nameAvailable": false, "reason": "Invalid", "message": "The specified resource name is invalid."}), nil
	}
	taken := false
	s.store.Read(func(sub *subscription) {
		for _, reg := range sub.Registries {
			taken = taken || strings.EqualFold(reg.Name, req.Name)
		}
	})
	if taken {
		return okReply(map[string]any{"nameAvailable": false, "reason": "AlreadyExists", "message": "The registry " + req.Name + " is already in use."}), nil
	}
	return okReply(map[string]any{"nameAvailable": true}), nil
}

func (s *Simulation) deleteRegistry(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		reg := lookup(sub.Registries, p["group"], p["name"])
		if reg == nil {
			return nil
		}
		sub.Registries = without(sub.Registries, reg.Group, reg.Name)
		op = s.doneOperation(sub, "Microsoft.ContainerRegistry", reg.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}
