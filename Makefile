BINARY  := check-puma
PROJECT := sensu-plugins-puma
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always 2>/dev/null || echo dev))
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
ARCHS   := amd64
REPO    := opsone/sensu-plugins-puma
ASSET_YML := dist/$(subst /,-,$(REPO))_$(VERSION).yml

LDFLAGS := -s -w \
	-X github.com/sensu/sensu-plugin-sdk/version.version=$(VERSION) \
	-X github.com/sensu/sensu-plugin-sdk/version.commit=$(COMMIT) \
	-X github.com/sensu/sensu-plugin-sdk/version.date=$(DATE)

.PHONY: test build linux integration dist release clean

test:
	go vet ./...
	go test ./...

build:
	go build -o $(BINARY) .

linux:
	GOOS=linux GOARCH=$(shell go env GOARCH) CGO_ENABLED=0 go build -o integration/$(BINARY) .

integration: linux
	./integration/run.sh

# Sensu assets: one tar.gz per arch with bin/check-puma, plus a sha512 checksums file.
dist: test
	rm -rf dist
	set -e; for arch in $(ARCHS); do \
		dir=dist/$(PROJECT)_$(VERSION)_linux_$$arch; \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/bin/$(BINARY) .; \
		cp README.md CHANGELOG.md $$dir/; \
		COPYFILE_DISABLE=1 tar -czf $$dir.tar.gz -C $$dir bin README.md CHANGELOG.md; \
		rm -rf $$dir; \
	done
	cd dist && shasum -a 512 *.tar.gz > $(PROJECT)_$(VERSION)_sha512-checksums.txt
	./scripts/asset-yml.sh $(REPO) https://github.com/$(REPO)/releases/download/$(VERSION) dist/$(PROJECT)_$(VERSION)_sha512-checksums.txt > $(ASSET_YML)
	@ls -l dist

# GitHub release of the tag, with the asset definition as description.
release: dist
	{ echo '```yaml'; cat $(ASSET_YML); echo '```'; } | \
		gh release create $(VERSION) --repo $(REPO) --title $(VERSION) --notes-file - \
		dist/*.tar.gz dist/*_sha512-checksums.txt

clean:
	rm -f $(BINARY) integration/$(BINARY)
	rm -rf dist
