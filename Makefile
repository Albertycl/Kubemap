GO      ?= go
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X kubemap/pkg/version.Version=$(VERSION)
# Image with the server and the node agent. Local by default; the registry is GitLab (see "push").
IMAGE   ?= localhost/kubemap

.PHONY: all build agent web web-build portal test vet fmt fmt-check image manifest bundle push smoke release clean

all: fmt-check vet test build

build: agent web portal

## Node agent binary (runs in the container image)
agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/kubemap-agent ./cmd/kubemap-agent

## Portal web UI (React + TypeScript, A2): type-check and build into web/dist,
## which the portal embeds. Needs Node.js and npm. A checkout without the UI
## sources skips it; the portal then shows a "UI not built" page, and
## "make release" refuses to run.
web:
	@if [ -f web/package.json ]; then $(MAKE) --no-print-directory web-build; \
	else echo "web: no web UI sources (web/package.json) in this checkout; skipped"; fi

web-build: web/node_modules/.installed
	cd web && npm test && npm run build

web/node_modules/.installed: web/package.json web/package-lock.json
	cd web && npm ci --no-audit --no-fund --ignore-scripts
	touch $@

## Server binary (web UI embedded; build "web" first)
portal:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/kubemap-portal ./cmd/kubemap-portal

test:
	$(GO) test ./...
	deploy/uninstall_test.sh

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "unformatted files:"; gofmt -l .; exit 1)

## Container image with the server and the node agent (deploy/Dockerfile).
## The web UI is built first because the server embeds web/dist.
image: web
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -f deploy/Dockerfile .

## deploy/kubemap.yaml, the install manifest for the default image. A test in
## agent/install fails while it is out of date.
manifest:
	$(GO) test ./agent/install -run TestGolden -update

## The install manifest for this version on stdout (bin/kubemap-<version>.yaml)
bundle:
	@mkdir -p bin
	$(GO) run ./cmd/kubemap-agent bundle --image $(IMAGE):$(VERSION) > bin/kubemap-$(VERSION).yaml

## Push the image to the GitLab registry. Refused until IMAGE points
## there, because the account is not decided yet. Log in first with
## "docker login registry.gitlab.com"; no credentials live in this repo.
push:
	@case "$(IMAGE)" in \
	  *'<'*|*'>'*) echo "make push: replace <account> in IMAGE=$(IMAGE)"; exit 1;; \
	  registry.gitlab.com/?*/?*) ;; \
	  *) echo "make push: IMAGE=$(IMAGE) is not a GitLab registry image."; \
	     echo "Set IMAGE=registry.gitlab.com/<account>/kubemap (the account is not decided yet),"; \
	     echo "log in with: docker login registry.gitlab.com, then run: make push IMAGE=... VERSION=..."; exit 1;; \
	esac
	docker push $(IMAGE):$(VERSION)

## Check that both binaries run and agree on the version (scripts/smoke.sh)
smoke: agent portal
	scripts/smoke.sh bin/kubemap-portal bin/kubemap-agent $(VERSION)

## Everything for an install, into dist/: make release VERSION=x.y.z
## (scripts/release.sh). SKIP_IMAGE=1 leaves the image out.
release:
	@VERSION="$(VERSION)" IMAGE="$(IMAGE)" GO="$(GO)" MAKE="$(MAKE)" scripts/release.sh

clean:
	rm -rf bin dist
	@if [ -d web/dist ]; then find web/dist -mindepth 1 ! -name .gitkeep -delete; fi
