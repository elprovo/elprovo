package lima

import (
	"reflect"
	"testing"
)

func TestParseIPv4ReadsInterfaceAddress(t *testing.T) {
	out := []byte(`2: eth0    inet 192.168.104.3/24 metric 200 brd 192.168.104.255 scope global dynamic eth0\       valid_lft 3570sec preferred_lft 3570sec
`)
	got, err := parseIPv4(out)
	if err != nil {
		t.Fatalf("parseIPv4: %v", err)
	}
	if got != "192.168.104.3" {
		t.Fatalf("address = %q, want 192.168.104.3", got)
	}
}

func TestParseIPv4FailsWithoutAddress(t *testing.T) {
	if _, err := parseIPv4([]byte("Device \"eth0\" does not exist.\n")); err == nil {
		t.Fatal("parseIPv4 returned no error for output without an address")
	}
}

func TestParseInstanceNamesReadsNDJSON(t *testing.T) {
	out := []byte(`{"name":"stagely-a1","status":"Running"}
{"name":"other","status":"Stopped"}
`)
	got, err := parseInstanceNames(out)
	if err != nil {
		t.Fatalf("parseInstanceNames: %v", err)
	}
	if want := []string{"stagely-a1", "other"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
}

func TestParseInstanceNamesAcceptsNoInstances(t *testing.T) {
	got, err := parseInstanceNames(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("parseInstanceNames(nil) = %v, %v; want no names and no error", got, err)
	}
}
