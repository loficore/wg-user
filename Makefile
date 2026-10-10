# wg-user build orchestration
# Use: make build / make build-go / make build-rust / make clean / make package / make verify

SHELL := /bin/bash
ROOT_DIR := $(shell pwd)
SCRIPTS  := $(ROOT_DIR)/scripts
BIN      := $(ROOT_DIR)/bin
DIST     := $(ROOT_DIR)/dist
VERSION  := $(shell grep '^version=' $(ROOT_DIR)/module.prop | cut -d= -f2)

ARCHS := arm64-v8a armeabi-v7a x86_64 x86
WPBINS := $(addprefix $(BIN)/,$(addsuffix /wireproxy,$(ARCHS)))
TNBINS := $(addprefix $(BIN)/,$(addsuffix /wstunnel,$(ARCHS)))

.PHONY: build build-go build-rust clean package verify help

help:
	@echo "make build      - cross-compile wireproxy (Go) + wstunnel (Rust)"
	@echo "make build-go   - only wireproxy (faster, ~3 min)"
	@echo "make build-rust - only wstunnel (~5 min; needs NDK + rustup targets)"
	@echo "make clean      - remove build artifacts"
	@echo "make package    - zip the module for distribution (after build)"
	@echo "make verify     - run syntax/static checks and configtest"

build: build-go build-rust

build-go: $(WPBINS)

build-rust: $(TNBINS)

$(BIN)/%/wireproxy:
	GOPROXY=$${GOPROXY:-https://goproxy.cn,direct} \
	WIREPROXY_VERSION=$${WIREPROXY_VERSION:-v1.1.3} \
	    bash $(SCRIPTS)/build.sh

$(BIN)/%/wstunnel:
	ANDROID_NDK_HOME=$${ANDROID_NDK_HOME:-$$HOME/Android/Sdk/ndk/26.3.11579264} \
	WSTUNNEL_VERSION=$${WSTUNNEL_VERSION:-v11.0.0} \
	    bash $(SCRIPTS)/cargo-cross.sh

clean:
	rm -rf $(BIN)/*/wireproxy $(BIN)/*/wstunnel
	rm -rf $(DIST)

package: build
	@echo "==> packaging wg-user v$(VERSION)..."
	@mkdir -p $(DIST)
	@cd $(ROOT_DIR) && zip -r $(DIST)/wg-user-$(VERSION).zip \
	    module.prop customize.sh install.sh uninstall.sh sepolicy.rule \
	    service.sh wg-user common/ etc/ webroot/ docs/ \
	    bin/arm64-v8a bin/armeabi-v7a bin/x86_64 bin/x86 \
	    scripts/cargo-wrappers/ \
	    README.md LICENSE 2>/dev/null || \
	  (echo "zip not found, trying tar:" && tar czf $(DIST)/wg-user-$(VERSION).tar.gz \
	    module.prop customize.sh install.sh uninstall.sh sepolicy.rule \
	    service.sh wg-user common/ etc/ webroot/ docs/ \
	    bin/arm64-v8a bin/armeabi-v7a bin/x86_64 bin/x86 \
	    scripts/cargo-wrappers/ \
	    README.md LICENSE)
	@ls -lh $(DIST)

verify:
	@echo "==> sh -n syntax checks..."
	@ok=0; fail=0; \
	for f in customize.sh install.sh uninstall.sh service.sh wg-user scripts/*.sh common/*.sh; do \
	    if [ -f "$$f" ]; then \
	        if sh -n "$$f" 2>/dev/null; then \
	            echo "  ok: $$f"; ok=$$((ok+1)); \
	        else \
	            echo "  FAIL: $$f"; sh -n "$$f"; fail=$$((fail+1)); \
	        fi; \
	    fi; \
	done; \
	if [ $$fail -gt 0 ]; then echo "$$fail file(s) failed syntax"; exit 1; fi
	@echo "==> wrapper executability check..."
	@for f in scripts/cargo-wrappers/*; do \
	    if [ ! -x "$$f" ]; then echo "  NOT exec: $$f"; exit 1; fi; \
	done; \
	echo "  ok: $$(ls scripts/cargo-wrappers/ | wc -l) wrappers"
	@if [ -x $(BIN)/x86_64/wireproxy ]; then \
	    echo "==> wireproxy -n configtest..."; \
	    if [ -f etc/wireproxy.conf ]; then \
	        $(BIN)/x86_64/wireproxy -n -c etc/wireproxy.conf \
	            || echo "  (configtest failed; expected with PLACEHOLDER keys)"; \
	    fi; \
	fi
