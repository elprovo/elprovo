package digitalocean

import (
	"net/http"
	"strconv"
	"testing"
)

type domainRecordResponse struct {
	DomainRecord domainRecordJSON `json:"domain_record"`
}

type domainRecordsResponse struct {
	DomainRecords []domainRecordJSON `json:"domain_records"`
	Meta          meta               `json:"meta"`
}

func (e *testEnv) createDomain(name, ip string) {
	e.t.Helper()
	body := map[string]any{"name": name}
	if ip != "" {
		body["ip_address"] = ip
	}
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/domains", body, nil)
}

func TestANewDomainHasItsSOANameServersAndApexRecord(t *testing.T) {
	e := newEnv(t)

	e.createDomain("acme.dev", "203.0.113.7")

	var got struct {
		Domain domainJSON `json:"domain"`
	}
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/domains/acme.dev", nil, &got)
	if got.Domain.Name != "acme.dev" || got.Domain.TTL != 1800 {
		t.Fatalf("domain = %+v, want acme.dev with TTL 1800", got.Domain)
	}
	var records domainRecordsResponse
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/domains/acme.dev/records", nil, &records)
	var kinds []string
	for _, r := range records.DomainRecords {
		kinds = append(kinds, r.Type+" "+r.Name+" "+r.Data)
	}
	want := []string{"SOA @ 1800", "NS @ ns1.digitalocean.com", "NS @ ns2.digitalocean.com", "NS @ ns3.digitalocean.com", "A @ 203.0.113.7"}
	if records.Meta.Total != len(want) || len(kinds) != len(want) {
		t.Fatalf("records = %q, want %q", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("records = %q, want %q", kinds, want)
		}
	}
}

// TestRecordLifecycleAsTheTerraformProviderDrivesIt follows the calls of the
// digitalocean_record resource: create with every numeric field, read by id,
// update with a PUT and delete.
func TestRecordLifecycleAsTheTerraformProviderDrivesIt(t *testing.T) {
	e := newEnv(t)
	e.createDomain("acme.dev", "")

	var created domainRecordResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/domains/acme.dev/records", map[string]any{
		"type": "A", "name": "*.stagelets", "data": "203.0.113.7", "priority": 0, "port": 0, "ttl": 300, "weight": 0, "flags": 0,
	}, &created)
	rec := created.DomainRecord
	if rec.ID == 0 || rec.Name != "*.stagelets" || rec.Data != "203.0.113.7" || rec.TTL != 300 || rec.Priority != nil || rec.Port != nil {
		t.Fatalf("created = %+v, want the A record with an id, its relative name, TTL 300 and no priority or port", rec)
	}
	path := "/v2/domains/acme.dev/records/" + strconv.FormatInt(rec.ID, 10)

	var read domainRecordResponse
	e.mustCall(http.StatusOK, http.MethodGet, path, nil, &read)
	if read.DomainRecord != rec {
		t.Fatalf("read = %+v, want %+v", read.DomainRecord, rec)
	}
	var byName domainRecordsResponse
	e.mustCall(http.StatusOK, http.MethodGet, "/v2/domains/acme.dev/records?type=A&name=*.stagelets.acme.dev", nil, &byName)
	if len(byName.DomainRecords) != 1 || byName.DomainRecords[0].ID != rec.ID {
		t.Fatalf("records named *.stagelets.acme.dev = %+v, want the new record", byName.DomainRecords)
	}

	var updated domainRecordResponse
	e.mustCall(http.StatusOK, http.MethodPut, path, map[string]any{
		"type": "A", "name": "*.stagelets", "data": "203.0.113.8", "priority": 0, "port": 0, "ttl": 300, "weight": 0, "flags": 0,
	}, &updated)
	if updated.DomainRecord.Data != "203.0.113.8" || updated.DomainRecord.ID != rec.ID {
		t.Fatalf("updated = %+v, want the same record pointing at 203.0.113.8", updated.DomainRecord)
	}

	e.mustCall(http.StatusNoContent, http.MethodDelete, path, nil, nil)
	if status := e.call(http.MethodGet, path, nil, nil); status != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404, which the provider reads as gone", status)
	}
}

func TestRecordsAreCheckedAndNamedRelativeToTheirDomain(t *testing.T) {
	e := newEnv(t)
	e.createDomain("acme.dev", "")

	cases := []struct {
		name   string
		path   string
		body   map[string]any
		status int
	}{
		{"a record in a missing domain", "/v2/domains/other.dev/records", map[string]any{"type": "A", "name": "www", "data": "203.0.113.7"}, http.StatusNotFound},
		{"an A record without an IPv4 address", "/v2/domains/acme.dev/records", map[string]any{"type": "A", "name": "www", "data": "2001:db8::1"}, http.StatusUnprocessableEntity},
		{"an unknown type", "/v2/domains/acme.dev/records", map[string]any{"type": "ALIAS", "name": "www", "data": "203.0.113.7"}, http.StatusUnprocessableEntity},
		{"an SRV record without a port", "/v2/domains/acme.dev/records", map[string]any{"type": "SRV", "name": "_sip._tcp", "data": "sip.acme.dev.", "priority": 10, "weight": 5}, http.StatusUnprocessableEntity},
		{"a TTL below 30 seconds", "/v2/domains/acme.dev/records", map[string]any{"type": "A", "name": "www", "data": "203.0.113.7", "ttl": 10}, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status := e.call(http.MethodPost, c.path, c.body, nil); status != c.status {
				t.Fatalf("POST = %d, want %d", status, c.status)
			}
		})
	}

	var fqdn domainRecordResponse
	e.mustCall(http.StatusCreated, http.MethodPost, "/v2/domains/acme.dev/records", map[string]any{"type": "A", "name": "www.acme.dev.", "data": "203.0.113.7"}, &fqdn)
	if fqdn.DomainRecord.Name != "www" || fqdn.DomainRecord.TTL != 1800 {
		t.Fatalf("record named www.acme.dev. = %+v, want the relative name www and the default TTL 1800", fqdn.DomainRecord)
	}
}

func TestDeletingADomainDeletesItsRecords(t *testing.T) {
	e := newEnv(t)
	e.createDomain("acme.dev", "203.0.113.7")

	e.mustCall(http.StatusNoContent, http.MethodDelete, "/v2/domains/acme.dev", nil, nil)

	if status := e.call(http.MethodGet, "/v2/domains/acme.dev/records", nil, nil); status != http.StatusNotFound {
		t.Fatalf("GET records after delete = %d, want 404", status)
	}
	if status := e.call(http.MethodDelete, "/v2/domains/acme.dev", nil, nil); status != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", status)
	}
}
