ROOTDIR := $(shell pwd)
OUTPUT_DIR ?= $(ROOTDIR)/_output

IMAGE_REPO ?= 707907454361.dkr.ecr.eu-central-1.amazonaws.com
VERSION ?= latest
CLUSTER_NAME ?= kind

.PHONY: clean
clean:
	rm -rf _output

.PHONY: build
build: clean
	GOARCH=amd64 GOOS=linux go build -o $(OUTPUT_DIR)/flagger cmd/flagger/main.go 

.PHONY: docker
docker: build
	docker build -t $(IMAGE_REPO)/flagger:$(VERSION) .

.PHONY: push-kind-images
push-kind-images: docker
	kind load docker-image $(IMAGE_REPO)/flagger:$(VERSION) --name $(CLUSTER_NAME)
