GO ?= go
VERSION ?= dev
OUT ?= generated-artifacts
# Pinned staticcheck version supporting the Go 1.25 language.
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@v0.7.0
LDFLAGS = -s -w -X main.version=$(VERSION)

.PHONY: build test lint fmt check-format cross-compile check clean
build:
	@mkdir -p $(OUT)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/eks ./cmd/eks

test:
	$(GO) test -race ./...

fmt:
	gofmt -w cmd internal

check-format:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)

lint: check-format
	$(GO) vet ./...
	$(STATICCHECK) ./...

cross-compile:
	@mkdir -p $(OUT)
	@set -e; for os in linux darwin windows; do \
		for arch in amd64 arm64; do \
			suffix=""; if [ "$$os" = windows ]; then suffix=".exe"; fi; \
			echo "Building $$os/$$arch"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
				-ldflags "$(LDFLAGS)" -o "$(OUT)/eks-$$os-$$arch$$suffix" ./cmd/eks; \
		done; \
	done

check: test lint cross-compile

clean:
	rm -rf generated-artifacts
