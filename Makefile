SHELL := /bin/sh

BINARY := picomaild
BUILD_TARGET := ./cmd/picomaild
PREFIX ?= /usr/local/bin
INSTALL_PATH := $(PREFIX)/$(BINARY)
SYSTEMD_DIR ?= /etc/systemd/system
CONFIG_DIR ?= /etc/picomail
ENV_FILE ?= $(CONFIG_DIR)/picomail.env
ENV_EXAMPLE := examples/picomail.env.example
SERVICE_USER ?= picomail
SERVICE_GROUP ?= $(SERVICE_USER)
SUDO ?= sudo

.PHONY: build test fmt install setup-user install-config install-systemd deploy

build:
	go build -o $(BINARY) $(BUILD_TARGET)

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

install: build
	$(SUDO) install -m 755 $(BINARY) $(INSTALL_PATH)

setup-user:
	@if ! getent group $(SERVICE_GROUP) >/dev/null; then \
		$(SUDO) groupadd --system $(SERVICE_GROUP); \
	fi

install-config:
	$(SUDO) install -d -m 755 $(CONFIG_DIR)
	@if [ ! -f $(ENV_FILE) ]; then \
		$(SUDO) install -m 600 $(ENV_EXAMPLE) $(ENV_FILE); \
		echo "edit $(ENV_FILE) before starting picomail.socket"; \
	fi
	@if ! getent passwd $(SERVICE_USER) >/dev/null; then \
		$(SUDO) useradd --system --gid $(SERVICE_GROUP) --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin $(SERVICE_USER); \
	fi

install-systemd:
	$(SUDO) install -m 644 deploy/systemd/picomail.service $(SYSTEMD_DIR)/picomail.service
	$(SUDO) install -m 644 deploy/systemd/picomail.socket $(SYSTEMD_DIR)/picomail.socket
	$(SUDO) systemctl daemon-reload

deploy: setup-user install install-config install-systemd
