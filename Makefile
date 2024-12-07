IMAGE   := abali/janitor

VERSION := $(shell git describe --tags || echo "unknown")
BUILD   := $(shell date '+%FT%T%z')
COMMIT  := $(shell git describe --always --long)

build:
	@go build -ldflags="-X main.version=$(VERSION) -X main.build=$(BUILD) -X main.commit=$(COMMIT)"

static_build:
	@CGO_ENABLED=0 go build -ldflags="-X main.version=$(VERSION) -X main.build=$(BUILD) -X main.commit=$(COMMIT)"

.PHONY: docker
docker:
	docker build . -t $(IMAGE)

.PHONY: publish
publish:
	@docker buildx create --use --name=crossplat --node=crossplat && \
	docker buildx build \
		--platform linux/amd64,linux/arm/v6,linux/arm/v7,linux/arm64 \
		--output "type=image,push=true" \
		--tag $(IMAGE):$(VERSION) \
		--tag $(IMAGE):latest \
		.

.PHONY: publish-dev
publish-dev:
	@docker buildx create --use --name=crossplat --node=crossplat && \
	docker buildx build \
		--platform linux/amd64,linux/arm/v6,linux/arm/v7,linux/arm64 \
		--output "type=image,push=true" \
		--tag $(IMAGE):dev \
		.
