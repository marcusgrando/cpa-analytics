# Release libraries are built natively per platform: linux/amd64 inside a
# Debian bookworm Go image, so the .so needs no newer glibc than the CPA image
# ships, and darwin/arm64 on a macOS host. cgo rules out cross-compiling.
VERSION := $(shell sed -n 's/^[[:space:]]*pluginVersion[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/plugin/plugin.go)
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
EXT := $(if $(filter darwin,$(GOOS)),dylib,so)
GO_IMAGE ?= golang:1.26-bookworm
CACHE ?= $(HOME)/.cache/cpa-analytics
DIST ?= $(CACHE)/dist

.PHONY: version test lib archive build package e2e clean

version:
	@echo $(VERSION)

test:
	go vet ./...
	go test -race ./...

# lib builds the plugin for the host platform: cpa-analytics.so on Linux,
# cpa-analytics.dylib on macOS, which is what CPA looks for.
lib:
	mkdir -p $(DIST)
	CGO_ENABLED=1 go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o $(DIST)/cpa-analytics.$(EXT) .
	rm -f $(DIST)/cpa-analytics.h

# archive writes the asset the CPA plugin store installs from:
# cpa-analytics_<version>_<goos>_<goarch>.zip with the library at its root.
archive:
	cd $(DIST) && rm -f cpa-analytics_$(VERSION)_$(GOOS)_$(GOARCH).zip && zip -q -X cpa-analytics_$(VERSION)_$(GOOS)_$(GOARCH).zip cpa-analytics.$(EXT)

# build runs lib for linux/amd64 inside $(GO_IMAGE), from any host with Docker.
build:
	mkdir -p $(DIST) $(CACHE)/go-mod $(CACHE)/go-build
	docker run --rm --platform linux/amd64 --user $$(id -u):$$(id -g) \
		-v $(CURDIR):/src:ro -v $(DIST):/out \
		-v $(CACHE)/go-mod:/go-mod -v $(CACHE)/go-build:/go-build \
		-e GOMODCACHE=/go-mod -e GOCACHE=/go-build -e CGO_ENABLED=1 -e HOME=/tmp \
		-w /src $(GO_IMAGE) \
		go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o /out/cpa-analytics.so .
	rm -f $(DIST)/cpa-analytics.h

# package builds the linux/amd64 release asset locally. Tagged releases are
# built by .github/workflows/release.yml for every platform.
package: build
	cd $(DIST) && rm -f cpa-analytics_$(VERSION)_linux_amd64.zip && zip -q -X cpa-analytics_$(VERSION)_linux_amd64.zip cpa-analytics.so
	cd $(DIST) && sha256sum cpa-analytics_*.zip > checksums.txt
	@echo "release assets in $(DIST)"

e2e:
	./test/e2e/run.sh

clean:
	rm -rf $(DIST) $(CACHE)/build
