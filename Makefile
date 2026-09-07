# Default goal: `make` runs the full build (fmt, vet, test-race, coverage gate,
# cross-builds, license check). Use `make build` for a quick native-only build.
.DEFAULT_GOAL := all

.PHONY: all build build-all install test test-race coverage open-coverage \
        vet fmt clean tidy check-licenses notices publish deploy \
        check-installsh _all_parallel

# ---------------------------------------------------------------------------
# Paths and version metadata
# ---------------------------------------------------------------------------
BINDIR      := bin
BINARY      := $(BINDIR)/slack-acp
NOTICE_FILE := THIRD_PARTY_NOTICES.md

VERSION    := $(shell cat VERSION 2>/dev/null || echo dev)
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_TAG    := $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_DIRTY  := $(shell git diff --quiet 2>/dev/null || echo .dirty)
ifneq ($(GIT_TAG),v$(VERSION))
  ifneq ($(GIT_COMMIT),)
    VERSION := $(VERSION)-dev+$(GIT_COMMIT)$(GIT_DIRTY)
  endif
endif

LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := go build -trimpath -ldflags="$(LDFLAGS)"

# ---------------------------------------------------------------------------
# Quiet build helpers — print a short step name, show output only on failure.
# Usage: $(call RUN,label,command).  Set V=1 for verbose output.
# ---------------------------------------------------------------------------
ifdef V
  define RUN
	@printf "  %-28s\n" "$(1)"
	$(2)
  endef
else
  define RUN
	@_log=$$(mktemp) && ( $(2) ) > $$_log 2>&1 \
		&& { printf "  %-28s ✓\n" "$(1)"; rm -f $$_log; } \
		|| { printf "  %-28s ✗\n" "$(1)"; cat $$_log; rm -f $$_log; exit 1; }
  endef
endif

$(BINDIR):
	@mkdir -p $(BINDIR)

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------
build: tidy | $(BINDIR)
	$(call RUN,build (native),$(GOBUILD) -o $(BINARY) ./cmd/slack-acp/)

install:
	go install -ldflags="$(LDFLAGS)" ./cmd/slack-acp/

# Cross-compile matrix — matches fir's targets: darwin/{arm64,amd64},
# linux/{armv6,arm64,amd64}.  Each entry expands to its own phony target so
# `_all_parallel` can build them concurrently under `make -j`.
#
# Format: <goos>/<goarch>[/<goarm>]
CROSS_TARGETS := darwin/arm64 darwin/amd64 linux/arm/6 linux/arm64 linux/amd64

define CROSS_template
$(eval _os    := $(word 1,$(subst /, ,$(1))))
$(eval _arch  := $(word 2,$(subst /, ,$(1))))
$(eval _goarm := $(word 3,$(subst /, ,$(1))))
$(eval _name  := $(_os)-$(if $(_goarm),$(_arch)v$(_goarm),$(_arch)))
build-$(_name): | $$(BINDIR)
	$$(call RUN,build $(_os)/$(if $(_goarm),$(_arch)v$(_goarm),$(_arch)),GOOS=$(_os) GOARCH=$(_arch) $(if $(_goarm),GOARM=$(_goarm) )$$(GOBUILD) -o $$(BINARY)-$(_name) ./cmd/slack-acp/)
.PHONY: build-$(_name)
CROSS_PHONY += build-$(_name)
endef

$(foreach t,$(CROSS_TARGETS),$(eval $(call CROSS_template,$(t))))

build-all: $(CROSS_PHONY) build

# ---------------------------------------------------------------------------
# Aggregate target: `make all` runs fmt + tidy serially, then everything else
# in parallel.  TIDY_DONE=1 tells sub-targets to skip a redundant go-mod-tidy.
# ---------------------------------------------------------------------------
all: fmt tidy
	@$(MAKE) -j --no-print-directory _all_parallel TIDY_DONE=1

_all_parallel: vet test-race coverage build-all check-licenses check-installsh

# ---------------------------------------------------------------------------
# install.sh — generated from install.sh.json by the canonical distkit
# template (github.com/kfet/distkit/installsh); never hand-edited. Both
# targets are DEV-ONLY: they run here and in CI, never on a user's machine.
# `check-installsh` fails the build when the checked-in script has drifted
# from the template (a hand edit, or a distkit bump that changed it), so a
# stale installer can never be published.
# ---------------------------------------------------------------------------
install.sh: install.sh.json
	$(call RUN,generate install.sh,go run github.com/kfet/distkit/cmd/distkit-installsh -o $@)

check-installsh:
	$(call RUN,check install.sh,go run github.com/kfet/distkit/cmd/distkit-installsh -check)

fmt:
	@gofmt -s -w .

# Skipped when TIDY_DONE=1 (set by `make all` after running tidy upfront).
tidy:
ifndef TIDY_DONE
	@go mod tidy
endif

# ---------------------------------------------------------------------------
# Test & coverage
# ---------------------------------------------------------------------------
test: tidy
	go test ./...

test-race: tidy
	$(call RUN,test (race),go test -race ./...)

vet:
	$(call RUN,vet,go vet ./...)

# coverage runs the suite and enforces 100% statement coverage on the
# filtered profile. The covgate tool (github.com/kfet/covgate) reads
# .covignore, strips matching lines from the raw profile, writes the
# filtered profile to bin/coverage.out, and fails if the result is
# below 100%.
coverage: tidy | $(BINDIR)
	$(call RUN,test (cover),go test -covermode=set -coverprofile=$(BINDIR)/coverage.tmp.out ./...)
	$(call RUN,coverage (100%),go tool covgate -profile=$(BINDIR)/coverage.tmp.out -out=$(BINDIR)/coverage.out -ignore=.covignore -min=100)

open-coverage:
	go tool cover -html=$(BINDIR)/coverage.out

# ---------------------------------------------------------------------------
# Third-party license notices
# ---------------------------------------------------------------------------
GO_LICENSES := go run github.com/google/go-licenses@v1.6.0

notices: $(NOTICE_FILE)

$(NOTICE_FILE): go.mod go.sum
	$(call RUN,generate notices,$(GO_LICENSES) report ./cmd/slack-acp > $(NOTICE_FILE) 2>/dev/null)

check-licenses:
	$(call RUN,check licenses,$(GO_LICENSES) check ./cmd/slack-acp --disallowed_types=forbidden,restricted 2>/dev/null)

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------
clean:
	rm -rf $(BINDIR) dist
	rm -f $(NOTICE_FILE)

# ---------------------------------------------------------------------------
# Release publishing & remote deployment
# ---------------------------------------------------------------------------
RELEASE_TAG := v$(shell cat VERSION 2>/dev/null || echo 0.0.0)

publish: build notices
	@if ! git diff --quiet -- $(NOTICE_FILE); then \
		git add $(NOTICE_FILE) && git commit -m "chore: refresh THIRD_PARTY_NOTICES.md for $(RELEASE_TAG)"; \
	fi
	@echo "Preflight $(RELEASE_TAG)..."
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "main" ]; then \
		echo "ABORT: on branch '$$BRANCH'; releases publish from 'main'."; \
		exit 1; \
	fi; \
	git fetch origin >/dev/null 2>&1 || { echo "ABORT: 'git fetch origin' failed."; exit 1; }; \
	LOCAL=$$(git rev-parse main); \
	REMOTE=$$(git rev-parse origin/main); \
	BASE=$$(git merge-base main origin/main); \
	if [ "$$LOCAL" != "$$REMOTE" ] && [ "$$REMOTE" != "$$BASE" ]; then \
		echo "ABORT: origin/main has moved ahead of local main."; \
		echo "  local  main: $$LOCAL"; \
		echo "  origin/main: $$REMOTE"; \
		echo "A concurrent release landed while this one was in flight."; \
		echo "Do NOT rebase this release commit: $(RELEASE_TAG) was cut against a"; \
		echo "stale VERSION and rebasing it would publish the wrong version number."; \
		echo "Re-cut the release instead:"; \
		echo "  git tag -d $(RELEASE_TAG) && git reset --hard origin/main"; \
		echo "  then bump VERSION above the new tip and re-run the release."; \
		exit 1; \
	fi; \
	if [ -n "$$(git ls-remote --tags origin refs/tags/$(RELEASE_TAG) 2>/dev/null)" ]; then \
		echo "ABORT: tag $(RELEASE_TAG) already exists on origin."; \
		echo "Bump VERSION and re-cut the release."; \
		exit 1; \
	fi; \
	HIGHEST=$$(git ls-remote --tags origin 2>/dev/null \
		| awk '{print $$2}' \
		| sed -e 's|^refs/tags/||' -e 's|\^{}$$||' \
		| grep '^v[0-9]' | sort -V -u | tail -n 1); \
	if [ -n "$$HIGHEST" ] && [ "$$HIGHEST" != "$(RELEASE_TAG)" ] && \
	   [ "$$(printf '%s\n%s\n' "$(RELEASE_TAG)" "$$HIGHEST" | sort -V | tail -n 1)" = "$$HIGHEST" ]; then \
		echo "ABORT: origin already has a higher version tag ($$HIGHEST > $(RELEASE_TAG))."; \
		echo "Bump VERSION above $$HIGHEST and re-cut the release."; \
		exit 1; \
	fi; \
	echo "  origin/main in sync, $(RELEASE_TAG) is the newest tag - OK"
	@echo "Publishing $(RELEASE_TAG)..."
	git push --atomic origin main $(RELEASE_TAG)
	@echo "Pushed $(RELEASE_TAG)."

# Deploy to a remote host via scp (auto-detects OS and arch).
# Usage: make deploy HOST=myhost
deploy: build-all
	@if [ -z "$(HOST)" ]; then echo "Usage: make deploy HOST=<hostname>"; exit 1; fi
	@INFO=$$(ssh -o ConnectTimeout=5 $(HOST) "uname -s -m") || { echo "Cannot reach $(HOST)"; exit 1; }; \
	OS=$$(echo "$$INFO" | awk '{print $$1}'); \
	ARCH=$$(echo "$$INFO" | awk '{print $$2}'); \
	case "$$OS-$$ARCH" in \
		Linux-aarch64|Linux-arm64)   BIN=$(BINARY)-linux-arm64 ;; \
		Linux-armv6l|Linux-armv7l)   BIN=$(BINARY)-linux-armv6 ;; \
		Linux-x86_64)                BIN=$(BINARY)-linux-amd64 ;; \
		Darwin-arm64)                BIN=$(BINARY)-darwin-arm64 ;; \
		Darwin-x86_64)               BIN=$(BINARY)-darwin-amd64 ;; \
		*) echo "Unsupported platform: $$OS $$ARCH"; exit 1 ;; \
	esac; \
	echo "Deploying to $(HOST) ($$OS/$$ARCH → $$BIN)..."; \
	scp -q $$BIN $(HOST):~/.local/bin/slack-acp && \
	ssh $(HOST) "chmod +x ~/.local/bin/slack-acp && ~/.local/bin/slack-acp --version"
