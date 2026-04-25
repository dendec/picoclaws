.PHONY: all build-lambdas clean help

# Build variables
BUILD_DIR=build
GO?=go
LDFLAGS=-ldflags "-s -w"

# Python dependency variables
PYTHON_ROOT=assets/python
PYTHON_TARGET=$(PYTHON_ROOT)/lib/python3.12/site-packages
BIN_TARGET=assets/bin
PYTHON_VERSION=3.12.9+20250325
PYTHON_URL=https://github.com/astral-sh/python-build-standalone/releases/download/20250325/cpython-$(PYTHON_VERSION)-aarch64-unknown-linux-gnu-install_only.tar.gz

# Default target
all: build-lambdas

## download-bins: Download essential ARM64 static binaries
download-bins:
	@echo "Checking for essential ARM64 binaries in $(BIN_TARGET)..."
	@mkdir -p $(BIN_TARGET)
	@if [ ! -f $(BIN_TARGET)/busybox ] || [ $$(stat -c%s $(BIN_TARGET)/busybox) -lt 1000000 ]; then \
		echo "Downloading ARM64 BusyBox from Alpine (v3.20)..."; \
		rm -f $(BIN_TARGET)/busybox $(BIN_TARGET)/busybox.apk; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/busybox-static-1.36.1-r31.apk -o $(BIN_TARGET)/busybox.apk; \
		tar -xf $(BIN_TARGET)/busybox.apk -C $(BIN_TARGET) bin/busybox.static; \
		mv $(BIN_TARGET)/bin/busybox.static $(BIN_TARGET)/busybox; \
		rm -rf $(BIN_TARGET)/bin $(BIN_TARGET)/busybox.apk; \
		chmod +x $(BIN_TARGET)/busybox; \
		\
		echo "Generating all BusyBox symlinks (using host-compatible x86_64 binary)..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/main/x86_64/busybox-static-1.36.1-r31.apk -o $(BIN_TARGET)/busybox-host.apk; \
		tar -xf $(BIN_TARGET)/busybox-host.apk -C $(BIN_TARGET) bin/busybox.static; \
		mv $(BIN_TARGET)/bin/busybox.static $(BIN_TARGET)/busybox-host; \
		chmod +x $(BIN_TARGET)/busybox-host; \
		for tool in $$($(BIN_TARGET)/busybox-host --list); do \
			[ "$$tool" = "busybox" ] && continue; \
			ln -sf busybox $(BIN_TARGET)/$$tool; \
		done; \
		rm -rf $(BIN_TARGET)/bin $(BIN_TARGET)/busybox-host $(BIN_TARGET)/busybox-host.apk; \
		echo "Generated $$(ls $(BIN_TARGET) | wc -l) commands in $(BIN_TARGET)."; \
	fi
	@if [ ! -f $(BIN_TARGET)/curl ]; then \
		echo "Downloading static curl (ARM64)..."; \
		curl -sL https://github.com/moparisthebest/static-curl/releases/download/v8.7.1/curl-aarch64 -o $(BIN_TARGET)/curl && chmod +x $(BIN_TARGET)/curl; \
	fi
	@if [ ! -f $(BIN_TARGET)/gh ]; then \
		echo "Downloading GitHub CLI (ARM64)..."; \
		curl -sL https://github.com/cli/cli/releases/download/v2.65.0/gh_2.65.0_linux_arm64.tar.gz -o $(BIN_TARGET)/gh.tar.gz; \
		tar -xzf $(BIN_TARGET)/gh.tar.gz -C $(BIN_TARGET) --strip-components=2 gh_2.65.0_linux_arm64/bin/gh; \
		rm $(BIN_TARGET)/gh.tar.gz; \
		chmod +x $(BIN_TARGET)/gh; \
	fi

## download-python: Download portable Aarch64 Python 3.12
download-python:
	@echo "Checking for portable Python in $(PYTHON_ROOT)..."
	@if [ ! -f $(PYTHON_ROOT)/bin/python3 ]; then \
		echo "Downloading portable Python 3.12 (Aarch64)..."; \
		mkdir -p $(PYTHON_ROOT); \
		curl -sL $(PYTHON_URL) -o $(PYTHON_ROOT)/python.tar.gz; \
		tar -xzf $(PYTHON_ROOT)/python.tar.gz -C $(PYTHON_ROOT) --strip-components=1; \
		rm $(PYTHON_ROOT)/python.tar.gz; \
		echo "Python 3.12 installed in $(PYTHON_ROOT)."; \
	fi

## install-deps: Install Python dependencies for ARM64 Lambda
install-deps: clean-deps
	@echo "Installing Python dependencies to $(PYTHON_TARGET)..."
	@mkdir -p $(PYTHON_TARGET)
	pip install -r requirements.txt \
		--platform manylinux2014_aarch64 \
		--only-binary=:all: \
		--target $(PYTHON_TARGET) \
		--upgrade

## clean-deps: Remove installed Python dependencies
clean-deps:
	@echo "Cleaning Python dependencies..."
	@rm -rf $(PYTHON_TARGET)/*

## build-lambdas: Build all PicoClAWS Lambdas for AWS (Linux/ARM64)
build-lambdas: download-bins download-python install-deps build-tg-webhook-lambda build-tg-worker-lambda build-tg-heartbeat-lambda

## build-tg-webhook-lambda: Build the Telegram Webhook Lambda for AWS
build-tg-webhook-lambda:
	@echo "Building tg-webhook-lambda for AWS (Linux/ARM64)..."
	@mkdir -p $(BUILD_DIR)/tg-webhook-lambda
	GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/tg-webhook-lambda/bootstrap ./cmd/tg-webhook-lambda
	@cp config.json $(BUILD_DIR)/tg-webhook-lambda/
	@echo "Building zip: $(BUILD_DIR)/tg-webhook-lambda.zip"
	@cd $(BUILD_DIR)/tg-webhook-lambda && zip -qry ../tg-webhook-lambda.zip bootstrap config.json
	@echo "Build complete: $(BUILD_DIR)/tg-webhook-lambda.zip"

## build-tg-worker-lambda: Build the Telegram Worker Lambda for AWS
build-tg-worker-lambda:
	@echo "Building tg-worker-lambda for AWS (Linux/ARM64)..."
	@rm -rf $(BUILD_DIR)/tg-worker-lambda
	@mkdir -p $(BUILD_DIR)/tg-worker-lambda
	GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/tg-worker-lambda/bootstrap ./cmd/tg-worker-lambda
	@cp config.json $(BUILD_DIR)/tg-worker-lambda/
	@cp -r assets $(BUILD_DIR)/tg-worker-lambda/
	@echo "Building zip: $(BUILD_DIR)/tg-worker-lambda.zip"
	@rm -f $(BUILD_DIR)/tg-worker-lambda.zip
	@cd $(BUILD_DIR)/tg-worker-lambda && zip -qry ../tg-worker-lambda.zip bootstrap config.json assets
	@echo "Build complete: $(BUILD_DIR)/tg-worker-lambda.zip"

## build-tg-heartbeat-lambda: Build the Heartbeat Dispatcher Lambda for AWS
build-tg-heartbeat-lambda:
	@echo "Building tg-heartbeat-lambda for AWS (Linux/ARM64)..."
	@mkdir -p $(BUILD_DIR)/tg-heartbeat-lambda
	GOOS=linux GOARCH=arm64 $(GO) build $(LDFLAGS) -o $(BUILD_DIR)/tg-heartbeat-lambda/bootstrap ./cmd/tg-heartbeat-lambda
	@echo "Building zip: $(BUILD_DIR)/tg-heartbeat-lambda.zip"
	@cd $(BUILD_DIR)/tg-heartbeat-lambda && zip -qry ../tg-heartbeat-lambda.zip bootstrap
	@echo "Build complete: $(BUILD_DIR)/tg-heartbeat-lambda.zip"

## clean: Remove build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

## deploy: Deploy Lambdas to AWS using Serverless
deploy: build-lambdas
	@echo "Deploying to AWS..."
	@cd deployment && sls deploy

## help: Show this help message
help:
	@echo "PicoClAWS Makefile"
	@echo ""
	@echo "Usage:"
	@echo "  make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build-lambdas       Build and zip all lambdas"
	@echo "  deploy              Build and deploy to AWS"
	@echo "  clean               Remove build artifacts"
	@echo "  help                Show this help"
