BIN ?= ./bin
OUTPUT_BIN := $(abspath $(BIN))
BUILD_DIR ?= .build/site-server
SITE_OVERRIDES ?=
COMMAND_DIR ?= cmd/artisan-webserver
GO ?= go
GOFLAGS ?= -buildvcs=false
COVERAGE_MIN ?= 70
COVERAGE_DIR ?= .build/coverage
COVERAGE_PROFILE ?= $(COVERAGE_DIR)/coverage.out
COVERAGE_HTML ?= $(COVERAGE_DIR)/coverage.html

.PHONY: build run clean-build test test-fast test-race test-integration test-cover vet

ifeq ($(strip $(SITE_OVERRIDES)),)
build:
	GOFLAGS="$(GOFLAGS)" $(GO) build -o "$(OUTPUT_BIN)" ./$(COMMAND_DIR)
else
build: clean-build
	mkdir -p "$(BUILD_DIR)/$(COMMAND_DIR)"
	cp go.mod go.sum "$(BUILD_DIR)/"
	cp "$(COMMAND_DIR)"/*.go "$(BUILD_DIR)/$(COMMAND_DIR)/"
	cp -R theme "$(BUILD_DIR)/"
	cp -R "$(SITE_OVERRIDES)"/. "$(BUILD_DIR)/$(COMMAND_DIR)/"
	cd "$(BUILD_DIR)" && GOFLAGS="$(GOFLAGS)" $(GO) build -o "$(OUTPUT_BIN)" ./$(COMMAND_DIR)
endif

run: build
	$(BIN)

clean-build:
	rm -rf "$(BUILD_DIR)"

test: vet test-fast test-race test-integration test-cover

test-fast:
	GOFLAGS="$(GOFLAGS)" $(GO) test ./...

test-race:
	GOFLAGS="$(GOFLAGS)" $(GO) test -race ./...

test-integration:
	GOFLAGS="$(GOFLAGS)" $(GO) test -tags=integration -run '^TestServerProcess$$' ./$(COMMAND_DIR)

test-cover:
	mkdir -p "$(COVERAGE_DIR)"
	GOFLAGS="$(GOFLAGS)" $(GO) test -coverprofile="$(COVERAGE_PROFILE)" ./...
	GOFLAGS="$(GOFLAGS)" $(GO) tool cover -func="$(COVERAGE_PROFILE)"
	GOFLAGS="$(GOFLAGS)" $(GO) tool cover -html="$(COVERAGE_PROFILE)" -o "$(COVERAGE_HTML)"
	@coverage="$$( GOFLAGS="$(GOFLAGS)" $(GO) tool cover -func="$(COVERAGE_PROFILE)" | awk '/^total:/ {gsub(/%/, "", $$3); print $$3}' )"; \
	awk -v coverage="$$coverage" -v minimum="$(COVERAGE_MIN)" 'BEGIN { if (coverage + 0 < minimum + 0) exit 1 }' || { \
		echo "coverage $$coverage% is below required $(COVERAGE_MIN)%"; exit 1; \
	}; \
	echo "coverage $$coverage% meets required $(COVERAGE_MIN)%"

vet:
	GOFLAGS="$(GOFLAGS)" $(GO) vet ./...
