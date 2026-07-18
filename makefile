BIN ?= ./bin
BUILD_DIR ?= .build/site-server
SITE_OVERRIDES ?=

.PHONY: build run clean-build

ifeq ($(strip $(SITE_OVERRIDES)),)
build:
	go build -o $(BIN)
else
build: clean-build
	mkdir -p "$(BUILD_DIR)"
	cp go.mod go.sum "$(BUILD_DIR)/"
	cp *.go "$(BUILD_DIR)/"
	cp -R theme "$(BUILD_DIR)/"
	cp -R "$(SITE_OVERRIDES)"/. "$(BUILD_DIR)/"
	cd "$(BUILD_DIR)" && go build -o "$(CURDIR)/$(BIN)"
endif

run: build
	$(BIN)

clean-build:
	rm -rf "$(BUILD_DIR)"
