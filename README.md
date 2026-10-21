# SlimMedia Agent

[![Build & Release](https://github.com/will25u/slimmedia-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/will25u/slimmedia-agent/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**SlimMedia Agent** is the high-performance local worker daemon and system tray application for [SlimMedia](https://github.com/will25u/slimmedia). It handles local media discovery, hardware-accelerated transcoding orchestration (FFmpeg), and secure bi-directional control via WebSockets.

---

## Features

- **Hardware-Accelerated Transcoding:** Automated GPU detection and pipeline tuning for NVIDIA NVENC, Intel QuickSync (QSV), Apple VideoToolbox, and AMD AMF/VAAPI.
- **Chunked Pipeline Orchestration:** Splits large media into deterministic chunks, encodes them with optimal efficiency, and reconstitutes lossless container streams with perfect audio/subtitle sync.
- **Stream-Safety Interlocking:** Dynamically pauses background transcode tasks when active user streaming is detected on Plex, Jellyfin, or Emby media servers.
- **Dual Runtime Profiles:**
  - **Headless CLI (`cmd/agent`):** Suitable for systemd services, Linux servers, NAS appliances, and unRAID/TrueNAS containers.
  - **System Tray GUI (`cmd/tray`):** Native Windows desktop companion with one-click pairing, real-time activity status, and tray controls.
- **Secure Architecture:** Mutual cryptographic pairing tokens, Ed25519 binary update verification, and path-traversal sandboxing.

---

## Building from Source

### Prerequisites

- **Go:** 1.22+ (tested through Go 1.26)
- **Git**
- Optional (for Windows GUI compilation on Linux): `x86_64-w64-mingw32-gcc` (if building with CGo) or pure Go cross-compilation (`CGO_ENABLED=0 GOOS=windows`).

### Compiling Headless CLI

```bash
# Build headless CLI binary for the host platform
go build -o bin/slimmedia-agent ./cmd/agent
```

### Compiling Windows System Tray Application

```bash
# Build native Windows GUI executable
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-H=windowsgui" \
  -o dist/slimmedia-agent-windows-amd64.exe ./cmd/tray
```

---

## Usage & Pairing

### 1. Pairing with a SlimMedia Server

On first run, provide your SlimMedia server pairing code or launch the tray application to enter the token in the graphical wizard:

```bash
./bin/slimmedia-agent pair --server https://slimmedia.example.com --token <PAIRING_TOKEN>
```

### 2. Starting the Agent

```bash
./bin/slimmedia-agent run --config ~/.config/slimmedia/agent.json
```

---

## CI/CD & Code Signing

Binaries are compiled via GitHub Actions on hosted runners. Windows binaries are code-signed by the SignPath Foundation. Windows releases are signed using Authenticode certificates provided by the **[SignPath Foundation](https://signpath.org)**.

---

## Open Source License & Compliance

SlimMedia Agent is released under the **MIT License**. See [LICENSE](LICENSE) for the full license text.

This project bundles or orchestrates build distributions of FFmpeg licensed under GPLv3. In accordance with Section 6 of GPLv3, complete instructions and access to corresponding source code for bundled FFmpeg builds are available by running:

```bash
./bin/slimmedia-agent license
```
