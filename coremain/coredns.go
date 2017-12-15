package main

import (
	_ "github.com/coredns/distributed"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/coremain"
)

var directives = []string{
	"tls",
	"nsid",
	"root",
	"bind",
	"debug",
	"trace",
	"health",
	"pprof",
	"prometheus",
	"errors",
	"log",
	"dnstap",
	"chaos",
	"cache",
	"rewrite",
	"loadbalance",
	"dnssec",
	"autopath",
	"reverse",
	"hosts",
	"federation",
	"kubernetes",
	"file",
	"auto",
	"secondary",
	"etcd",
	"proxy",
	"erratic",
	"distributed",
	"whoami",
	"startup",
	"shutdown",
}

func init() {
	dnsserver.Directives = directives
}

func main() {
	coremain.Run()
}
