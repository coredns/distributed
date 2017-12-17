package distributed

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"

	"github.com/mholt/caddy"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func init() {
	caddy.RegisterPlugin("distributed", caddy.Plugin{
		ServerType: "dns",
		Action:     setup,
	})
}

type nsid struct {
	sync.RWMutex
	data string
	tmpl *template.Template
	quit chan struct{}
}

func (n *nsid) onShutdown(d *Distributed) error {
	close(n.quit)
	return nil
}

func (n *nsid) onStartup(d *Distributed) error {
	var data bytes.Buffer
	if err := n.tmpl.Execute(&data, nil); err == nil {
		if v := data.String(); v != "" {
			log.Printf("[INFO] NSID %q", plugin.Name(v).Normalize())
			n.data = hex.EncodeToString([]byte(plugin.Name(v).Normalize()))
			return nil
		}
	}
	id, err := ec2Metadata(ec2AmiLaunchIndex)
	if err != nil {
		return err
	}
	data.Reset()
	if err := n.tmpl.Execute(&data, map[string]string{"id": id}); err == nil {
		if v := data.String(); v != "" {
			log.Printf("[INFO] NSID %q", plugin.Name(v).Normalize())
			n.data = hex.EncodeToString([]byte(plugin.Name(v).Normalize()))
			return nil
		}
	}
	return fmt.Errorf("invalid id information")
}

func (n *nsid) value(d *Distributed) string {
	n.RLock()
	defer n.RUnlock()
	return n.data
}

func ec2Metadata(query string) (string, error) {
	resp, err := http.Get(query)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invalid status code: %s (%d)", resp.Status, resp.StatusCode)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", fmt.Errorf("invalid response %q", string(body))
	}
	return string(body), nil
}

func setup(c *caddy.Controller) error {
	origin, endpoints, tmpl, err := distributedParse(c)
	if err != nil {
		return plugin.Error("distributed", err)
	}

	n := nsid{
		tmpl: tmpl,
		quit: make(chan struct{}),
	}

	d := &Distributed{
		Entries: &entries{
			byName: map[string][]net.IP{},
			byAddr: map[string]string{},
		},
	}
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		d.Next = next
		d.Origin = origin
		d.Endpoints = &endpoints
		d.Identity = &n
		return d
	})

	for i := range endpoints {
		u := endpoints[i]
		c.OnStartup(func() error {
			return u.OnStartup(d)
		})
		c.OnShutdown(func() error {
			return u.OnShutdown(d)
		})
	}

	c.OnStartup(func() error {
		return n.onStartup(d)
	})
	c.OnShutdown(func() error {
		return n.onShutdown(d)
	})

	return nil
}

func distributedParse(c *caddy.Controller) (string, []endpoint, *template.Template, error) {
	for c.Next() {
		args := c.RemainingArgs()
		if len(args) != 3 {
			return "", nil, nil, c.Dispenser.ArgErr()
		}
		var credential *credentials.Credentials
		for c.NextBlock() {
			switch c.Val() {
			case "aws_access_key":
				v := c.RemainingArgs()
				if len(v) < 2 {
					return "", nil, nil, c.Errf("invalid access key '%v'", v)
				}
				credential = credentials.NewStaticCredentials(v[0], v[1], "")
			default:
				return "", nil, nil, c.Errf("unknown property '%s'", c.Val())
			}
		}

		origin := plugin.Host(args[0]).Normalize()

		endpoints, err := parseEndpoint(args[1], credential)
		if err != nil {
			return "", nil, nil, err
		}

		tmpl := template.Must(template.New("nsid").Option("missingkey=error").Parse(args[2]))

		if origin == "" || len(endpoints) == 0 || tmpl == nil {
			return "", nil, nil, c.Dispenser.ArgErr()
		}

		if err := tmpl.Execute(ioutil.Discard, map[string]int{"id": 0}); err != nil {
			return "", nil, nil, err
		}

		return origin, endpoints, tmpl, nil
	}
	return "", nil, nil, c.Dispenser.ArgErr()
}

func parseEndpoint(arg string, credential *credentials.Credentials) ([]endpoint, error) {
	var endpoints []endpoint

	entries := map[string]struct{}{}
	if strings.HasPrefix(arg, "aws:ec2:Subnet") {
		privateIPAddress, err := ec2Metadata(ec2PrivateIPAddress)
		if err != nil {
			return nil, fmt.Errorf("invalid private ip: %s", err)
		}
		interfaces, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("invalid interfaces: %s", err)
		}
		for _, i := range interfaces {
			addrs, err := i.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				n, ok := a.(*net.IPNet)
				if !ok || n.IP.String() != privateIPAddress {
					continue
				}
				ip, ipnet, err := net.ParseCIDR(n.String())
				if err != nil {
					continue
				}
				port := "53"
				l := listIPAddr(ip, ipnet)
				// remove aws subnet reserved address
				for _, ip = range l[4 : len(l)-1] {
					if _, ok := entries[ip.String()]; !ok {
						entries[ip.String()] = struct{}{}
						endpoints = append(endpoints, endpoint{
							addr: ip.String(),
							port: port,
							quit: make(chan struct{}),
						})
					}
				}
				return endpoints, nil
			}
		}

		return nil, fmt.Errorf("invalid subnet for %q", privateIPAddress)
	}
	if strings.HasPrefix(arg, "aws:ec2:ReservationId") {
		reservationID, err := ec2Metadata(ec2ReservationID)
		if err != nil {
			return nil, fmt.Errorf("invalid reservation id: %s", err)
		}

		az, err := ec2Metadata(ec2AvailabilityZone)
		if err != nil {
			return nil, fmt.Errorf("invalide availability zone: %s", err)
		}
		if len(az) <= 1 {
			return nil, fmt.Errorf("invalide availability zone %q", az)
		}
		region := az[:len(az)-1]

		service := ec2.New(session.Must(session.NewSession(&aws.Config{
			Region:      aws.String(region),
			Credentials: credential,
		})))

		result, err := service.DescribeInstances(&ec2.DescribeInstancesInput{
			Filters: []*ec2.Filter{
				{
					Name: aws.String("reservation-id"),
					Values: []*string{
						aws.String(reservationID),
					},
				},
			},
		})
		if err != nil {
			return nil, err
		}

		port := "53"
		for _, reservation := range result.Reservations {
			for _, instance := range reservation.Instances {
				if instance.PrivateIpAddress != nil {
					endpoints = append(endpoints, endpoint{
						addr: aws.StringValue(instance.PrivateIpAddress),
						port: port,
						quit: make(chan struct{}),
					})
				}
			}
		}

		return endpoints, nil
	}
	for _, s := range strings.Split(arg, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		host := s
		port := "53"
		colon := strings.LastIndex(s, ":")
		if colon == len(s)-1 {
			return nil, fmt.Errorf("expecting data after last colon: %q", s)
		}
		if colon != -1 {
			if p, err := strconv.Atoi(s[colon+1:]); err == nil {
				if p < 0 || p >= 65536 {
					return nil, fmt.Errorf("invalid port number: %q", s[colon+1:])
				}
				port = strconv.Itoa(p)
				host = s[:colon]
			}
		}

		slash := strings.LastIndex(host, "/")
		if slash == len(host)-1 {
			return nil, fmt.Errorf("expecting data after last slash: %q", host)
		}
		if slash != -1 {
			ip, ipnet, err := net.ParseCIDR(host)
			if err != nil {
				return nil, fmt.Errorf("invalid cidr block %q: %s", host, err)
			}
			l := listIPAddr(ip, ipnet)
			// remove network address and broadcast address
			for _, ip = range l[1 : len(l)-1] {
				if _, ok := entries[ip.String()]; !ok {
					entries[ip.String()] = struct{}{}
					endpoints = append(endpoints, endpoint{
						addr: ip.String(),
						port: port,
						quit: make(chan struct{}),
					})
				}
			}

			continue
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("invalid ip address: %q", host)
		}
		if _, ok := entries[ip.String()]; !ok {
			entries[ip.String()] = struct{}{}
			endpoints = append(endpoints, endpoint{
				addr: ip.String(),
				port: port,
				quit: make(chan struct{}),
			})
		}
	}
	return endpoints, nil
}

func listIPAddr(ip net.IP, ipnet *net.IPNet) []net.IP {
	inc := func(ip net.IP) {
		for j := len(ip) - 1; j >= 0; j-- {
			ip[j]++
			if ip[j] > 0 {
				break
			}
		}
	}

	var entries []net.IP
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		entry := make(net.IP, len(ip))
		copy(entry, ip)
		entries = append(entries, entry)
	}
	return entries
}

const (
	ec2AmiLaunchIndex   = "http://169.254.169.254/latest/meta-data/ami-launch-index"
	ec2ReservationID    = "http://169.254.169.254/latest/meta-data/reservation-id"
	ec2AvailabilityZone = "http://169.254.169.254/latest/meta-data/placement/availability-zone"
	ec2PrivateIPAddress = "http://169.254.169.254/latest/meta-data/local-ipv4"
)
