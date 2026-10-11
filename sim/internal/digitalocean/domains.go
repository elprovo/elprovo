package digitalocean

import (
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// defaultTTL is the TTL of a domain, and of a record created without one.
const defaultTTL = 1800

var (
	domainNamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*[a-z0-9]$`)
	recordNamePattern = regexp.MustCompile(`^(@|\*|(\*\.)?[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?(\.[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?)*)$`)
	hostPattern       = regexp.MustCompile(`^(@|([A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?\.)*[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?\.?)$`)
	recordTypes       = []string{"A", "AAAA", "CAA", "CNAME", "MX", "NS", "SOA", "SRV", "TXT"}
	nameServers       = []string{"ns1.digitalocean.com", "ns2.digitalocean.com", "ns3.digitalocean.com"}
)

// domain is a DNS zone that DigitalOcean serves. Creating one adds the
// zone's SOA record and DigitalOcean's three name servers, and an A record
// for the apex when the request names an address. Record names are relative
// to the domain, with @ for the apex.
type domain struct {
	Name    string          `json:"name"`
	Records []*domainRecord `json:"records"`
}

type domainRecord struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	Priority int    `json:"priority"`
	Port     int    `json:"port"`
	TTL      int    `json:"ttl"`
	Weight   int    `json:"weight"`
	Flags    int    `json:"flags"`
	Tag      string `json:"tag"`
}

type domainJSON struct {
	Name     string `json:"name"`
	TTL      int    `json:"ttl"`
	ZoneFile string `json:"zone_file"`
}

// domainRecordJSON leaves out, as null, the fields that the record's type
// does not use.
type domainRecordJSON struct {
	ID       int64   `json:"id"`
	Type     string  `json:"type"`
	Name     string  `json:"name"`
	Data     string  `json:"data"`
	Priority *int    `json:"priority"`
	Port     *int    `json:"port"`
	TTL      int     `json:"ttl"`
	Weight   *int    `json:"weight"`
	Flags    *int    `json:"flags"`
	Tag      *string `json:"tag"`
}

func renderDomain(d *domain) domainJSON {
	var zone strings.Builder
	fmt.Fprintf(&zone, "$ORIGIN %s.\n$TTL %d\n", d.Name, defaultTTL)
	for _, r := range d.Records {
		fmt.Fprintf(&zone, "%s\t%d\tIN\t%s\t%s\n", r.Name, r.TTL, r.Type, r.Data)
	}
	return domainJSON{Name: d.Name, TTL: defaultTTL, ZoneFile: zone.String()}
}

func renderRecord(r *domainRecord) domainRecordJSON {
	out := domainRecordJSON{ID: r.ID, Type: r.Type, Name: r.Name, Data: r.Data, TTL: r.TTL}
	switch r.Type {
	case "MX":
		out.Priority = &r.Priority
	case "SRV":
		out.Priority, out.Port, out.Weight = &r.Priority, &r.Port, &r.Weight
	case "CAA":
		out.Flags, out.Tag = &r.Flags, &r.Tag
	}
	return out
}

func (a *account) domain(name string) *domain {
	for _, d := range a.Domains {
		if d.Name == name {
			return d
		}
	}
	return nil
}

func (d *domain) record(id string) *domainRecord {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil
	}
	for _, r := range d.Records {
		if r.ID == n {
			return r
		}
	}
	return nil
}

// relativeName turns a fully qualified name in the domain, which ends with
// a dot, into the relative name DigitalOcean stores.
func (d *domain) relativeName(name string) string {
	switch {
	case strings.EqualFold(name, d.Name+"."):
		return "@"
	case strings.HasSuffix(strings.ToLower(name), "."+d.Name+"."):
		return name[:len(name)-len(d.Name)-2]
	}
	return name
}

// fqdn returns the record's fully qualified name without the final dot, as
// the name filter of a record listing takes it.
func (d *domain) fqdn(r *domainRecord) string {
	if r.Name == "@" {
		return d.Name
	}
	return strings.ToLower(r.Name) + "." + d.Name
}

func (s *Simulation) listDomains(r *http.Request) (int, any, error) {
	var out []domainJSON
	s.store.Read(func(a *account) {
		for _, d := range a.Domains {
			out = append(out, renderDomain(d))
		}
	})
	return listResponse(r, "domains", out)
}

type createDomainRequest struct {
	Name      string `json:"name"`
	IPAddress string `json:"ip_address"`
}

func (s *Simulation) createDomain(r *http.Request) (int, any, error) {
	var req createDomainRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	name := strings.ToLower(strings.TrimSuffix(req.Name, "."))
	if !domainNamePattern.MatchString(name) {
		return 0, nil, errUnprocessable("Name is not a valid domain name.")
	}
	if req.IPAddress != "" {
		if _, err := netip.ParseAddr(req.IPAddress); err != nil {
			return 0, nil, errUnprocessable("IP address is invalid.")
		}
	}
	var out domainJSON
	err := s.store.Update(func(a *account) error {
		if a.domain(name) != nil {
			return errUnprocessable("Name already exists.")
		}
		d := &domain{Name: name}
		d.Records = append(d.Records, &domainRecord{ID: a.nextID(), Type: "SOA", Name: "@", Data: strconv.Itoa(defaultTTL), TTL: defaultTTL})
		for _, ns := range nameServers {
			d.Records = append(d.Records, &domainRecord{ID: a.nextID(), Type: "NS", Name: "@", Data: ns, TTL: defaultTTL})
		}
		if req.IPAddress != "" {
			typ := "A"
			if netip.MustParseAddr(req.IPAddress).Is6() {
				typ = "AAAA"
			}
			d.Records = append(d.Records, &domainRecord{ID: a.nextID(), Type: typ, Name: "@", Data: req.IPAddress, TTL: defaultTTL})
		}
		a.Domains = append(a.Domains, d)
		out = renderDomain(d)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"domain": out}, nil
}

func (s *Simulation) getDomain(r *http.Request) (int, any, error) {
	var out *domainJSON
	s.store.Read(func(a *account) {
		if d := a.domain(r.PathValue("name")); d != nil {
			rendered := renderDomain(d)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"domain": out}, nil
}

func (s *Simulation) deleteDomain(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		d := a.domain(r.PathValue("name"))
		if d == nil {
			return errNotFound()
		}
		a.Domains = slices.DeleteFunc(a.Domains, func(v *domain) bool { return v == d })
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// listDomainRecords lists the domain's records, filtered by type and by
// fully qualified name when the query names them.
func (s *Simulation) listDomainRecords(r *http.Request) (int, any, error) {
	typ, name := r.URL.Query().Get("type"), strings.ToLower(strings.TrimSuffix(r.URL.Query().Get("name"), "."))
	var out []domainRecordJSON
	found := false
	s.store.Read(func(a *account) {
		d := a.domain(r.PathValue("name"))
		if d == nil {
			return
		}
		found = true
		for _, rec := range d.Records {
			if (typ == "" || rec.Type == typ) && (name == "" || d.fqdn(rec) == name) {
				out = append(out, renderRecord(rec))
			}
		}
	})
	if !found {
		return 0, nil, errNotFound()
	}
	return listResponse(r, "domain_records", out)
}

// recordRequest is the body of a record create or update. A field left out
// of an update keeps its value.
type recordRequest struct {
	Type     *string `json:"type"`
	Name     *string `json:"name"`
	Data     *string `json:"data"`
	Priority *int    `json:"priority"`
	Port     *int    `json:"port"`
	TTL      *int    `json:"ttl"`
	Weight   *int    `json:"weight"`
	Flags    *int    `json:"flags"`
	Tag      *string `json:"tag"`
}

func (req recordRequest) applyTo(d *domain, rec *domainRecord) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setInt := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	set(&rec.Type, req.Type)
	if req.Name != nil {
		rec.Name = d.relativeName(*req.Name)
	}
	set(&rec.Data, req.Data)
	setInt(&rec.Priority, req.Priority)
	setInt(&rec.Port, req.Port)
	if req.TTL != nil && *req.TTL != 0 {
		rec.TTL = *req.TTL
	}
	setInt(&rec.Weight, req.Weight)
	setInt(&rec.Flags, req.Flags)
	set(&rec.Tag, req.Tag)
}

// validateRecord checks a record as DigitalOcean does before it keeps it.
func validateRecord(rec *domainRecord, portGiven bool) error {
	if !slices.Contains(recordTypes, rec.Type) {
		return errUnprocessable("Type must be one of %s.", strings.Join(recordTypes, ", "))
	}
	if !recordNamePattern.MatchString(rec.Name) {
		return errUnprocessable("Name is invalid.")
	}
	if rec.TTL < 30 {
		return errUnprocessable("TTL must be at least 30 seconds.")
	}
	switch rec.Type {
	case "A":
		if addr, err := netip.ParseAddr(rec.Data); err != nil || !addr.Is4() {
			return errUnprocessable("Data needs to be an IPv4 address.")
		}
	case "AAAA":
		if addr, err := netip.ParseAddr(rec.Data); err != nil || !addr.Is6() || addr.Is4In6() {
			return errUnprocessable("Data needs to be an IPv6 address.")
		}
	case "CNAME", "MX", "NS":
		if !hostPattern.MatchString(rec.Data) {
			return errUnprocessable("Data needs to be a hostname.")
		}
	case "SRV":
		if !hostPattern.MatchString(rec.Data) {
			return errUnprocessable("Data needs to be a hostname.")
		}
		if !portGiven || rec.Port < 1 || rec.Port > 65535 {
			return errUnprocessable("Port is required for SRV records.")
		}
	case "CAA":
		if !slices.Contains([]string{"issue", "issuewild", "iodef"}, rec.Tag) {
			return errUnprocessable("Tag must be issue, issuewild or iodef.")
		}
	}
	if rec.Data == "" {
		return errUnprocessable("Data is required.")
	}
	return nil
}

func (s *Simulation) createDomainRecord(r *http.Request) (int, any, error) {
	var req recordRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out domainRecordJSON
	err := s.store.Update(func(a *account) error {
		d := a.domain(r.PathValue("name"))
		if d == nil {
			return errNotFound()
		}
		rec := &domainRecord{TTL: defaultTTL}
		req.applyTo(d, rec)
		if err := validateRecord(rec, req.Port != nil); err != nil {
			return err
		}
		rec.ID = a.nextID()
		d.Records = append(d.Records, rec)
		out = renderRecord(rec)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"domain_record": out}, nil
}

func (s *Simulation) getDomainRecord(r *http.Request) (int, any, error) {
	var out *domainRecordJSON
	s.store.Read(func(a *account) {
		if d := a.domain(r.PathValue("name")); d != nil {
			if rec := d.record(r.PathValue("id")); rec != nil {
				rendered := renderRecord(rec)
				out = &rendered
			}
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"domain_record": out}, nil
}

// updateDomainRecord serves PUT and PATCH, which both change only the fields
// the body names.
func (s *Simulation) updateDomainRecord(r *http.Request) (int, any, error) {
	var req recordRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out domainRecordJSON
	err := s.store.Update(func(a *account) error {
		d := a.domain(r.PathValue("name"))
		if d == nil {
			return errNotFound()
		}
		rec := d.record(r.PathValue("id"))
		if rec == nil {
			return errNotFound()
		}
		changed := *rec
		req.applyTo(d, &changed)
		if err := validateRecord(&changed, req.Port != nil || changed.Port != 0); err != nil {
			return err
		}
		*rec = changed
		out = renderRecord(rec)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"domain_record": out}, nil
}

func (s *Simulation) deleteDomainRecord(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		d := a.domain(r.PathValue("name"))
		if d == nil {
			return errNotFound()
		}
		rec := d.record(r.PathValue("id"))
		if rec == nil {
			return errNotFound()
		}
		d.Records = slices.DeleteFunc(d.Records, func(v *domainRecord) bool { return v == rec })
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}
