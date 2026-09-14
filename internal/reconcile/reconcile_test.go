package reconcile

import "testing"

func TestParseTapsReadsKilnDevicesOnly(t *testing.T) {
	out := `1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN mode DEFAULT group default qlen 1000
3: kiln-0123456789: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
4: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
5: kiln-deadbeef: <BROADCAST,MULTICAST> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000`
	got := parseTaps(out)
	if len(got) != 2 || got[0] != "kiln-0123456789" || got[1] != "kiln-deadbeef" {
		t.Fatalf("parseTaps = %v, want [kiln-0123456789 kiln-deadbeef]", got)
	}
}

func TestParseChainsReadsShortIDs(t *testing.T) {
	out := `table inet kiln {
	chain markcopy {
		type filter hook prerouting priority filter; policy accept;
	}
	chain kiln_0123456789_fwd {
	}
	chain kiln_0123456789_dnat {
	}
	chain other_chain {
	}
}`
	got := parseChains(out)
	if len(got) != 2 || got[0] != "0123456789" || got[1] != "0123456789" {
		t.Fatalf("parseChains = %v, want two 0123456789 entries", got)
	}
}

func TestCgroupIDFromData(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{"jailer child", "0::/kiln/abc123/abc123\n", "abc123", true},
		{"direct parent", "0::/kiln/abc123\n", "abc123", true},
		{"daemon", "0::/system.slice/kiln.service\n", "", false},
		{"root", "0::/\n", "", false},
		{"v1 layout", "5:memory:/kiln/abc\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cgroupIDFromData(tc.data)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("cgroupIDFromData(%q) = %q, %t; want %q, %t", tc.data, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSnapfaultSocketFromCmdline(t *testing.T) {
	line := "/var/lib/kiln/bin/kiln\x00snapfault\x00--mem\x00/templates/py/mem\x00--sock\x00/var/lib/kiln/sandboxes/abc/jail/firecracker/abc/root/run/uffd.sock\x00"
	got, ok := snapfaultSocketFromCmdline(line)
	if !ok || got != "/var/lib/kiln/sandboxes/abc/jail/firecracker/abc/root/run/uffd.sock" {
		t.Fatalf("snapfaultSocketFromCmdline = %q, %t", got, ok)
	}
	if _, ok := snapfaultSocketFromCmdline("/var/lib/kiln/bin/kiln\x00serve\x00"); ok {
		t.Fatal("a serve process was read as a page fault source")
	}
}

func TestSandboxIDFromPath(t *testing.T) {
	const root = "/var/lib/kiln"
	got, ok := sandboxIDFromPath(root, "/var/lib/kiln/sandboxes/abc123/jail/firecracker/abc123/root/run/uffd.sock")
	if !ok || got != "abc123" {
		t.Fatalf("sandboxIDFromPath = %q, %t, want abc123", got, ok)
	}
	if _, ok := sandboxIDFromPath(root, "/var/lib/kiln/templates/py/mem"); ok {
		t.Fatal("a template path produced a sandbox id")
	}
}

func TestRouteDevice(t *testing.T) {
	got := routeDevice("172.31.0.2 dev kiln-0123456789 scope link")
	if got != "kiln-0123456789" {
		t.Fatalf("routeDevice = %q", got)
	}
	if got := routeDevice("blackhole 172.31.0.0/30"); got != "" {
		t.Fatalf("routeDevice without a device = %q", got)
	}
}
