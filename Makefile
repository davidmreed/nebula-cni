BUILD_NUMBER ?= "$(shell git describe --tags --always)"
export CGO_ENABLED=0

LDFLAGS = -X main.Build=$(BUILD_NUMBER)

all: nebula-cni-plugin nebula-cni-instance nebula-cni-init nebula-cni-install

nebula-cni-plugin:
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/nebula-cni-plugin

nebula-cni-instance:
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/nebula-cni-instance

nebula-cni-init:
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/nebula-cni-init

nebula-cni-install:
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/nebula-cni-install

.PHONY: all nebula-cni-plugin nebula-cni-instance nebula-cni-init nebula-cni-install
