# Makefile for building CoreDNS
GITCOMMIT:=$(shell git describe --dirty --always)
VERSION:=0.1.2
BINARY:=coredns
SYSTEM:=

all: coredns

coredns: check godep
	CGO_ENABLED=0 $(SYSTEM) go build -v -ldflags="-s -w -X github.com/coredns/coredns/coremain.gitCommit=$(GITCOMMIT)" -o $(BINARY) coremain/coredns.go

release: coredns
	@rm -rf release && mkdir release
	tar -zcf release/coredns_$(VERSION)_linux_amd64.tgz $(BINARY)

check:
	go get -v -u github.com/alecthomas/gometalinter
	gometalinter --install golint
	gometalinter --deadline=1m --disable-all --enable=gofmt --enable=golint --enable=vet --exclude=^vendor/ --exclude=^pb/ ./...

godep:
	go get -v ...

clean:
	go clean
	rm -f coredns

.PHONY: check godep clean
