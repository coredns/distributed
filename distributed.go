// Package distributed implements a distributed CoreDNS with NSID
package distributed

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

type identity interface {
	value(d *Distributed) string
}

type endpoint struct {
	sync.RWMutex
	addr string
	port string
	quit chan struct{}
}

type entries struct {
	sync.RWMutex
	byAddr map[string]string
	byName map[string][]net.IP
}

// Distributed plugin
type Distributed struct {
	Next       plugin.Handler
	Origin     string
	Identity   identity
	Endpoints  *[]endpoint
	Entries    *entries
	OnStartup  func(d *Distributed) error
	OnShutdown func(d *Distributed) error
	quit       chan struct{}
}

func (e *endpoint) update(d *Distributed) error {
	name, addr, _ := e.query(d)
	if name == "" && addr == "" {
		return nil
	}
	d.Entries.Lock()
	defer d.Entries.Unlock()

	// Only update when data is dirty
	dirty := false
	if name == "" {
		if v, ok := d.Entries.byAddr[addr]; ok {
			delete(d.Entries.byAddr, addr)
			dirty = true
			log.Printf("[INFO] Record %q:%q has been removed from the lookup table", addr, v)
		}
	} else {
		if v, ok := d.Entries.byAddr[addr]; !ok || v != name {
			d.Entries.byAddr[addr] = name
			dirty = true
			log.Printf("[INFO] Record %q:%q has been inserted to the lookup table", addr, name)
		}
	}

	if dirty {
		d.Entries.byName = make(map[string][]net.IP)
		for k, v := range d.Entries.byAddr {
			d.Entries.byName[v] = append(d.Entries.byName[v], net.ParseIP(k))
		}
	}
	return nil
}

func (e *endpoint) query(d *Distributed) (string, string, error) {
	e.RLock()
	defer e.RUnlock()
	if e.addr == "" {
		return "", "", nil
	}

	addr := net.JoinHostPort(e.addr, e.port)
	conn, err := net.DialTimeout("udp", addr, defaultTimeout)
	if err != nil {
		return "", e.addr, err
	}

	req := dns.Msg{}
	req.SetQuestion(dns.Fqdn(d.Origin), dns.TypeSRV)
	req.Question[0].Qclass = dns.ClassINET

	req.SetEdns0(4096, false)
	option := req.Extra[0].(*dns.OPT)
	option.Option = append(option.Option, &dns.EDNS0_NSID{Code: dns.EDNS0NSID, Nsid: ""})

	udpsize := option.UDPSize

	dnsconn := &dns.Conn{Conn: conn, UDPSize: udpsize()}

	writeDeadline := time.Now().Add(defaultTimeout)
	dnsconn.SetWriteDeadline(writeDeadline)
	dnsconn.WriteMsg(&req)

	readDeadline := time.Now().Add(defaultTimeout)
	conn.SetReadDeadline(readDeadline)
	res, err := dnsconn.ReadMsg()

	dnsconn.Close()
	conn.Close()

	if res == nil {
		return "", e.addr, err
	}

	option = res.IsEdns0()
	for _, s := range option.Option {
		switch v := s.(type) {
		case *dns.EDNS0_NSID:
			nsid, err := hex.DecodeString(v.Nsid)
			if err != nil {
				return "", e.addr, err
			}
			return string(nsid), e.addr, nil
		}
	}

	return "", e.addr, fmt.Errorf("no nsid returned for %q", addr)
}

func (d Distributed) lookup(ctx context.Context, qname string) []net.IP {
	// Perform table lookup
	d.Entries.RLock()
	defer d.Entries.RUnlock()
	if len(d.Entries.byName) != 0 {
		if ips, ok := d.Entries.byName[qname]; ok {
			entries := make([]net.IP, len(ips))
			copy(entries, ips)
			return entries
		}
	}
	return nil
}

// ServeDNS implements the plugin.Handler interface.
func (d Distributed) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := plugin.Name(state.Name()).Normalize()

	answers := []dns.RR{}

	if !plugin.Name(d.Origin).Matches(qname) {
		return plugin.NextOrFailure(d.Name(), d.Next, ctx, w, r)
	}

	switch state.QType() {
	case dns.TypeA:
		// A record response to client
		for _, ip := range d.lookup(ctx, qname) {
			a := new(dns.A)
			a.Hdr = dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: defaultTTL}
			a.A = ip
			answers = append(answers, a)
		}
	case dns.TypeSRV:
		// SRV record response so that peers could obtain information (with NSID)
		srv := new(dns.SRV)
		srv.Hdr = dns.RR_Header{Name: "_" + state.Proto() + "." + state.QName(), Rrtype: dns.TypeSRV, Class: state.QClass()}
		if state.QName() == "." {
			srv.Hdr.Name = "_" + state.Proto() + state.QName()
		}
		port, _ := strconv.Atoi(state.Port())
		srv.Port = uint16(port)
		srv.Target = "."

		answers = append(answers, srv)
		if nsid := d.Identity.value(&d); nsid != "" {
			if option := r.IsEdns0(); option != nil {
				for _, o := range option.Option {
					if e, ok := o.(*dns.EDNS0_NSID); ok {
						e.Code = dns.EDNS0NSID
						e.Nsid = nsid
					}
				}
			}
		}
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, true, true
	m.Answer = answers
	if len(answers) == 0 {
		m.Rcode = dns.RcodeNameError
	}

	state.SizeAndDo(m)
	m, _ = state.Scrub(m)
	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// Name implements the Handler interface.
func (d Distributed) Name() string { return "distributed" }

const (
	defaultTTL      = 15
	defaultTimeout  = 5 * time.Second
	defaultInterval = 15 * time.Second
)
