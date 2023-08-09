DEFAULT_GOAL := build
IMAGE ?= abali/janitor:latest

build:
	go build -o janitor

.PHONY: docker
docker:
	docker build . -t $(IMAGE)

.PHONY: publish 
publish:
	@docker buildx create --use --name=crossplat --node=crossplat && \
	docker buildx build \
		--platform linux/amd64,linux/arm/v6,linux/arm/v7,linux/arm64 \
		--output "type=image,push=true" \
		--tag $(IMAGE) \
		.
