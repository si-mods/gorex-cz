BIN     := czgorex
GOOS    ?= linux
GOARCH  ?= arm64
PREFIX  ?= $(HOME)/.local

APP_DIR := /usr/share/APPLaunch

.PHONY: build install install-app clean

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) .

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)

# Launcher entry + icon; needs root for /usr/share/APPLaunch.
install-app:
	sudo install -d $(APP_DIR)/applications $(APP_DIR)/share/images
	sudo install -m 0644 packaging/czgorex.desktop $(APP_DIR)/applications/czgorex.desktop
	sudo install -m 0644 assets/icon-256.png $(APP_DIR)/share/images/czgorex_256.png

clean:
	rm -f $(BIN)
