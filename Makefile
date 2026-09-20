PLUGIN_NAME := local-llm-pool
BIN_DIR := bin
DIST_DIR := dist

# VERSION is stamped into the plugin's registration metadata. Release builds
# pass the tag without its leading v; local builds keep the -dev default so a
# hand-built library is never mistaken for a published one.
VERSION ?= 0.1.0-dev

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

ifeq ($(GOOS),windows)
LIB_EXT := .dll
else ifeq ($(GOOS),darwin)
LIB_EXT := .dylib
else
LIB_EXT := .so
endif

PLUGIN := $(BIN_DIR)/$(PLUGIN_NAME)$(LIB_EXT)
LDFLAGS := -X main.pluginVersion=$(VERSION)

.PHONY: build test vet package verify-package clean

# build produces the c-shared plugin and discards the cgo-generated header,
# which the host never reads and which must not reach the release archive.
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o $(PLUGIN) .
	@rm -f $(BIN_DIR)/$(PLUGIN_NAME).h

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

# package builds the release archive for the current platform, using the same
# code the release workflow runs, so the layout can be checked locally.
package: build
	go run ./tools/pkgzip pack \
		-id $(PLUGIN_NAME) -version $(VERSION) \
		-goos $(GOOS) -goarch $(GOARCH) \
		-lib $(PLUGIN) -out $(DIST_DIR)
	go run ./tools/pkgzip checksums -dir $(DIST_DIR)

# verify-package re-opens the built archive and asserts the store's layout
# rules: exactly one entry, at the root, correctly named, checksum matching.
verify-package:
	go run ./tools/pkgzip verify \
		-dir $(DIST_DIR) -id $(PLUGIN_NAME) -version $(VERSION) \
		-goos $(GOOS) -goarch $(GOARCH)

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
