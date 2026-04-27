.PHONY: all build-lambdas clean help test

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
		upx --best $(BIN_TARGET)/gh; \
	fi
	@if [ ! -f $(BIN_TARGET)/jq ]; then \
		echo "Downloading jq (ARM64) from Alpine..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/jq-1.7.1-r0.apk -o $(BIN_TARGET)/jq.apk && \
		tar -xf $(BIN_TARGET)/jq.apk -C $(BIN_TARGET) usr/bin/jq && \
		mv $(BIN_TARGET)/usr/bin/jq $(BIN_TARGET)/jq && \
		rm -rf $(BIN_TARGET)/usr $(BIN_TARGET)/jq.apk && \
		upx --best $(BIN_TARGET)/jq; \
	fi
	@if [ ! -f $(BIN_TARGET)/rg ]; then \
		echo "Downloading ripgrep (ARM64) from Alpine..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/community/aarch64/ripgrep-14.1.0-r1.apk -o $(BIN_TARGET)/rg.apk && \
		tar -xf $(BIN_TARGET)/rg.apk -C $(BIN_TARGET) usr/bin/rg && \
		mv $(BIN_TARGET)/usr/bin/rg $(BIN_TARGET)/rg && \
		rm -rf $(BIN_TARGET)/usr $(BIN_TARGET)/rg.apk && \
		upx --best $(BIN_TARGET)/rg; \
	fi
	@if [ ! -f $(BIN_TARGET)/gron ]; then \
		echo "Downloading gron (ARM64)..."; \
		curl -sL https://github.com/tomnomnom/gron/releases/download/v0.7.1/gron-linux-arm64-0.7.1.tgz -o $(BIN_TARGET)/gron.tar.gz && \
		tar -xzf $(BIN_TARGET)/gron.tar.gz -C $(BIN_TARGET) && \
		rm $(BIN_TARGET)/gron.tar.gz && \
		upx --best $(BIN_TARGET)/gron; \
	fi
	@if [ ! -f $(BIN_TARGET)/pup ]; then \
		echo "Downloading pup (ARM64)..."; \
		curl -sL https://github.com/ericchiang/pup/releases/download/v0.4.0/pup_v0.4.0_linux_arm64.zip -o $(BIN_TARGET)/pup.zip && \
		unzip -o $(BIN_TARGET)/pup.zip -d $(BIN_TARGET) && \
		rm $(BIN_TARGET)/pup.zip && \
		upx --best $(BIN_TARGET)/pup; \
	fi
	@if [ ! -f $(BIN_TARGET)/duf ]; then \
		echo "Downloading duf (ARM64)..."; \
		curl -sL https://github.com/muesli/duf/releases/download/v0.8.1/duf_0.8.1_linux_arm64.tar.gz -o $(BIN_TARGET)/duf.tar.gz && \
		tar -xzf $(BIN_TARGET)/duf.tar.gz -C $(BIN_TARGET) duf && \
		rm $(BIN_TARGET)/duf.tar.gz && \
		upx --best $(BIN_TARGET)/duf; \
	fi
	@if [ ! -f $(BIN_TARGET)/ffmpeg ]; then \
		echo "Downloading static ffmpeg (ARM64)..."; \
		curl -sL https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-arm64-static.tar.xz -o $(BIN_TARGET)/ffmpeg.tar.xz && \
		tar -xJf $(BIN_TARGET)/ffmpeg.tar.xz -C $(BIN_TARGET) --strip-components=1 --wildcards "*/ffmpeg" && \
		rm $(BIN_TARGET)/ffmpeg.tar.xz && \
		upx --best $(BIN_TARGET)/ffmpeg; \
	fi
	@if [ ! -f $(BIN_TARGET)/pdfcpu ]; then \
		echo "Downloading pdfcpu (ARM64)..."; \
		curl -sL https://github.com/pdfcpu/pdfcpu/releases/download/v0.9.1/pdfcpu_0.9.1_Linux_arm64.tar.xz -o $(BIN_TARGET)/pdfcpu.tar.xz && \
		tar -xJf $(BIN_TARGET)/pdfcpu.tar.xz -C $(BIN_TARGET) --strip-components=1 pdfcpu_0.9.1_Linux_arm64/pdfcpu && \
		rm $(BIN_TARGET)/pdfcpu.tar.xz && \
		chmod +x $(BIN_TARGET)/pdfcpu && \
		upx --best $(BIN_TARGET)/pdfcpu; \
	fi
	@if [ ! -f $(BIN_TARGET)/yq ]; then \
		echo "Downloading yq (ARM64) from Alpine..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/community/aarch64/yq-go-4.44.1-r2.apk -o $(BIN_TARGET)/yq.apk && \
		tar -xf $(BIN_TARGET)/yq.apk -C $(BIN_TARGET) usr/bin/yq && \
		mv $(BIN_TARGET)/usr/bin/yq $(BIN_TARGET)/yq && \
		rm -rf $(BIN_TARGET)/usr $(BIN_TARGET)/yq.apk && \
		chmod +x $(BIN_TARGET)/yq && \
		upx --best $(BIN_TARGET)/yq; \
	fi
	@if [ ! -f $(BIN_TARGET)/zstd ]; then \
		echo "Downloading zstd (ARM64) from Alpine..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/zstd-1.5.6-r0.apk -o $(BIN_TARGET)/zstd.apk && \
		tar -xf $(BIN_TARGET)/zstd.apk -C $(BIN_TARGET) usr/bin/zstd && \
		mv $(BIN_TARGET)/usr/bin/zstd $(BIN_TARGET)/zstd && \
		rm -rf $(BIN_TARGET)/usr $(BIN_TARGET)/zstd.apk && \
		chmod +x $(BIN_TARGET)/zstd && \
		upx --best $(BIN_TARGET)/zstd; \
	fi
	@if [ ! -f $(BIN_TARGET)/sqlite3 ]; then \
		echo "Downloading sqlite3 (ARM64) from Alpine..."; \
		curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.20/main/aarch64/sqlite-3.45.3-r3.apk -o $(BIN_TARGET)/sqlite3.apk && \
		tar -xf $(BIN_TARGET)/sqlite3.apk -C $(BIN_TARGET) usr/bin/sqlite3 && \
		mv $(BIN_TARGET)/usr/bin/sqlite3 $(BIN_TARGET)/sqlite3 && \
		rm -rf $(BIN_TARGET)/usr $(BIN_TARGET)/sqlite3.apk && \
		chmod +x $(BIN_TARGET)/sqlite3 && \
		upx --best $(BIN_TARGET)/sqlite3; \
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
	@$(MAKE) prune-deps

## prune-deps: Remove heavy/unnecessary Python files to save space
prune-deps:
	@echo "Pruning Python dependencies to save space..."
	# Remove cache and compiled files
	find $(PYTHON_ROOT) -name "__pycache__" -exec rm -rf {} +
	find $(PYTHON_ROOT) -name "*.pyc" -delete
	# Remove large unnecessary subdirectories in heavy packages
	rm -rf $(PYTHON_TARGET)/sympy/plotting
	rm -rf $(PYTHON_TARGET)/sympy/benchmarks
	# Remove metadata and info (saves thousands of small files)
	rm -rf $(PYTHON_TARGET)/*.dist-info
	rm -rf $(PYTHON_TARGET)/*.egg-info
	# Remove tests and docs from all packages
	find $(PYTHON_TARGET) -type d -name "tests" -exec rm -rf {} +
	find $(PYTHON_TARGET) -type d -name "test" -exec rm -rf {} +
	find $(PYTHON_TARGET) -type d -name "testing" -exec rm -rf {} +
	find $(PYTHON_TARGET) -type d -name "docs" -exec rm -rf {} +
	# Remove specialized fontTools modules not needed for basic PDF generation (saves ~10MB)
	# varLib: variable fonts, feaLib: OpenType features, cu2qu/qu2cu: curve conversion
	rm -rf $(PYTHON_TARGET)/fontTools/varLib
	rm -rf $(PYTHON_TARGET)/fontTools/feaLib
	rm -rf $(PYTHON_TARGET)/fontTools/cu2qu
	rm -rf $(PYTHON_TARGET)/fontTools/qu2cu
	@echo "Python dependencies pruned. Current size: $$(du -sh $(PYTHON_ROOT) | cut -f1)"

## clean-deps: Remove installed Python dependencies
clean-deps:
	@echo "Cleaning Python dependencies..."
	@rm -rf $(PYTHON_TARGET)/*

## test: Run all Go tests
test:
	@echo "Running Go tests..."
	$(GO) test -v ./...

## build-lambdas: Build all Lambdas for AWS
build-lambdas: test download-bins download-python install-deps build-tg-webhook-lambda build-tg-worker-lambda build-tg-heartbeat-lambda
	@echo "All Lambdas built successfully."

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
	@upx --fast $(BUILD_DIR)/tg-worker-lambda/bootstrap
	@cp config.json $(BUILD_DIR)/tg-worker-lambda/
	@cp -r assets $(BUILD_DIR)/tg-worker-lambda/
	@echo "Total unzipped size of Worker Lambda: $$(du -sh $(BUILD_DIR)/tg-worker-lambda | cut -f1)"
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
	@echo "  test                Run all Go tests"
	@echo "  build-lambdas       Build and zip all lambdas"
	@echo "  deploy              Build and deploy to AWS"
	@echo "  clean               Remove build artifacts"
	@echo "  help                Show this help"
