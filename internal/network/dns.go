package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// DNS record types the resolver cares about.
const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
)

// DNS response codes.
const (
	dnsFormErr  = 1
	dnsServFail = 2
	dnsRefused  = 5
)

var errNoQuestion = errors.New("dns: exactly one question is required")

// question returns the lowercased qname and the offset just past the
// question section.
func question(msg []byte) (string, int, error) {
	if len(msg) < 12 {
		return "", 0, errors.New("dns: message is too short")
	}
	if binary.BigEndian.Uint16(msg[4:6]) != 1 {
		return "", 0, errNoQuestion
	}
	name, next, err := dnsName(msg, 12)
	if err != nil {
		return "", 0, err
	}
	if next+4 > len(msg) {
		return "", 0, errors.New("dns: truncated question")
	}
	return strings.ToLower(name), next + 4, nil
}

// dnsName parses one possibly compressed name and returns it with the offset
// just past its first occurrence.
func dnsName(msg []byte, off int) (string, int, error) {
	var labels []string
	next := off
	pointed := false
	hops := 0
	for {
		if off >= len(msg) {
			return "", 0, errors.New("dns: name runs past the message")
		}
		b := int(msg[off])
		switch {
		case b == 0:
			if !pointed {
				next = off + 1
			}
			return strings.Join(labels, "."), next, nil
		case b&0xc0 == 0xc0:
			if off+2 > len(msg) {
				return "", 0, errors.New("dns: truncated compression pointer")
			}
			if !pointed {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:off+2]) & 0x3fff)
			pointed = true
			hops++
			if hops > 20 {
				return "", 0, errors.New("dns: compression loop")
			}
		case b <= 63:
			if off+1+b > len(msg) {
				return "", 0, errors.New("dns: truncated label")
			}
			labels = append(labels, string(msg[off+1:off+1+b]))
			off += 1 + b
		default:
			return "", 0, fmt.Errorf("dns: bad label length %d", b)
		}
		if len(labels) > 128 {
			return "", 0, errors.New("dns: name has too many labels")
		}
	}
}

// answer is one parsed resource record.
type answer struct {
	name string
	typ  uint16
	ttl  uint32
	data []byte
}

// answers parses the answer section of a DNS response.
func answers(msg []byte) ([]answer, error) {
	if len(msg) < 12 {
		return nil, errors.New("dns: message is too short")
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := dnsName(msg, off)
		if err != nil {
			return nil, err
		}
		if next+4 > len(msg) {
			return nil, errors.New("dns: truncated question")
		}
		off = next + 4
	}
	out := make([]answer, 0, an)
	for i := 0; i < an; i++ {
		name, next, err := dnsName(msg, off)
		if err != nil {
			return nil, err
		}
		if next+10 > len(msg) {
			return nil, errors.New("dns: truncated record")
		}
		typ := binary.BigEndian.Uint16(msg[next : next+2])
		ttl := binary.BigEndian.Uint32(msg[next+4 : next+8])
		rdlen := int(binary.BigEndian.Uint16(msg[next+8 : next+10]))
		dataStart := next + 10
		if dataStart+rdlen > len(msg) {
			return nil, errors.New("dns: truncated record data")
		}
		out = append(out, answer{name: name, typ: typ, ttl: ttl, data: msg[dataStart : dataStart+rdlen]})
		off = dataStart + rdlen
	}
	return out, nil
}

// response builds a reply with the given rcode. A well formed query has its
// question echoed; a malformed one gets a bare header.
func response(query []byte, rcode int) []byte {
	if len(query) < 12 {
		return nil
	}
	out := make([]byte, 12, 12+len(query))
	copy(out, query[:12])
	binary.BigEndian.PutUint16(out[2:4], 0x8180|uint16(rcode))
	binary.BigEndian.PutUint16(out[4:6], 0)
	binary.BigEndian.PutUint16(out[6:8], 0)
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)
	if _, off, err := question(query); err == nil {
		out = append(out, query[12:off]...)
		binary.BigEndian.PutUint16(out[4:6], 1)
	}
	return out
}
