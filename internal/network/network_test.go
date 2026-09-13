package network

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

func dnsQuery(name string, typ uint16) []byte {
	var b []byte
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:2], 0x1234)
	binary.BigEndian.PutUint16(hdr[2:4], 0x0100)
	binary.BigEndian.PutUint16(hdr[4:6], 1)
	b = append(b, hdr[:]...)
	for _, label := range strings.Split(name, ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	var tail [4]byte
	binary.BigEndian.PutUint16(tail[0:2], typ)
	binary.BigEndian.PutUint16(tail[2:4], 1)
	return append(b, tail[:]...)
}

func dnsResponse(t *testing.T, question []byte, records []answer) []byte {
	t.Helper()
	msg := make([]byte, len(question))
	copy(msg, question)
	binary.BigEndian.PutUint16(msg[2:4], 0x8180)
	binary.BigEndian.PutUint16(msg[6:8], uint16(len(records)))
	for _, rec := range records {
		msg = append(msg, 0xc0, 0x0c) // pointer to the question name
		var fixed [10]byte
		binary.BigEndian.PutUint16(fixed[0:2], rec.typ)
		binary.BigEndian.PutUint16(fixed[2:4], 1)
		binary.BigEndian.PutUint32(fixed[4:8], rec.ttl)
		binary.BigEndian.PutUint16(fixed[8:10], uint16(len(rec.data)))
		msg = append(msg, fixed[:]...)
		msg = append(msg, rec.data...)
	}
	return msg
}

func TestQuestionParsesNormalQuery(t *testing.T) {
	name, end, err := question(dnsQuery("pypi.org", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if name != "pypi.org" {
		t.Fatalf("name %q, want pypi.org", name)
	}
	if end != len(dnsQuery("pypi.org", dnsTypeA)) {
		t.Fatalf("end %d, want the whole query", end)
	}
}

func TestQuestionRejectsMalformed(t *testing.T) {
	cases := map[string][]byte{
		"short":     {0, 1, 2},
		"no name":   append(make([]byte, 12), 0),
		"two q":     func() []byte { q := dnsQuery("a.example", dnsTypeA); binary.BigEndian.PutUint16(q[4:6], 2); return q }(),
		"bad label": append(append(make([]byte, 12), 0x40), 0, 0, 0, 1, 0, 1),
	}
	for name, msg := range cases {
		if _, _, err := question(msg); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}

func TestAnswersReadsCompressedRecords(t *testing.T) {
	q := dnsQuery("pypi.org", dnsTypeA)
	resp := dnsResponse(t, q, []answer{
		{typ: dnsTypeA, ttl: 60, data: net.IPv4(1, 2, 3, 4).To4()},
		{typ: dnsTypeAAAA, ttl: 30, data: net.ParseIP("2001:db8::1").To16()},
		{typ: 5, ttl: 20, data: []byte{3, 'w', 'w', 'w', 0}},
	})
	got, err := answers(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("records %d, want 3", len(got))
	}
	if net.IP(got[0].data).String() != "1.2.3.4" || got[0].ttl != 60 {
		t.Fatalf("A record %+v", got[0])
	}
	if net.IP(got[1].data).String() != "2001:db8::1" || got[1].typ != dnsTypeAAAA {
		t.Fatalf("AAAA record %+v", got[1])
	}
}

func TestAnswersRejectsTruncatedRecord(t *testing.T) {
	q := dnsQuery("pypi.org", dnsTypeA)
	resp := dnsResponse(t, q, []answer{{typ: dnsTypeA, ttl: 60, data: net.IPv4(1, 2, 3, 4).To4()}})
	if _, err := answers(resp[:len(resp)-2]); err == nil {
		t.Fatal("truncated record accepted")
	}
}

func TestResponseEchoesQuestionAndRCode(t *testing.T) {
	q := dnsQuery("pypi.org", dnsTypeA)
	resp := response(q, dnsRefused)
	if len(resp) != len(q) {
		t.Fatalf("response length %d, want %d", len(resp), len(q))
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0x1234 {
		t.Fatal("response id changed")
	}
	if rcode := binary.BigEndian.Uint16(resp[2:4]) & 0x000f; rcode != dnsRefused {
		t.Fatalf("rcode %d, want %d", rcode, dnsRefused)
	}
	if binary.BigEndian.Uint16(resp[4:6]) != 1 {
		t.Fatal("question count is not 1")
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Fatal("answer count is not 0")
	}
}

func TestNormalizeAllow(t *testing.T) {
	got, err := normalizeAllow([]string{" PyPI.org ", "files.pythonhosted.org."})
	if err != nil {
		t.Fatal(err)
	}
	if !got["pypi.org"] || !got["files.pythonhosted.org"] {
		t.Fatalf("allow %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("allow has %d entries, want 2", len(got))
	}
	for _, bad := range [][]string{{" "}, {"bad/name"}, {"-bad.example"}, {"bad..example"}, {""}} {
		if _, err := normalizeAllow(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestValidateAllowAcceptsEmpty(t *testing.T) {
	if err := ValidateAllow(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAllow([]string{}); err != nil {
		t.Fatal(err)
	}
}

func TestShortIDFitsInterfaceName(t *testing.T) {
	a := shortID("build-py312")
	if len(a) != 10 {
		t.Fatalf("short id %q length %d, want 10", a, len(a))
	}
	if len("kiln-"+a) > 15 {
		t.Fatalf("tap name %q is too long", "kiln-"+a)
	}
	if a == shortID("build-py313") {
		t.Fatal("different ids share a short id")
	}
	if a != shortID("build-py312") {
		t.Fatal("short id is not stable")
	}
}

func TestResolverRefusesNamesOutsideTheAllowlist(t *testing.T) {
	r := &resolver{id: "test", allow: map[string]bool{"pypi.org": true}}
	resp := r.handle(dnsQuery("example.com", dnsTypeA))
	if resp == nil {
		t.Fatal("no response")
	}
	if rcode := binary.BigEndian.Uint16(resp[2:4]) & 0x000f; rcode != dnsRefused {
		t.Fatalf("rcode %d, want refused", rcode)
	}
	resp = r.handle(dnsQuery("pypi.org", dnsTypeA))
	if rcode := binary.BigEndian.Uint16(resp[2:4]) & 0x000f; rcode != dnsServFail {
		t.Fatalf("rcode %d, want servfail without an upstream", rcode)
	}
}
