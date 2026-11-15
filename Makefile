# Release builds run in the same Debian bookworm toolchain image family CPA
# uses, so the .so links against a glibc no newer than the CPA runtime image.
VERSION := $(shell sed -n 's/^[[:space:]]*pluginVersion[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/plugin/plugin.go)
GOARCH ?= amd64
GO_IMAGE ?= golang:1.26-bookworm
CACHE ?= $(HOME)/.cache/cpa-analytics
DIST ?= $(CACHE)/dist
ARCHIVE := cpa-analytics_$(VERSION)_linux_$(GOARCH).zip

.PHONY: test build package e2e clean

test:
	go vet ./...
	go test -race ./...

# build compiles dist/cpa-analytics.so for linux/$(GOARCH) inside $(GO_IMAGE).
build:
	mkdir -p $(DIST) $(CACHE)/go-mod $(CACHE)/go-build
	docker run --rm --platform linux/$(GOARCH) --user $$(id -u):$$(id -g) \
		-v $(CURDIR):/src:ro -v $(DIST):/out \
		-v $(CACHE)/go-mod:/go-mod -v $(CACHE)/go-build:/go-build \
		-e GOMODCACHE=/go-mod -e GOCACHE=/go-build -e CGO_ENABLED=1 -e HOME=/tmp \
		-w /src $(GO_IMAGE) \
		go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w" -o /out/cpa-analytics.so .
	rm -f $(DIST)/cpa-analytics.h

# package writes the GitHub release assets the CPA plugin store expects:
# <id>_<version>_<goos>_<goarch>.zip with the library at its root, plus checksums.txt.
package: build
	cd $(DIST) && rm -f $(ARCHIVE) && zip -q -X $(ARCHIVE) cpa-analytics.so
	cd $(DIST) && sha256sum cpa-analytics_*.zip > checksums.txt
	@echo "release assets in $(DIST): $(ARCHIVE) checksums.txt"

e2e:
	./test/e2e/run.sh

clean:
	rm -rf $(DIST) $(CACHE)/build
