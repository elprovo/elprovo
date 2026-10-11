package main

import (
	"reflect"
	"testing"
)

func TestDiffAddressesRemovesTheOldAndAddsTheNew(t *testing.T) {
	remove, add := diffAddresses([]string{"172.31.128.1", "172.31.128.2"}, []string{"172.31.128.2", "172.31.128.3"})

	if !reflect.DeepEqual(remove, []string{"172.31.128.1"}) || !reflect.DeepEqual(add, []string{"172.31.128.3"}) {
		t.Fatalf("remove %v, add %v; want to remove .1 and add .3", remove, add)
	}
}

func TestInterfaceWithAddressFindsThePublicInterface(t *testing.T) {
	out := "1: lo    inet 127.0.0.1/8 scope host lo\n" +
		"2: eth0    inet 172.30.255.254/16 brd 172.30.255.255 scope global eth0\n" +
		"3: eth1    inet 172.31.255.254/16 brd 172.31.255.255 scope global eth1\n"

	if got := interfaceWithAddress(out, "172.31.255.254"); got != "eth1" {
		t.Fatalf("interface = %q, want eth1", got)
	}
	if got := interfaceWithAddress(out, "172.31.255.25"); got != "" {
		t.Fatalf("a prefix of an address matched %q", got)
	}
}

func TestParseAddressesAcceptsOnlyIPv4(t *testing.T) {
	if _, err := parseAddresses([]byte(`{"addresses":["fd00::1"]}`)); err == nil {
		t.Fatal("an IPv6 address was accepted")
	}
	got, err := parseAddresses([]byte(`{"addresses":["172.31.128.1"]}`))
	if err != nil || !reflect.DeepEqual(got, []string{"172.31.128.1"}) {
		t.Fatalf("parse = %v, %v", got, err)
	}
}
