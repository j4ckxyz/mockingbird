package config

import (
	"net/netip"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{"MB_SECRET_KEY": strings.Repeat("11", 32), "MB_ENCRYPTION_KEY": strings.Repeat("22", 32)}
}

func TestAdminAddrRestrictions(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:9090": true, "localhost:9090": true, "[::1]:9090": true, "100.71.92.71:9090": true,
		":9090": false, "0.0.0.0:9090": false, "192.168.1.5:9090": false, "8.8.8.8:9090": false,
	} {
		e := base()
		e["MB_ADMIN_ADDR"] = addr
		_, err := load(envOf(e))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", addr, err, ok)
		}
	}
}

func TestAdminInContainer(t *testing.T) {
	e := base()
	e["MB_ADMIN_ADDR"] = "0.0.0.0:9090"
	e["MB_ADMIN_IN_CONTAINER"] = "true"
	e["MB_ADMIN_TRUSTED_PEERS"] = "10.89.72.1"
	c, err := load(envOf(e))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AdminPeerAllowed(netip.MustParseAddr("10.89.72.1")) || c.AdminPeerAllowed(netip.MustParseAddr("10.89.72.9")) ||
		!c.AdminPeerAllowed(netip.MustParseAddr("100.64.1.2")) || c.AdminPeerAllowed(netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("admin peer policy wrong")
	}
	// A specific non-admin address is still refused even in container mode.
	e["MB_ADMIN_ADDR"] = "192.168.1.5:9090"
	if _, err := load(envOf(e)); err == nil {
		t.Fatal("LAN admin address accepted")
	}
}

func TestKeysRequiredAndDistinct(t *testing.T) {
	if _, err := load(envOf(map[string]string{})); err == nil {
		t.Fatal("missing keys accepted")
	}
	e := base()
	e["MB_ENCRYPTION_KEY"] = e["MB_SECRET_KEY"]
	if _, err := load(envOf(e)); err == nil {
		t.Fatal("identical keys accepted")
	}
}
