BIN_NAME := evpn-agent
KIND_CLUSTER ?= evpn

# evpn-agent image
AGENT_IMAGE_REPO ?= hostfw-local.artifactory-espoo1.int.net.nokia.com/release/evpn-agent
AGENT_IMAGE_TAG  ?= dev
AGENT_IMAGE      := $(AGENT_IMAGE_REPO):$(AGENT_IMAGE_TAG)

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/$(BIN_NAME) ./cmd/$(BIN_NAME)

.PHONY: docker docker-agent build-agent-image load-agent push-agent
# Default: build and load agent image into kind
docker: docker-agent

docker-agent: build-agent-image load-agent

build-agent-image:
	docker build -t $(AGENT_IMAGE) -f dockerfile/evpn-agent/Dockerfile .

push-agent:
	docker push $(AGENT_IMAGE)

load-agent:
	kind load docker-image --name $(KIND_CLUSTER) $(AGENT_IMAGE)

.PHONY: helm-install
helm-install:
	helm upgrade --install evpn-agent ./charts/evpn-agent -n evpnlab --create-namespace
