# wg-user build orchestration
# Use: make build / make test / make clean / make package / make verify

SHELL := /bin/bash
ROOT_DIR := $(shell pwd)
SCRIPTS  := $(ROOT_DIR)/scripts
BIN      := $(ROOT_DIR)/bin
DIST     := $(ROOT_DIR)/dist
VERSION  := $(shell sed -n 's/^version=//p' $(ROOT_DIR)/module.prop)

ARCHS := arm64-v8a armeabi-v7a x86_64 x86
GOWBINS := $(addprefix $(BIN)/,$(addsuffix /wg-user,$(ARCHS)))
WEBUI_DIR := $(ROOT_DIR)/webui
WEBROOT := $(ROOT_DIR)/webroot

# Files that make up the distributable module zip.
# NOTE: etc/wireproxy.conf and etc/tunnel.conf* are deliberately absent —
# they hold real keys. customize.sh seeds them from the example (or keeps
# the copy already on the device).
PKG_FILES := module.prop customize.sh uninstall.sh sepolicy.rule service.sh wg-user common/ etc/wireproxy.conf.example webroot/ bin/arm64-v8a bin/armeabi-v7a bin/x86_64 bin/x86 README.md LICENSE docs/INSTALL.md docs/CONFIG.md docs/TUNNEL-SERVER.md docs/CHANGELOG.md docs/ARCHITECTURE.md

.PHONY: build build-go test webui webui-clean clean check-bins package package-only verify interop help FORCE

help:
	@echo "make build      - cross-compile the single wg-user binary for 4 arches"
	@echo "make build-go   - same as build (kept for CI compatibility)"
	@echo "make test       - go vet + go test"
	@echo "make webui      - rebuild the Preact+Tailwind KSU WebUI (needs node)"
	@echo "make clean      - remove build artifacts"
	@echo "make package    - zip the module for distribution (after build)"
	@echo "make verify     - shell syntax checks (+ CLI/install tests when present)"
	@echo "make interop    - run the tests against the real wstunnel v11 server (needs $(BIN)/interop/wstunnel)"

build: build-go

build-go: $(GOWBINS)
	@sh $(ROOT_DIR)/test/shell/build_test.sh

# One cross-compile per arch. build.sh loops over all four when ARCH is unset;
# the pattern rule builds just the arch it was asked for.
#
# FORCE: Go does its own dependency tracking inside the build cache, and make
# cannot see the package graph, so every build re-runs the cross-compile
# instead of calling a stale bin/<arch>/wg-user up to date.
$(BIN)/%/wg-user: FORCE $(SCRIPTS)/build.sh
	ARCH=$* bash $(SCRIPTS)/build.sh

test:
	go vet ./... && go test ./...

webui:
	@echo "==> building Preact + TypeScript + Tailwind WebUI -> $(WEBROOT)..."
	@cd $(WEBUI_DIR) && pnpm install --frozen-lockfile
	@cd $(WEBUI_DIR) && pnpm run check

webui-clean:
	rm -f $(WEBROOT)/app.js $(WEBROOT)/index.html $(WEBROOT)/style.css

# NOTE: this deliberately does not run webui-clean. webroot/app.js, index.html
# and style.css are tracked in git and shipped in the module zip, and make build
# does not regenerate them, so wiping them here would record three deletions of
# files the module cannot do without. Rebuild the bundle explicitly with
# `make webui` (or `make webui-clean` to drop it on purpose).
clean:
	rm -rf $(BIN)/*/wg-user $(BIN)/interop $(DIST)

# zip exits 0 when a named entry is missing, it just leaves it out — so a
# package run after `make clean` would "succeed" with empty bin/<arch>/ dirs.
# Refuse to build an archive unless all four cross-compiles are present.
check-bins:
	@missing=""; \
	for b in $(GOWBINS); do \
	    if [ ! -f "$$b" ] || [ ! -x "$$b" ]; then missing="$$missing $$b"; fi; \
	done; \
	if [ -n "$$missing" ]; then \
	    echo "ERROR: cannot package, missing wg-user binaries:$$missing"; \
	    echo "       run 'make build' first (or 'make package', which builds)."; \
	    exit 1; \
	fi

package: build check-bins webui
	@echo "==> packaging wg-user v$(VERSION)..."
	@mkdir -p $(DIST)
	@cd $(ROOT_DIR) && zip -r $(DIST)/wg-user-$(VERSION).zip $(PKG_FILES) 2>/dev/null || \
	  (echo "zip not found, trying tar:" && tar czf $(DIST)/wg-user-$(VERSION).tar.gz $(PKG_FILES))
	@ls -lh $(DIST)

# Same archive, but without re-running the cross-compiles. Use after
# touching only webui/ or docs/.
package-only: check-bins webui
	@echo "==> packaging wg-user v$(VERSION) (no rebuild)..."
	@mkdir -p $(DIST)
	@cd $(ROOT_DIR) && zip -r $(DIST)/wg-user-$(VERSION).zip $(PKG_FILES) 2>/dev/null || \
	  (echo "zip not found, trying tar:" && tar czf $(DIST)/wg-user-$(VERSION).tar.gz $(PKG_FILES))
	@ls -lh $(DIST)

# No Go toolchain and no built binaries required: syntax-check every script
# with the interpreter named in its shebang, then run the shell suites that
# Tasks 11 and 12 add (skipped while they do not exist yet).
verify:
	@echo "==> syntax checks (interpreter from the shebang)..."
	@ok=0; fail=0; \
	for f in customize.sh uninstall.sh service.sh wg-user scripts/*.sh common/*.sh test/shell/*.sh; do \
	    [ -f "$$f" ] || continue; \
	    interp=sh; \
	    if head -n 1 "$$f" | grep -q bash; then interp=bash; fi; \
	    if command -v $$interp >/dev/null 2>&1 && $$interp -n "$$f" 2>/dev/null; then \
	        echo "  ok: $$f ($$interp)"; ok=$$((ok+1)); \
	    else \
	        echo "  FAIL: $$f ($$interp)"; $$interp -n "$$f"; fail=$$((fail+1)); \
	    fi; \
	done; \
	if [ $$fail -gt 0 ]; then echo "$$fail file(s) failed syntax"; exit 1; fi
	@echo "==> shell tests..."
	@for t in test/shell/cli_test.sh test/shell/install_test.sh; do \
	    if [ -f "$$t" ]; then echo "  running $$t"; sh "$$t" || exit 1; fi; \
	done

# Interoperability tests against the official wstunnel v11 server. The binary is
# scratch (bin/interop/ is git-ignored); fetch it with the commands printed when
# it is missing. Pass WSTUNNEL_SERVER_BIN to use a different build.
#
# Fetch commands are inline (not a download target) because this is a manual,
# local-only step. Note the asset name: v11.0.0 names its Linux x86_64 build
# wstunnel_11.0.0_linux_amd64.tar.gz (the older wstunnel-v11.0.0-x86_64-... form
# 404s), the tarball stores the binary without the exec bit (hence chmod), and
# the checksum is the verified SHA-256 of that tarball.
#
# -count=1 is required: the test cache does not see the server binary (it is
# exec'd, not read through the instrumented file APIs) and WSTUNNEL_SERVER_BIN
# keeps the same path when the pinned server is swapped, so a cached PASS would
# defeat exactly the drift check this target exists to be.
interop:
	@if [ ! -x "$${WSTUNNEL_SERVER_BIN:-$(BIN)/interop/wstunnel}" ]; then \
	    echo "missing wstunnel v11.0.0 server binary ($${WSTUNNEL_SERVER_BIN:-$(BIN)/interop/wstunnel}); fetch it with:"; \
	    echo "  mkdir -p $(BIN)/interop && curl -fsSL -o /tmp/wstunnel.tar.gz \\"; \
	    echo "    https://github.com/erebe/wstunnel/releases/download/v11.0.0/wstunnel_11.0.0_linux_amd64.tar.gz"; \
	    echo "  echo '9708a99717b5a951453c2ff7c14c25d3418d02ca7fcb96fdb382a8f2083bab5e  /tmp/wstunnel.tar.gz' | sha256sum -c -"; \
	    echo "  tar -xzf /tmp/wstunnel.tar.gz -C $(BIN)/interop wstunnel && chmod +x $(BIN)/interop/wstunnel"; \
	    exit 1; \
	fi
	@WSTUNNEL_SERVER_BIN=$${WSTUNNEL_SERVER_BIN:-$(BIN)/interop/wstunnel} \
	    go test -tags interop ./internal/wstunnel -run Interop -v -count=1
