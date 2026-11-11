# shiga builds as a static binary: CGO_ENABLED=0, no libc dependency.
#
#   make            vet, test and build bin/shiga for this machine
#   make dist       bin/shiga-linux-amd64 and bin/shiga-linux-arm64
#   make VERSION=v0.1.0 dist
#   make apicompat  the public API against the latest release (apidiff)
#
# VERSION defaults to `git describe`; shiga version prints it.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)'
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all build dist test vet apicompat clean

all: vet test build

build:
	$(GOBUILD) -o bin/shiga ./cmd/shiga

dist: $(foreach p,$(PLATFORMS),bin/shiga-$(subst /,-,$(p)))

bin/shiga-%: FORCE
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) $(GOBUILD) -o $@ ./cmd/shiga

test:
	go test ./...

vet:
	go vet ./...
	GOARCH=386 go vet ./...

apicompat:
	.github/scripts/apicompat.sh

clean:
	rm -rf bin

.PHONY: FORCE
FORCE:
