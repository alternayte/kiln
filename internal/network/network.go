// Package network owns the host side of every microVM link: the TAP device,
// the per-VM nftables chains and the resolver that answers only the names in a
// template's allowlist. Guest addresses never vary; only the host side does.
package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/alternayte/kiln/internal/runtime"
)

// Table is the one nftables table Kiln owns.
const Table = "kiln"

// metadataAddr is dropped before any allow rule. It cannot be configured back
// on.
const metadataAddr = "169.254.169.254"

// tapMAC is the host-side MAC of every TAP. All guest links are separate L2
// segments, so one MAC is not a collision, and it keeps a restored guest's
// gateway ARP entry valid.
const tapMAC = "02:00:00:00:00:01"

// Manager creates and removes per-VM network attachments.
type Manager struct {
	next atomic.Uint32
}

// New returns the network manager.
func New() *Manager { return &Manager{} }

// Attachment is one VM's host-side link: a TAP device, a routing table, a
// firewall mark and the resolver sockets.
type Attachment struct {
	ID      string
	TAPName string

	short    string
	mark     int
	table    int
	resolver *resolver

	mu   sync.Mutex
	done bool
}

// EnsureBase creates the Kiln table and its shared mark chain, and enables
// forwarding. It is idempotent.
func (m *Manager) EnsureBase(ctx context.Context) error {
	if err := enableForwarding(); err != nil {
		return err
	}
	out, err := run(ctx, "nft", "list", "table", "inet", Table)
	if err != nil {
		return nftScript(ctx, "add table inet "+Table+"\n"+markScript())
	}
	if !strings.Contains(out, "chain markcopy") {
		return nftScript(ctx, markScript())
	}
	return nil
}

// markScript copies the conntrack mark onto the packet mark for traffic to a
// guest, so a reply routes back out the TAP its connection arrived on. It runs
// at priority 0, after the de-NAT in the dstnat chain at -100, so the
// destination is the guest address by the time the rule is evaluated. The
// chain is named markcopy because mark is a reserved nftables word.
func markScript() string {
	return "add chain inet " + Table + " markcopy { type filter hook prerouting priority 0; policy accept; }\n" +
		"add rule inet " + Table + " markcopy ip daddr " + runtime.GuestIP + " meta mark set ct mark\n"
}

// Attach creates the TAP device, the resolver, the nftables chains and the
// routing rule for one VM. The allowlist is the template's egress_allow.
func (m *Manager) Attach(ctx context.Context, id string, allow []string) (*Attachment, error) {
	if _, err := normalizeAllow(allow); err != nil {
		return nil, err
	}
	short := shortID(id)
	idx := int(m.next.Add(1))
	a := &Attachment{ID: id, TAPName: "kiln-" + short, short: short, mark: idx, table: 1000 + idx}

	if _, err := run(ctx, "ip", "tuntap", "add", "dev", a.TAPName, "mode", "tap",
		"user", strconv.Itoa(runtime.JailUID), "group", strconv.Itoa(runtime.JailGID)); err != nil {
		return nil, err
	}
	if err := a.setUp(ctx); err != nil {
		_ = a.Detach(ctx)
		return nil, err
	}
	up, err := upstreams()
	if err != nil {
		_ = a.Detach(ctx)
		return nil, err
	}
	res, err := newResolver(ctx, a, allow, up)
	if err != nil {
		_ = a.Detach(ctx)
		return nil, err
	}
	a.resolver = res
	if err := a.applyRules(ctx); err != nil {
		_ = a.Detach(ctx)
		return nil, err
	}
	if err := a.applyRoutes(ctx); err != nil {
		_ = a.Detach(ctx)
		return nil, err
	}
	return a, nil
}

// Detach removes every host resource this attachment owns. It is idempotent.
func (a *Attachment) Detach(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return nil
	}
	a.done = true
	if a.resolver != nil {
		a.resolver.Close()
		a.resolver = nil
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, suffix := range []string{"_fwd", "_in", "_dnat", "_src"} {
		if _, err := run(ctx, "nft", "delete", "chain", "inet", Table, a.chain(suffix)); err != nil {
			keep(err)
		}
	}
	if _, err := run(ctx, "ip", "rule", "del", "fwmark", strconv.Itoa(a.mark),
		"to", runtime.GuestIP, "lookup", strconv.Itoa(a.table)); err != nil {
		keep(err)
	}
	if _, err := run(ctx, "ip", "route", "del", runtime.GuestIP+"/32",
		"dev", a.TAPName, "table", strconv.Itoa(a.table)); err != nil {
		keep(err)
	}
	if _, err := run(ctx, "ip", "addr", "del", runtime.GuestGateway+"/32", "dev", a.TAPName); err != nil {
		keep(err)
	}
	if _, err := run(ctx, "ip", "link", "del", a.TAPName); err != nil {
		keep(err)
	}
	return first
}

func (a *Attachment) setUp(ctx context.Context) error {
	// Every TAP presents the same host MAC. A restored guest keeps the ARP
	// entry for its gateway from the snapshot, so a new MAC would drop its
	// first packets until the entry expires.
	for _, args := range [][]string{
		{"link", "set", a.TAPName, "address", tapMAC},
		{"link", "set", a.TAPName, "up"},
		{"addr", "add", runtime.GuestGateway + "/32", "dev", a.TAPName},
	} {
		if _, err := run(ctx, "ip", args...); err != nil {
			return err
		}
	}
	// A guest source address is 172.31.0.2 on every TAP, so the reverse path
	// filter has no unique route to check and ARP must answer only on the
	// interface that owns the address.
	for _, sysctl := range []struct{ name, value string }{
		{"rp_filter", "0"},
		{"arp_ignore", "1"},
		{"arp_announce", "2"},
	} {
		path := "/proc/sys/net/ipv4/conf/" + a.TAPName + "/" + sysctl.name
		if err := os.WriteFile(path, []byte(sysctl.value+"\n"), 0o644); err != nil {
			return fmt.Errorf("network: write %s: %w", path, err)
		}
	}
	return nil
}

func (a *Attachment) applyRules(ctx context.Context) error {
	if a.resolver == nil {
		return fmt.Errorf("network: internal: no resolver")
	}
	tap := a.TAPName
	var b strings.Builder
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags timeout; }\n", Table, a.setName())

	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook forward priority 0; policy accept; }\n", Table, a.chain("_fwd"))
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ct mark set %d\n", Table, a.chain("_fwd"), tap, a.mark)
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s drop\n", Table, a.chain("_fwd"), tap, metadataAddr)
	// An established connection was allowed when it started. Keep it after its
	// address leaves the set, so a long transfer is not cut by a DNS TTL.
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ct state established,related accept\n", Table, a.chain("_fwd"), tap)
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr @%s tcp dport 443 accept\n", Table, a.chain("_fwd"), tap, a.setName())
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q drop\n", Table, a.chain("_fwd"), tap)

	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook input priority 0; policy accept; }\n", Table, a.chain("_in"))
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ct mark set %d\n", Table, a.chain("_in"), tap, a.mark)
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s drop\n", Table, a.chain("_in"), tap, metadataAddr)
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s udp dport %d accept\n", Table, a.chain("_in"), tap, runtime.GuestGateway, a.resolver.udpPort())
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s tcp dport %d accept\n", Table, a.chain("_in"), tap, runtime.GuestGateway, a.resolver.tcpPort())
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q drop\n", Table, a.chain("_in"), tap)

	fmt.Fprintf(&b, "add chain inet %s %s { type nat hook prerouting priority dstnat; policy accept; }\n", Table, a.chain("_dnat"))
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s udp dport 53 dnat ip to %s:%d\n",
		Table, a.chain("_dnat"), tap, runtime.GuestGateway, runtime.GuestGateway, a.resolver.udpPort())
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q ip daddr %s tcp dport 53 dnat ip to %s:%d\n",
		Table, a.chain("_dnat"), tap, runtime.GuestGateway, runtime.GuestGateway, a.resolver.tcpPort())

	fmt.Fprintf(&b, "add chain inet %s %s { type nat hook postrouting priority srcnat; policy accept; }\n", Table, a.chain("_src"))
	fmt.Fprintf(&b, "add rule inet %s %s iifname %q masquerade\n", Table, a.chain("_src"), tap)

	return nftScript(ctx, b.String())
}

func (a *Attachment) applyRoutes(ctx context.Context) error {
	if _, err := run(ctx, "ip", "route", "add", runtime.GuestIP+"/32",
		"dev", a.TAPName, "table", strconv.Itoa(a.table)); err != nil {
		return err
	}
	if _, err := run(ctx, "ip", "rule", "add", "fwmark", strconv.Itoa(a.mark),
		"to", runtime.GuestIP, "lookup", strconv.Itoa(a.table)); err != nil {
		return err
	}
	return nil
}

func (a *Attachment) chain(suffix string) string { return "kiln_" + a.short + suffix }

func (a *Attachment) setName() string { return "kiln_" + a.short + "_v4" }

// shortID is the stable short name of a VM id. It carries the id without
// relying on its characters, and fits the 15 byte interface name limit.
func shortID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:10]
}

// enableForwarding makes the host route between TAP devices and its uplink.
// rp_filter is cleared for the same reason it is cleared per TAP: every guest
// uses one source address, so no unique reverse route exists. Egress control
// is the per-VM nftables chain, not the reverse path filter.
func enableForwarding() error {
	for _, sysctl := range []struct{ path, value string }{
		{"/proc/sys/net/ipv4/ip_forward", "1"},
		{"/proc/sys/net/ipv4/conf/all/rp_filter", "0"},
	} {
		b, err := os.ReadFile(sysctl.path)
		if err != nil {
			return fmt.Errorf("network: read %s: %w", sysctl.path, err)
		}
		if strings.TrimSpace(string(b)) == sysctl.value {
			continue
		}
		if err := os.WriteFile(sysctl.path, []byte(sysctl.value+"\n"), 0o644); err != nil {
			return fmt.Errorf("network: write %s: %w", sysctl.path, err)
		}
	}
	return nil
}

// nftScript applies one nftables script from stdin. A failure returns the
// command output.
func nftScript(ctx context.Context, script string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("network: nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// run runs one command and returns its combined output on failure.
func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("network: %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ValidateAllow reports whether every entry is a hostname.
func ValidateAllow(allow []string) error {
	_, err := normalizeAllow(allow)
	return err
}

// normalizeAllow lowercases names and rejects entries that are not hostnames.
func normalizeAllow(names []string) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	for i, raw := range names {
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if name == "" {
			return nil, fmt.Errorf("network: egress_allow[%d] is empty", i)
		}
		if !validHostname(name) {
			return nil, fmt.Errorf("network: egress_allow[%d] %q is not a hostname", i, raw)
		}
		out[name] = true
	}
	return out, nil
}

func validHostname(name string) bool {
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '-' && i != 0 && i != len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// upstreams reads the host's resolvers. The resolver forwards allowlisted
// queries to them.
func upstreams() ([]string, error) {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := strings.Split(fields[1], "%")[0]
		if net.ParseIP(ip) == nil {
			continue
		}
		out = append(out, ip)
		if len(out) == 3 {
			break
		}
	}
	return out, nil
}
