APP_NAME   ?= weiran
UPPER_APP_NAME := $(shell echo $(APP_NAME) | tr '[:lower:]-' '[:upper:]_')
# VENDOR is the codesign / bundle-id namespace (e.g. me.<vendor>.<app>).
# Override with `make VENDOR=me.alice install` for your own builds. The
# default `local` is generic so soul-cli's open-source build doesn't
# embed any specific user's identity.
VENDOR     ?= local
INSTALL_DIR = $(HOME)/.local/bin
PLIST_NAME  = $(VENDOR).$(APP_NAME)-server
PLIST_SRC_TMPL = launchagent.plist.tmpl
PLIST_BUILT    = build/$(PLIST_NAME).plist
PLIST_DST      = $(HOME)/Library/LaunchAgents/$(PLIST_NAME).plist

# Linked instances — rebuilt together with the primary app
LINKED_INSTANCES ?=
VERSION    := $(shell cat internal/app/VERSION 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# Build date derived from the current commit's author date (ISO-8601 UTC).
# Critical: keeping this deterministic per-commit means the binary hash
# (and therefore the codesign CDHash) is stable across rebuilds of the
# same commit. Without this, every `make install` would produce a new
# CDHash and macOS Local Network Privacy / TCC would treat each build
# as a brand-new app, re-prompting for permission every time.
# Fallback: when not in a git repo or no commits, use a fixed epoch.
DATE       := $(shell git log -1 --format=%cI 2>/dev/null || echo 1970-01-01T00:00:00Z)
ifeq ($(strip $(DATE)),)
DATE       := 1970-01-01T00:00:00Z
endif
# Only inject APP_NAME into the binary. buildVersion is pulled from the
# embedded VERSION file (content-addressable). buildCommit / buildDate are
# loaded at runtime from <binary>.meta so commits don't change the binary's
# CDHash (see main.go loadBuildMeta). Without this split, every commit would
# drift the CDHash and macOS Local Network Privacy would re-prompt.
LDFLAGS     = -X github.com/kiyor/soul-cli/internal/app.defaultAppName=$(APP_NAME)
CODESIGN_IDENTITY ?= soul-cli Local Dev
ENTITLEMENTS ?= weiran.entitlements

# Info.plist embedded into Mach-O's __TEXT,__info_plist section.
# This is the KEY piece that lets macOS TCC track the binary by
# CFBundleIdentifier instead of CDHash — so LAN permission persists
# across rebuilds of new commits, not just same-commit rebuilds.
INFO_PLIST_TMPL  = Info.plist.tmpl
INFO_PLIST_BUILT = build/$(APP_NAME).Info.plist

.PHONY: build install test clean server-install server-uninstall server-restart server-status server-logs setup-codesign install-all install-linked restart-all fts-index

$(INFO_PLIST_BUILT): $(INFO_PLIST_TMPL) internal/app/VERSION
	@mkdir -p build
	@sed -e 's/@@APP_NAME@@/$(APP_NAME)/g' \
	     -e 's/@@VENDOR@@/$(VENDOR)/g' \
	     -e 's/@@VERSION@@/$(VERSION)/g' \
	     $(INFO_PLIST_TMPL) > $(INFO_PLIST_BUILT)

build: $(INFO_PLIST_BUILT)
	@mkdir -p $(INSTALL_DIR)
	@# external linker required to embed __info_plist section via -sectcreate.
	@# Deterministic: same commit + same plist content = identical binary.
	@# Build directly to INSTALL_DIR — no stale binary left in project dir
	@# or ~/go/bin/ that could shadow via PATH priority.
	go build -ldflags "$(LDFLAGS) -linkmode=external -extldflags=-Wl,-sectcreate,__TEXT,__info_plist,$(INFO_PLIST_BUILT)" -o $(INSTALL_DIR)/$(APP_NAME) .

setup-codesign:
	@CODESIGN_IDENTITY="$(CODESIGN_IDENTITY)" ./scripts/setup-codesign.sh

install: build
	@# Binary already at INSTALL_DIR from build target.
	@# Write build metadata NEXT TO the binary (not INTO it) so commit/date
	@# changes don't alter CDHash. Runtime reads via loadBuildMeta().
	@printf 'version=%s\ncommit=%s\ndate=%s\n' '$(VERSION)' '$(COMMIT)' '$(DATE)' > $(INSTALL_DIR)/$(APP_NAME).meta
ifeq ($(shell uname),Darwin)
	@# Codesign with entitlements + stable identifier.
	@# The --identifier flag forces the signing identifier to be stable
	@# (APP_NAME), decoupling it from the filesystem path. Combined with
	@# a deterministic DATE (git commit time) and entitlements, this makes
	@# the TCC record (identity + identifier + requirement) stable across
	@# rebuilds of the same commit — macOS Local Network Privacy will
	@# remember the LAN permission grant instead of re-prompting.
	@SIGN_ARGS="--identifier $(VENDOR).$(APP_NAME) -o runtime"; \
	if [ -f "$(ENTITLEMENTS)" ]; then \
		SIGN_ARGS="$$SIGN_ARGS --entitlements $(ENTITLEMENTS)"; \
	fi; \
	if security find-identity -v -p codesigning 2>/dev/null | grep -q "$(CODESIGN_IDENTITY)"; then \
		if ! codesign -s "$(CODESIGN_IDENTITY)" -f $$SIGN_ARGS $(INSTALL_DIR)/$(APP_NAME); then \
			echo "codesign with '$(CODESIGN_IDENTITY)' failed; falling back to ad-hoc signature."; \
			echo "Run 'make setup-codesign' or trust the certificate in Keychain Access to restore persistent signing."; \
			codesign -s - -f $$SIGN_ARGS $(INSTALL_DIR)/$(APP_NAME); \
		fi; \
	else \
		echo "No codesigning identity '$(CODESIGN_IDENTITY)' found. Run 'make setup-codesign' to create one."; \
		echo "Without it, macOS will show permission popups on every rebuild."; \
		codesign -s - -f $$SIGN_ARGS $(INSTALL_DIR)/$(APP_NAME); \
	fi
endif
	@echo "installed $(INSTALL_DIR)/$(APP_NAME)"

test:
	go test ./...

clean:
	rm -f $(APP_NAME)
	rm -rf build
	@# Remove stale binary from ~/go/bin if it exists (avoids PATH shadowing)
	@rm -f $(HOME)/go/bin/$(APP_NAME) 2>/dev/null || true

## ── LaunchAgent (server mode) ──
#
# Rehydration: server-restart kills all child Claude processes (including
# the session that triggered it). On startup, rehydrateSessions() restores
# interactive/telegram sessions from the last 2 hours by resuming their
# Claude JSONL history. Each restored session receives a context notice
# about the interruption.
#
# TL;DR: `make server-restart` is ALWAYS safe to run from inside a session.
# The session will die, server restarts, and the session is automatically
# resurrected ~3s later with full conversation history intact.
# Do NOT manually launchctl stop/start or cp+codesign — just `make server-restart`.

$(PLIST_BUILT): $(PLIST_SRC_TMPL) Makefile
	@mkdir -p build
	@sed -e 's|@@APP_NAME@@|$(APP_NAME)|g' \
	     -e 's|@@UPPER_APP_NAME@@|$(UPPER_APP_NAME)|g' \
	     -e 's|@@VENDOR@@|$(VENDOR)|g' \
	     -e 's|@@HOME@@|$(HOME)|g' \
	     -e 's|@@INSTALL_DIR@@|$(INSTALL_DIR)|g' \
	     $(PLIST_SRC_TMPL) > $(PLIST_BUILT)

server-install: install $(PLIST_BUILT)
	cp $(PLIST_BUILT) $(PLIST_DST)
	launchctl bootout gui/$$(id -u) $(PLIST_DST) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(PLIST_DST)
	@echo "$(PLIST_NAME) installed and started"

server-uninstall:
	launchctl bootout gui/$$(id -u) $(PLIST_DST) 2>/dev/null || true
	rm -f $(PLIST_DST)
	@echo "$(PLIST_NAME) removed"

server-restart: install $(PLIST_BUILT)
	cp $(PLIST_BUILT) $(PLIST_DST)
	launchctl bootout gui/$$(id -u) $(PLIST_DST) 2>/dev/null || true
	launchctl bootstrap gui/$$(id -u) $(PLIST_DST)
	@echo "$(PLIST_NAME) restarted"

server-status:
	@launchctl print gui/$$(id -u)/$(PLIST_NAME) 2>/dev/null || echo "not loaded"

server-logs:
	@tail -f $(HOME)/$(APP_NAME)/data/$(APP_NAME)-server.stderr.log

## ── Linked instances (hengzhun, etc.) ──

install-linked:
	@for inst in $(LINKED_INSTANCES); do \
		echo "building $$inst..."; \
		$(MAKE) install APP_NAME=$$inst; \
	done

install-all: install install-linked
	@echo "all instances installed: $(APP_NAME) $(LINKED_INSTANCES)"

restart-all: server-restart install-linked
	@for inst in $(LINKED_INSTANCES); do \
		PLIST=$(VENDOR).$$inst-server; \
		echo "restarting $$inst..."; \
		launchctl bootout gui/$$(id -u) $(HOME)/Library/LaunchAgents/$$PLIST.plist 2>/dev/null || true; \
		launchctl bootstrap gui/$$(id -u) $(HOME)/Library/LaunchAgents/$$PLIST.plist; \
	done
	@echo "all instances restarted: $(APP_NAME) $(LINKED_INSTANCES)"

## ── FTS5 index management ──

fts-index: install
	$(APP_NAME) db fts-index
	@echo "FTS5 index updated (daily notes + session content)"
