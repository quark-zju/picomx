SHELL := /bin/sh

BINARY := picomxd
BUILD_TARGET := ./cmd/picomxd
PREFIX ?= /usr/local/bin
INSTALL_PATH := $(PREFIX)/$(BINARY)
SYSTEMD_DIR ?= /etc/systemd/system
SYSTEMD_OVERRIDE_DIR ?= $(SYSTEMD_DIR)/picomx.socket.d
SYSTEMD_OVERRIDE_FILE ?= $(SYSTEMD_OVERRIDE_DIR)/override.conf
CONFIG_DIR ?= /etc/picomx
ENV_FILE ?= $(CONFIG_DIR)/picomx.env
ENV_EXAMPLE := examples/picomx.env.example
CUSTOM_POLICY := internal/config/custom_local.go
CUSTOM_POLICY_EXAMPLE := examples/custom_local.go.example
SERVICE_USER ?= picomx
SERVICE_GROUP ?= $(SERVICE_USER)
FAIL2BAN_FILTER_DIR ?= /etc/fail2ban/filter.d
FAIL2BAN_JAIL_DIR ?= /etc/fail2ban/jail.d
CERT_DIR ?= /etc/picosrv/certs
SUDO ?= sudo

.PHONY: build test fmt config install setup-user install-config install-systemd override-systemd install-fail2ban facl deploy deploy-fail2ban

build:
	go build -o $(BINARY) $(BUILD_TARGET)

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

config:
	@if [ ! -f $(CUSTOM_POLICY) ]; then \
		cp $(CUSTOM_POLICY_EXAMPLE) $(CUSTOM_POLICY); \
	fi
	EDITOR="$${EDITOR:-vim}" sh -c '"$$EDITOR" "$(CUSTOM_POLICY)"'

install: build
	$(SUDO) install -m 755 $(BINARY) $(INSTALL_PATH)

setup-user:
	@if ! getent group $(SERVICE_GROUP) >/dev/null; then \
		$(SUDO) groupadd --system $(SERVICE_GROUP); \
	fi

install-config:
	$(SUDO) install -d -m 755 $(CONFIG_DIR)
	@if [ ! -f $(ENV_FILE) ]; then \
		if ! command -v openssl >/dev/null 2>&1; then \
			echo "openssl is required to generate POP3 credentials" >&2; \
			exit 1; \
		fi; \
		password=$$(openssl rand -hex 24); \
		digest=$$(printf '%s' "$$password" | openssl dgst -sha256 -r | awk '{print $$1}'); \
		tmp_env=$$(mktemp); \
		trap 'rm -f "$$tmp_env"' 0 HUP INT TERM; \
		cp $(ENV_EXAMPLE) "$$tmp_env"; \
		printf '\nPICOMX_POP3_USERNAME=picomx\nPICOMX_POP3_PASSWORD_SHA256=%s\n' "$$digest" >>"$$tmp_env"; \
		$(SUDO) install -m 600 "$$tmp_env" $(ENV_FILE); \
		echo "generated POP3 username: picomx"; \
		echo "generated POP3 app password (shown once): $$password"; \
		echo "edit $(ENV_FILE) before starting picomx.socket"; \
	fi
	@if ! getent passwd $(SERVICE_USER) >/dev/null; then \
		$(SUDO) useradd --system --gid $(SERVICE_GROUP) --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin $(SERVICE_USER); \
	fi

install-systemd:
	$(SUDO) install -m 644 deploy/systemd/picomx.service $(SYSTEMD_DIR)/picomx.service
	$(SUDO) install -m 644 deploy/systemd/picomx.socket $(SYSTEMD_DIR)/picomx.socket
	$(SUDO) systemctl daemon-reload

override-systemd:
	@if [ -z "$(SMTP_PORT)" ]; then \
		echo "usage: make override-systemd SMTP_PORT=2525" >&2; \
		exit 2; \
	fi
	$(SUDO) install -d -m 755 $(SYSTEMD_OVERRIDE_DIR)
	tmp_override=$$(mktemp); \
	trap 'rm -f "$$tmp_override"' 0 HUP INT TERM; \
	{ \
		echo '[Socket]'; \
		echo 'ListenStream='; \
		echo 'ListenStream=$(SMTP_PORT)'; \
		echo 'ListenStream=995'; \
	} >"$$tmp_override"; \
	$(SUDO) install -m 644 "$$tmp_override" $(SYSTEMD_OVERRIDE_FILE); \
	$(SUDO) systemctl daemon-reload; \
	$(SUDO)edit $(SYSTEMD_OVERRIDE_FILE)

install-fail2ban:
	@if ! command -v fail2ban-client >/dev/null 2>&1; then \
		echo "fail2ban-client is required; install fail2ban first" >&2; \
		exit 1; \
	fi
	$(SUDO) install -d -m 755 $(FAIL2BAN_FILTER_DIR) $(FAIL2BAN_JAIL_DIR)
	$(SUDO) install -m 644 deploy/fail2ban/filter.d/picomx.conf $(FAIL2BAN_FILTER_DIR)/picomx.conf
	$(SUDO) install -m 644 deploy/fail2ban/jail.d/picomx.local $(FAIL2BAN_JAIL_DIR)/picomx.local

facl: setup-user
	@if ! command -v setfacl >/dev/null 2>&1; then \
		echo "setfacl is required; install ACL tools first" >&2; \
		exit 1; \
	fi
	$(SUDO) setfacl -Rm u:$(SERVICE_USER):rX $(CERT_DIR)

deploy: setup-user install install-config install-systemd

deploy-fail2ban: install-fail2ban
	$(SUDO) fail2ban-client reload
