# Fully qualified so podman-built images load into Docker as plain "multiclaude:latest".
IMAGE ?= docker.io/library/multiclaude:latest
GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X multiclaude/internal/proto.Version=$(VERSION)

.PHONY: build runners image image-tar serve-image deploy-nas release dev clean

build:
	$(GO) build -ldflags="$(LDFLAGS)" -o bin/ ./cmd/...

# Cross-compile runner binaries into dist/ (served by `make dev`)
runners:
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/multiclaude-runner-$$os-$$arch ./cmd/multiclaude-runner; \
	done

# Build the container image (docker or podman)
image:
	$$(command -v docker || command -v podman) build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Publish a release: make release V=v0.2.0 → GitHub builds ghcr.io/…/multiclaude:0.2.0 + :latest
release:
	@echo "$(V)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || (echo "usage: make release V=v1.2.3" >&2; exit 1)
	@test -z "$$(git status --porcelain)" || (echo "commit your changes first" >&2; exit 1)
	git tag -a $(V) -m "MultiClaude $(V)" && git push origin main $(V)

# Export the image as a tarball to load on the NAS: `docker load -i multiclaude.tar`
image-tar: image
	$$(command -v docker || command -v podman) save -o multiclaude.tar $(IMAGE)

# Without SSH: serve the image from this computer and load it from the
# TrueNAS web shell (System → Shell). Prints the command to paste there.
PORT ?= 8000
serve-image: image
	@rm -rf .serve && mkdir .serve && $$(command -v docker || command -v podman) save -o .serve/multiclaude.tar $(IMAGE)
	@ip=$$(hostname -I 2>/dev/null | awk '{print $$1}'); [ -n "$$ip" ] || ip=$$(ipconfig getifaddr en0 2>/dev/null); \
	echo; echo "Paste this into TrueNAS → System → Shell (Ctrl+C here when done):"; echo; \
	echo "  curl -fL http://$$ip:$(PORT)/multiclaude.tar -o /tmp/mc.tar && img=\$$(sudo docker load -q -i /tmp/mc.tar | awk '{print \$$NF}') && sudo docker tag \$$img multiclaude:latest && rm /tmp/mc.tar && sudo docker images multiclaude"; echo; \
	python3 -m http.server $(PORT) --bind 0.0.0.0 --directory .serve 2>&1 | grep --line-buffered multiclaude.tar

# With SSH enabled on the NAS: make deploy-nas NAS=truenas_admin@192.168.1.10
deploy-nas: image
	@test -n "$(NAS)" || (echo "usage: make deploy-nas NAS=user@nas-ip" >&2; exit 1)
	$$(command -v docker || command -v podman) save $(IMAGE) | ssh -o ConnectTimeout=10 $(NAS) "sudo docker load"
	@echo "Image loaded on $(NAS). In TrueNAS: Apps → multiclaude → Restart."

dev: build runners
	./bin/multiclaude-server --data ./data --downloads ./dist

clean:
	rm -rf bin dist multiclaude.tar .serve
