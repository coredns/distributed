# Makefile for building CoreDNS
GITCOMMIT:=$(shell git describe --dirty --always)
BINARY:=coredns
SYSTEM:=

all: coredns

coredns: check godep
	$(SYSTEM) go build -v -ldflags="-s -w -X github.com/coredns/coredns/coremain.gitCommit=$(GITCOMMIT)" -o $(BINARY) coremain/coredns.go

check:
	go get -v -u github.com/alecthomas/gometalinter
	gometalinter --install golint
	gometalinter --deadline=1m --disable-all --enable=gofmt --enable=golint --enable=vet --exclude=^vendor/ --exclude=^pb/ ./...

godep:
	go get -v ...

clean:
	go clean
	rm -f coredns

.PHONY: coredns check godep clean
