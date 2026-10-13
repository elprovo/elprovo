package hetzner

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

// serverActionCommands maps each supported server action to the command
// its action records.
var serverActionCommands = map[string]string{
	"poweron": "start_server", "poweroff": "stop_server", "shutdown": "shutdown_server", "reboot": "reboot_server",
	"reset": "reset_server", "create_image": "create_image", "attach_to_network": "attach_to_network",
	"detach_from_network": "detach_from_network",
}

type serverActionRequest struct {
	Description *string           `json:"description"`
	Type        string            `json:"type"`
	Labels      map[string]string `json:"labels"`
	Network     int64             `json:"network"`
	IP          string            `json:"ip"`
	AliasIPs    []string          `json:"alias_ips"`
	IPRange     string            `json:"ip_range"`
}

func (s *Simulation) postServerAction(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "server")
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("action")
	command, ok := serverActionCommands[name]
	if !ok {
		return 0, nil, errUnsupported("server action %q is not supported by the simulation", name)
	}
	var req serverActionRequest
	if r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil {
			return 0, nil, err
		}
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	body := map[string]any{}
	var op func(context.Context)
	err = s.store.Update(func(p *project) error {
		srv := p.server(id)
		if srv == nil {
			return errNotFound("server")
		}
		ref := resourceRef{ID: id, Type: "server"}
		if p.running(ref) {
			return errLocked("server")
		}
		if srv.Machine == "" {
			return errService("server %d has no running machine; delete it and create a new one", id)
		}
		switch name {
		case "attach_to_network":
			return s.attachToNetwork(p, srv, req, body)
		case "detach_from_network":
			return s.detachFromNetwork(p, srv, req, body)
		case "create_image":
			var err error
			op, err = s.startSnapshot(p, srv, req, body)
			return err
		}
		a := s.startAction(p, command, ref)
		body["action"] = a.render()
		op = s.powerOperation(srv, name, a.ID)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if op != nil {
		s.background(op)
	}
	return http.StatusCreated, body, nil
}

// powerOperation changes the server's status for a power action and returns
// the work that drives its machine.
func (s *Simulation) powerOperation(srv *server, name string, actionID int64) func(context.Context) {
	serverID, machine, status := srv.ID, srv.Machine, srv.Status
	switch name {
	case "poweron":
		srv.Status = "starting"
	case "poweroff", "shutdown":
		srv.Status = "stopping"
	}
	return func(ctx context.Context) {
		var m machines.Machine
		var err error
		switch name {
		case "poweroff", "shutdown":
			err = s.machines.Stop(ctx, machine, name == "shutdown")
		case "poweron":
			m, err = s.machines.Start(ctx, machine)
		default:
			m, err = s.machines.Restart(ctx, machine, name == "reboot")
		}
		s.firewalls.forget(serverID)
		if err != nil {
			s.log.Error("server power action", "server", serverID, "action", name, "error", err)
			observed, inspectErr := s.machines.Inspect(ctx, machine)
			s.finishAction(actionID, &actionError{Code: "action_failed", Message: "the server's machine did not " + name}, func(p *project) {
				if srv := p.server(serverID); srv != nil {
					srv.Status = status
					if inspectErr == nil {
						srv.Status = statusOf(observed)
					}
				}
			})
			return
		}
		s.finishAction(actionID, nil, func(p *project) {
			if srv := p.server(serverID); srv != nil {
				srv.Status = "running"
				if name == "poweroff" || name == "shutdown" {
					srv.Status = "off"
				} else {
					srv.PrivateIP = m.PrivateIP
				}
			}
		})
	}
}

func statusOf(m machines.Machine) string {
	if m.Running {
		return "running"
	}
	return "off"
}

// startSnapshot creates a snapshot image in the creating state and returns
// the work that commits the server's machine to it.
func (s *Simulation) startSnapshot(p *project, srv *server, req serverActionRequest, body map[string]any) (func(context.Context), error) {
	if req.Type != "" && req.Type != "snapshot" {
		return nil, errUnsupported("image type %q is not supported by the simulation; create a snapshot", req.Type)
	}
	t, _ := findServerType(srv.ServerType)
	id := p.nextID()
	description := fmt.Sprintf("%s-snapshot-%d", srv.Name, id)
	if req.Description != nil {
		description = *req.Description
	}
	img := &image{
		ID: id, Description: description, Status: "creating", Labels: req.Labels, Created: s.now().UTC(), DiskSize: t.disk,
		ServerID: srv.ID, ServerName: srv.Name, Architecture: t.architecture, Ref: snapshotRef(p.UUID, id),
	}
	p.Images = append(p.Images, img)
	a := s.startAction(p, "create_image", resourceRef{ID: srv.ID, Type: "server"}, resourceRef{ID: id, Type: "image"})
	body["image"], body["action"] = img.render(), a.render()
	machine, imageID, ref, actionID := srv.Machine, img.ID, img.Ref, a.ID
	return func(ctx context.Context) { s.takeSnapshot(ctx, machine, imageID, ref, actionID) }, nil
}

func (s *Simulation) takeSnapshot(ctx context.Context, machine string, imageID int64, ref string, actionID int64) {
	size, err := s.machines.Commit(ctx, machine, ref)
	if err != nil {
		s.log.Error("snapshot server", "image", imageID, "error", err)
		s.finishAction(actionID, &actionError{Code: "image_failed", Message: "the server's machine could not be committed"}, func(p *project) {
			p.Images = slices.DeleteFunc(p.Images, func(img *image) bool { return img.ID == imageID })
		})
		return
	}
	s.finishAction(actionID, nil, func(p *project) {
		if img := p.image(imageID); img != nil {
			img.Status, img.ImageSize = "available", math.Round(float64(size)/1e7)/100
		}
	})
}

func (s *Simulation) attachToNetwork(p *project, srv *server, req serverActionRequest, body map[string]any) error {
	n := p.network(req.Network)
	loc, _ := location(srv.Location)
	switch {
	case n == nil:
		return errNotFound("network")
	case slices.Contains(srv.Networks, n.ID):
		return errCode(http.StatusUnprocessableEntity, "server_already_attached", "server %d is already attached to network %d", srv.ID, n.ID)
	case len(srv.Networks) > 0:
		return errUnsupported("the simulation attaches a server to one network at most")
	case len(req.AliasIPs) > 0:
		return errUnsupported("alias IPs are not supported by the simulation")
	case req.IP != "" && req.IP != srv.PrivateIP:
		return errUnsupported("the simulation gives a server its machine's private address %s; omit ip", srv.PrivateIP)
	case req.IPRange != "" && !n.hasSubnet(req.IPRange):
		return errInvalidInput("ip_range", "network %d has no subnet %s", n.ID, req.IPRange)
	case !n.hasZone(loc.NetworkZone):
		return errCode(http.StatusUnprocessableEntity, "no_subnet_available", "network %d has no subnet in network zone %s", n.ID, loc.NetworkZone)
	}
	srv.Networks = append(srv.Networks, n.ID)
	a := s.succeeded(p, "attach_to_network", resourceRef{ID: srv.ID, Type: "server"}, resourceRef{ID: n.ID, Type: "network"})
	body["action"] = a.render()
	s.firewalls.kick()
	return nil
}

func (s *Simulation) detachFromNetwork(p *project, srv *server, req serverActionRequest, body map[string]any) error {
	if p.network(req.Network) == nil {
		return errNotFound("network")
	}
	if !slices.Contains(srv.Networks, req.Network) {
		return errCode(http.StatusUnprocessableEntity, "server_not_attached_to_network", "server %d is not attached to network %d", srv.ID, req.Network)
	}
	srv.Networks = slices.DeleteFunc(srv.Networks, func(id int64) bool { return id == req.Network })
	a := s.succeeded(p, "detach_from_network", resourceRef{ID: srv.ID, Type: "server"}, resourceRef{ID: req.Network, Type: "network"})
	body["action"] = a.render()
	s.firewalls.kick()
	return nil
}
