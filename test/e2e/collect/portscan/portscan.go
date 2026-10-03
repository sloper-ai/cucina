// SPDX-License-Identifier: FSL-1.1-ALv2

// Package portscan checks the complete, single-host evidence required by T10i.
// Raw scanner output is private; Report contains only allow-listed summaries.
package portscan

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const Ports = 65535
const MaxXMLBytes = 32 << 20

// Evidence binds scanner output to the one intended numeric IPv4 and its known
// TLS endpoint. A nonzero exit or diagnostic output is not complete evidence.
type Evidence struct {
	Target       string
	PositivePort int
	XML          []byte
	ExitCode     int
	Stderr       []byte
}

// Report deliberately excludes addresses, hostnames, raw output and argv.
type Report struct {
	SchemaVersion int            `json:"schemaVersion"`
	PortsScanned  int            `json:"portsScanned"`
	OpenPorts     []int          `json:"openPorts"`
	States        map[string]int `json:"states"`
}

// Target accepts one canonical numeric unicast IPv4, never an Nmap target
// expression, hostname, CIDR, IPv6 address or broadcast. Loopback permits the
// same public boundary to be exercised by local integration tests.
func Target(raw string) (netip.Addr, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil || !a.Is4() || (!a.IsGlobalUnicast() && !a.IsLoopback()) {
		return netip.Addr{}, errors.New("portscan requires one numeric unicast IPv4")
	}
	return a, nil
}

type state struct {
	Value string `xml:"state,attr"`
}
type port struct {
	ID       int     `xml:"portid,attr"`
	Protocol string  `xml:"protocol,attr"`
	States   []state `xml:"state"`
}
type reason struct {
	Count    int    `xml:"count,attr"`
	Protocol string `xml:"proto,attr"`
	Ports    string `xml:"ports,attr"`
}
type extra struct {
	State   string   `xml:"state,attr"`
	Count   int      `xml:"count,attr"`
	Reasons []reason `xml:"extrareasons"`
}
type portList struct {
	Ports []port  `xml:"port"`
	Extra []extra `xml:"extraports"`
}
type host struct {
	TimedOut  string  `xml:"timedout,attr"`
	States    []state `xml:"status"`
	Addresses []struct {
		Address string `xml:"addr,attr"`
		Type    string `xml:"addrtype,attr"`
	} `xml:"address"`
	Ports []portList `xml:"ports"`
}
type scanInfo struct {
	Type     string `xml:"type,attr"`
	Protocol string `xml:"protocol,attr"`
	Count    int    `xml:"numservices,attr"`
	Services string `xml:"services,attr"`
}
type runStats struct {
	Finished []struct {
		Exit  string `xml:"exit,attr"`
		Error string `xml:"errormsg,attr"`
		Time  string `xml:"time,attr"`
	} `xml:"finished"`
	Hosts []struct {
		Total int `xml:"total,attr"`
		Up    int `xml:"up,attr"`
		Down  int `xml:"down,attr"`
	} `xml:"hosts"`
}
type nmapRun struct {
	XMLName xml.Name   `xml:"nmaprun"`
	Scanner string     `xml:"scanner,attr"`
	Scans   []scanInfo `xml:"scaninfo"`
	Hosts   []host     `xml:"host"`
	Stats   []runStats `xml:"runstats"`
}

// Read requires complete successful XML, one matching host, all 65535 TCP
// ports accounted exactly once, and the intended positive endpoint open.
// Errors are fixed descriptions: untrusted/raw XML and environment identifiers
// must not escape into a bulk result, including in error text.
func Read(e Evidence) (Report, error) {
	bad := func(why string) (Report, error) { return Report{}, errors.New("portscan: " + why) }
	target, err := Target(e.Target)
	if err != nil {
		return Report{}, err
	}
	if e.PositivePort < 1 || e.PositivePort > Ports {
		return bad("known TLS endpoint port is required")
	}
	if e.ExitCode != 0 {
		return bad("scanner did not exit successfully")
	}
	// Nmap progress goes to stdout; stderr diagnostics, including local socket
	// failures and retry-cap warnings, require review rather than a silent pass.
	if len(bytes.TrimSpace(e.Stderr)) != 0 {
		return bad("scanner stderr requires review")
	}
	if len(e.XML) == 0 || len(e.XML) > MaxXMLBytes {
		return bad("missing or oversized XML")
	}
	if err := completeXML(e.XML); err != nil {
		return bad("malformed, duplicate or incomplete XML")
	}
	var run nmapRun
	if err := xml.Unmarshal(e.XML, &run); err != nil {
		return bad("invalid Nmap XML")
	}
	if run.Scanner != "nmap" || len(run.Scans) != 1 {
		return bad("one Nmap scan declaration is required")
	}
	s := run.Scans[0]
	if s.Type != "connect" || s.Protocol != "tcp" || s.Count != Ports {
		return bad("not a full TCP connect scan")
	}
	declared, err := portSet(s.Services)
	if err != nil || len(declared) != Ports {
		return bad("scan declaration does not cover each TCP port once")
	}
	if len(run.Hosts) != 1 || len(run.Stats) != 1 {
		return bad("one host and one completed run are required")
	}
	rs := run.Stats[0]
	if len(rs.Finished) != 1 || len(rs.Hosts) != 1 {
		return bad("missing or duplicate completion records")
	}
	finish := rs.Finished[0]
	finishedAt, err := strconv.ParseUint(finish.Time, 10, 64)
	if err != nil || finishedAt == 0 || finish.Exit != "success" || finish.Error != "" {
		return bad("scan completion is not successful")
	}
	if rs.Hosts[0].Total != 1 || rs.Hosts[0].Up != 1 || rs.Hosts[0].Down != 0 {
		return bad("run did not complete exactly one live host")
	}
	h := run.Hosts[0]
	if h.TimedOut != "" && h.TimedOut != "false" {
		return bad("host timed out or timeout state is invalid")
	}
	if len(h.States) != 1 || h.States[0].Value != "up" || len(h.Ports) != 1 {
		return bad("missing or duplicate host state or port accounting")
	}
	addresses := 0
	for _, a := range h.Addresses {
		if a.Type == "mac" {
			continue
		}
		if a.Type != "ipv4" || a.Address != target.String() {
			return bad("scan host does not match intended IPv4")
		}
		addresses++
	}
	if addresses != 1 {
		return bad("missing or duplicate target address")
	}
	r := Report{SchemaVersion: 1, States: map[string]int{}}
	seen := map[int]bool{}
	for _, p := range h.Ports[0].Ports {
		if p.ID < 1 || p.ID > Ports || p.Protocol != "tcp" || seen[p.ID] || len(p.States) != 1 {
			return bad("invalid or duplicate explicit TCP port")
		}
		st := p.States[0].Value
		if st != "open" && st != "closed" && st != "filtered" {
			return bad("ambiguous explicit port state")
		}
		seen[p.ID] = true
		r.States[st]++
		r.PortsScanned++
		if st == "open" {
			r.OpenPorts = append(r.OpenPorts, p.ID)
		}
	}
	aggregates := map[string]bool{}
	for _, x := range h.Ports[0].Extra {
		if (x.State != "closed" && x.State != "filtered") || x.Count < 1 || x.Count > Ports || aggregates[x.State] {
			return bad("invalid or duplicate aggregate state")
		}
		aggregates[x.State] = true
		reasonCount := 0
		rangedReasons := 0
		for _, reason := range x.Reasons {
			if reason.Count < 1 || reason.Count > Ports || (reason.Protocol != "" && reason.Protocol != "tcp") {
				return bad("invalid aggregate reason accounting")
			}
			reasonCount += reason.Count
			if reason.Ports == "" {
				continue
			}
			rangedReasons++
			set, err := portSet(reason.Ports)
			if err != nil || len(set) != reason.Count {
				return bad("aggregate range disagrees with count")
			}
			for p := range set {
				if seen[p] {
					return bad("aggregate ports overlap other evidence")
				}
				seen[p] = true
			}
		}
		if len(x.Reasons) > 0 && reasonCount != x.Count {
			return bad("aggregate reasons disagree with count")
		}
		if rangedReasons > 0 && rangedReasons != len(x.Reasons) {
			return bad("partially enumerated aggregate reasons")
		}
		r.States[x.State] += x.Count
		r.PortsScanned += x.Count
	}
	if r.PortsScanned != Ports {
		return bad("port accounting does not cover 65535 TCP ports")
	}
	sort.Ints(r.OpenPorts)
	found := false
	for _, p := range r.OpenPorts {
		if p == e.PositivePort {
			found = true
		}
	}
	if !found {
		return bad("known TLS endpoint was not observed open")
	}
	return r, nil
}

// Validate the whole document, not just the first decoded element. Nmap uses
// no XML namespaces. Duplicate attributes must not become last-value-wins.
func completeXML(b []byte) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	depth, roots := 0, 0
	for {
		t, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch v := t.(type) {
		case xml.StartElement:
			if v.Name.Space != "" {
				return errors.New("namespace")
			}
			if depth == 0 {
				roots++
				if roots != 1 || v.Name.Local != "nmaprun" {
					return errors.New("root")
				}
			}
			depth++
			if depth > 32 {
				return errors.New("depth")
			}
			attrs := map[string]bool{}
			for _, a := range v.Attr {
				if a.Name.Space != "" || attrs[a.Name.Local] {
					return errors.New("attribute")
				}
				attrs[a.Name.Local] = true
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(v)) != 0 {
				return errors.New("trailing data")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return errors.New("incomplete")
	}
	return nil
}

func portSet(raw string) (map[int]bool, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		low, high, ranged := strings.Cut(part, "-")
		lo, err := strconv.Atoi(low)
		if err != nil || lo < 1 || lo > Ports {
			return nil, errors.New("port range")
		}
		hi := lo
		if ranged {
			hi, err = strconv.Atoi(high)
		}
		if err != nil || hi < lo || hi > Ports {
			return nil, errors.New("port range")
		}
		for p := lo; p <= hi; p++ {
			if set[p] {
				return nil, errors.New("duplicate port")
			}
			set[p] = true
		}
	}
	return set, nil
}
