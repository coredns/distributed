package distributed

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"

	"github.com/mholt/caddy"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func init() {
	caddy.RegisterPlugin("distributed", caddy.Plugin{
		ServerType: "dns",
		Action:     setup,
	})
}

type nsid string

func (n nsid) value(d *Distributed) string {
	return hex.EncodeToString([]byte(n))
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
	origin, endpoints, identity, err := distributedParse(c)
	if err != nil {
		return plugin.Error("distributed", err)
	}

	d := &Distributed{
		Entries: &entries{
			byName: map[string][]net.IP{},
			byAddr: map[string]string{},
		},
		Endpoints: &endpoints,
	}
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		d.Next = next
		d.Origin = origin
		d.Identity = nsid(identity)
		return d
	})

	c.OnStartup(func() error {
		return d.OnStartup()
	})
	c.OnShutdown(func() error {
		return d.OnShutdown()
	})
	log.Printf("[INFO] NSID: %q", identity)
	return nil
}

func distributedParse(c *caddy.Controller) (string, []endpoint, string, error) {
	for c.Next() {
		args := c.RemainingArgs()
		if len(args) != 3 {
			return "", nil, "", c.Dispenser.ArgErr()
		}
		var credential *credentials.Credentials
		for c.NextBlock() {
			switch c.Val() {
			case "aws_access_key":
				v := c.RemainingArgs()
				if len(v) < 2 {
					return "", nil, "", c.Errf("invalid access key '%v'", v)
				}
				credential = credentials.NewStaticCredentials(v[0], v[1], "")
			default:
				return "", nil, "", c.Errf("unknown property '%s'", c.Val())
			}
		}

		origin := plugin.Host(args[0]).Normalize()

		endpoints, index, err := parseEndpointAndIndex(args[1], credential)
		if err != nil {
			return "", nil, "", err
		}

		tmpl := template.Must(template.New("nsid").Option("missingkey=error").Parse(args[2]))

		if origin == "" || len(endpoints) == 0 || tmpl == nil {
			return "", nil, "", c.Dispenser.ArgErr()
		}

		var data bytes.Buffer
		if err := tmpl.Execute(&data, map[string]string{"id": index}); err != nil {
			return "", nil, "", err
		}

		nsid := plugin.Name(data.String()).Normalize()

		return origin, endpoints, nsid, nil
	}
	return "", nil, "", c.Dispenser.ArgErr()
}

func parseEndpointAndIndex(arg string, credential *credentials.Credentials) ([]endpoint, string, error) {
	var endpoints []endpoint

	entries := map[string]struct{}{}
	if strings.HasPrefix(arg, "aws:ec2:Subnet") {
		privateIPAddress, err := ec2Metadata(ec2PrivateIPAddress)
		if err != nil {
			return nil, "", fmt.Errorf("invalid private ip: %s", err)
		}
		interfaces, err := net.Interfaces()
		if err != nil {
			return nil, "", fmt.Errorf("invalid interfaces: %s", err)
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
				id, err := ec2Metadata(ec2AmiLaunchIndex)
				if err != nil {
					return nil, "", fmt.Errorf("invalid launch index: %s", err)
				}
				return endpoints, id, nil
			}
		}

		return nil, "", fmt.Errorf("invalid subnet for %q", privateIPAddress)
	}
	if strings.HasPrefix(arg, "aws:autoscaling") {
		instanceID, err := ec2Metadata(ec2InstanceID)
		if err != nil {
			return nil, "", fmt.Errorf("invalid instance id: %s", err)
		}

		az, err := ec2Metadata(ec2AvailabilityZone)
		if err != nil {
			return nil, "", fmt.Errorf("invalide availability zone: %s", err)
		}
		if len(az) <= 1 {
			return nil, "", fmt.Errorf("invalide availability zone %q", az)
		}
		region := az[:len(az)-1]

		service := autoscaling.New(session.Must(session.NewSession(&aws.Config{
			Region:      aws.String(region),
			Credentials: credential,
		})))
		groups, err := service.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{})
		if err != nil {
			return nil, "", err
		}
		name := ""
		capacity := 0
		for _, group := range groups.AutoScalingGroups {
			for _, instance := range group.Instances {
				if aws.StringValue(instance.InstanceId) == instanceID {
					name = aws.StringValue(group.AutoScalingGroupName)
					capacity = int(aws.Int64Value(group.DesiredCapacity))
					break
				}
			}
			if name != "" {
				break
			}
		}
		if name == "" {
			return nil, "", fmt.Errorf("invalid instance id for autoscaling group %q", instanceID)
		}
		result, err := service.DescribeScalingActivities(&autoscaling.DescribeScalingActivitiesInput{
			AutoScalingGroupName: aws.String(name),
		})
		if err != nil {
			return nil, "", err
		}
		l := listInstances(result.Activities, capacity)
		id, ok := l[instanceID]
		if !ok {
			return nil, "", fmt.Errorf("unable to allocate a slot for autoscaling for %q", instanceID)
		}

		for i := 0; i < capacity; i++ {
			endpoints = append(endpoints, endpoint{
				addr: "",
				port: "",
				quit: make(chan struct{}),
			})
		}
		ec2service := ec2.New(session.Must(session.NewSession(&aws.Config{
			Region:      aws.String(region),
			Credentials: credential,
		})))

		port := "53"
		for v, i := range l {
			result, err := ec2service.DescribeInstances(&ec2.DescribeInstancesInput{
				Filters: []*ec2.Filter{
					{
						Name: aws.String("instance-id"),
						Values: []*string{
							aws.String(v),
						},
					},
				},
			})
			if err != nil {
				return nil, "", err
			}
			if len(result.Reservations) == 1 && len(result.Reservations[0].Instances) == 1 {
				endpoints[i].addr = aws.StringValue(result.Reservations[0].Instances[0].PrivateIpAddress)
				endpoints[i].port = port
			}
		}
		return endpoints, fmt.Sprintf("%d", id), nil
	}
	if strings.HasPrefix(arg, "aws:ec2:ReservationId") {
		reservationID, err := ec2Metadata(ec2ReservationID)
		if err != nil {
			return nil, "", fmt.Errorf("invalid reservation id: %s", err)
		}

		az, err := ec2Metadata(ec2AvailabilityZone)
		if err != nil {
			return nil, "", fmt.Errorf("invalide availability zone: %s", err)
		}
		if len(az) <= 1 {
			return nil, "", fmt.Errorf("invalide availability zone %q", az)
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
			return nil, "", err
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

		id, err := ec2Metadata(ec2AmiLaunchIndex)
		if err != nil {
			return nil, "", fmt.Errorf("invalid launch index: %s", err)
		}
		return endpoints, id, nil
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
			return nil, "", fmt.Errorf("expecting data after last colon: %q", s)
		}
		if colon != -1 {
			if p, err := strconv.Atoi(s[colon+1:]); err == nil {
				if p < 0 || p >= 65536 {
					return nil, "", fmt.Errorf("invalid port number: %q", s[colon+1:])
				}
				port = strconv.Itoa(p)
				host = s[:colon]
			}
		}

		slash := strings.LastIndex(host, "/")
		if slash == len(host)-1 {
			return nil, "", fmt.Errorf("expecting data after last slash: %q", host)
		}
		if slash != -1 {
			ip, ipnet, err := net.ParseCIDR(host)
			if err != nil {
				return nil, "", fmt.Errorf("invalid cidr block %q: %s", host, err)
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
			return nil, "", fmt.Errorf("invalid ip address: %q", host)
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
	id, err := ec2Metadata(ec2AmiLaunchIndex)
	if err != nil {
		return nil, "", fmt.Errorf("invalid launch index: %s", err)
	}
	return endpoints, id, nil
}

type byActivity []*autoscaling.Activity

func (s byActivity) Len() int {
	return len(s)
}
func (s byActivity) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s byActivity) Less(i, j int) bool {
	return s[i].EndTime.Before(*s[j].EndTime) || s[i].StartTime.Before(*s[j].StartTime) || aws.StringValue(s[i].ActivityId) < aws.StringValue(s[j].ActivityId)
}

func listInstances(activities []*autoscaling.Activity, capacity int) map[string]int {
	entries := activities[:0]
	for _, entry := range activities {
		if strings.HasPrefix(aws.StringValue(entry.Description), "Launching a new EC2 instance: ") || strings.HasPrefix(aws.StringValue(entry.Description), "Terminating EC2 instance: ") || aws.StringValue(entry.StatusCode) == "Successful" {
			entries = append(entries, entry)
		}
	}
	sort.Sort(byActivity(entries))
	slot := map[int]string{}
	for _, entry := range entries {
		if strings.HasPrefix(aws.StringValue(entry.Description), "Launching a new EC2 instance: ") {
			instanceID := aws.StringValue(entry.Description)[30:]
			id := -1
			for i := 0; i < capacity; i++ {
				if _, ok := slot[i]; !ok {
					id = i
					break
				}
			}
			if id < 0 {
				panic(fmt.Errorf("no open slot %s", instanceID))
			}
			slot[id] = instanceID
		} else if strings.HasPrefix(aws.StringValue(entry.Description), "Terminating EC2 instance: ") {
			instanceID := aws.StringValue(entry.Description)[26:]
			id := -1
			for i := 0; i < capacity; i++ {
				if v, ok := slot[i]; ok {
					if v == instanceID {
						id = i
					}
				}
			}
			if id < 0 {
				panic(fmt.Errorf("unknown id %s", instanceID))
			}
			delete(slot, id)
		}
	}
	instances := map[string]int{}
	for k, v := range slot {
		instances[v] = k
	}
	return instances
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
	ec2InstanceID       = "http://169.254.169.254/latest/meta-data/instance-id"
	ec2AvailabilityZone = "http://169.254.169.254/latest/meta-data/placement/availability-zone"
	ec2PrivateIPAddress = "http://169.254.169.254/latest/meta-data/local-ipv4"
)
