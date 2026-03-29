# 🦀 PicoClAWS

**PicoClAWS** is a lightweight, "pico-scale" agentic framework designed specifically for **AWS Lambda**. It brings the power of persistent, tool-equipped agents to a serverless environment with minimal footprint and maximum efficiency.

## 🚀 Concept

PicoClAWS leverages the "Claw" (tool-using) capabilities of LLMs within the "Pico" (serverless/micro) architecture of AWS. It provides:

- **Ephemeral Compute, Persistent Memory**: Agents run on AWS Lambda but maintain state via S3-backed workspace synchronization.
- **Embedded Toolset**: Bundles a full suite of **300+ shell utilities** via a static BusyBox binary, making standard Linux tools available in the restricted Lambda environment.
- **Python Support**: Ready-to-use Python environment for data processing and automation.
- **Telegram Integration**: Native support for Telegram as a primary communication channel.

## 🔗 Based On

PicoClAWS is a specialized AWS Lambda distribution of the core [PicoClaw](https://github.com/sipeed/picoclaw) engine. It adapts the original project's tool-using (Claw) philosophy to a purely serverless environment.

## 🛠 Features

- **Dynamic Symlink Engine**: Automatically generates a full set of Linux commands (`ls`, `grep`, `sed`, `awk`, `find`, etc.) during build, keeping the deployment package under 15MB.
- **Smart Workspace Sync**: Transparently archives and restores the agent's working directory to S3, including support for symlinks and complex directory structures.
- **ARM64 Native**: Optimized for AWS Graviton (ARM64) for better performance and lower costs.

## 🕹 Commands & State Management

PicoClAWS handles state with a persistent-first approach, ensuring any work the agent does is saved and restorable across serverless invocations.

- **`/reset` (The Honest Reset)**: Performs a multi-layer wipe:
  - **Cloud**: Deletes the persistent workspace archive from S3.
  - **Local**: Wipes the ephemeral `/tmp` directory on the Lambda worker.
  - **AI**: Sends a system-level nudge to the LLM to verify it has started fresh with a clean slate (restored from the default `skeleton` assets).
- **`/start` & `/help`**: These are passed directly to the agent's core, allowing its unique "Soul" and "Identity" to define the response.

## 📋 Quick Start

### Prerequisites

- Go 1.25+
- Node.js & NPM (for Serverless framework)
- AWS CLI configured

### Installation & Build

1. Clone the repository

2. Download essential binaries and generate symlinks:
   ```bash
   make download-bins
   ```

3. Build the Lambda packages:
   ```bash
   make build-lambdas
   ```

### Deployment

Deploy to AWS using the Serverless framework:
```bash
make deploy
```

## 🏗 Project Structure

- `cmd/`: Entry points for Webhook and Worker Lambdas.
- `internal/app/`: Core application logic (Worker driver, Telegram handlers).
- `internal/pkg/`: Shared utilities (Archive, Assets management).
- `assets/`: Bundled binaries and Python dependencies (populated during build).
- `deployment/`: Serverless configuration and infrastructure as code.
