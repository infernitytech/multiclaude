IMAGE ?= multiclaude:latest
GO ?= go

.PHONY: build runners image image-tar dev clean

build:
	$(GO) build -o bin/ ./cmd/...

# Cross-compile runner binaries into dist/ (served by `make dev`)
runners:
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags="-s -w" -o dist/multiclaude-runner-$$os-$$arch ./cmd/multiclaude-runner; \
	done

# Build the container image (docker or podman)
image:
	$$(command -v docker || command -v podman) build -t $(IMAGE) .

# Export the image as a tarball to load on the NAS: `docker load -i multiclaude.tar`
image-tar: image
	$$(command -v docker || command -v podman) save -o multiclaude.tar $(IMAGE)

dev: build runners
	./bin/multiclaude-server --data ./data --downloads ./dist

clean:
	rm -rf bin dist multiclaude.tar
