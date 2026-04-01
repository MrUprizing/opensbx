#!/usr/bin/env bash
set -euo pipefail

BINARY_NAME="opensbx"
REPO="${OPENSBX_REPO:-MrUprizing/opensbx}"
INSTALL_DIR="${OPENSBX_INSTALL_DIR:-/usr/local/bin}"
LIB_DIR="${OPENSBX_LIB_DIR:-/usr/local/lib/opensbx}"
AUTO_INSTALL_DOCKER="${OPENSBX_AUTO_INSTALL_DOCKER:-1}"

detect_os() {
  case "$(uname -s)" in
    Linux)
      echo "linux"
      ;;
    Darwin)
      echo "macos"
      ;;
    *)
      echo "Unsupported OS: $(uname -s). Download the release manually from https://github.com/${REPO}/releases" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    arm64|aarch64)
      echo "arm64"
      ;;
    *)
      echo "Unsupported architecture: $(uname -m). Download the release manually from https://github.com/${REPO}/releases" >&2
      exit 1
      ;;
  esac
}

fetch_latest_version() {
  local api_url version
  api_url="https://api.github.com/repos/${REPO}/releases/latest"

  if command -v curl >/dev/null 2>&1; then
    version="$( (curl -fsSL "$api_url" || true) | grep -m1 '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' )"
  elif command -v wget >/dev/null 2>&1; then
    version="$( (wget -qO- "$api_url" || true) | grep -m1 '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/' )"
  else
    echo "Neither curl nor wget is available" >&2
    exit 1
  fi

  if [[ -z "$version" ]]; then
    echo "Unable to resolve latest version from GitHub Releases" >&2
    exit 1
  fi

  echo "$version"
}

download() {
  local url="$1"
  local output="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL "$url" -o "$output"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -O "$output" "$url"
    return
  fi
  echo "Neither curl nor wget is available" >&2
  exit 1
}

run_as_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    "$@"
    return
  fi

  if command -v sudo >/dev/null 2>&1; then
    sudo "$@"
    return
  fi

  echo "This operation requires root privileges but sudo is not available" >&2
  exit 1
}

install_docker_linux() {
  if command -v apt-get >/dev/null 2>&1; then
    echo "Installing Docker via apt-get"
    run_as_root apt-get update
    run_as_root apt-get install -y docker.io
  else
    echo "Automatic Docker installation is only supported on apt-based Linux distributions" >&2
    echo "Install Docker manually and rerun this installer" >&2
    exit 1
  fi

  if command -v systemctl >/dev/null 2>&1; then
    run_as_root systemctl enable --now docker
    return
  fi

  if command -v service >/dev/null 2>&1; then
    run_as_root service docker start
    return
  fi

  echo "Docker installed but could not start daemon automatically. Start it manually and rerun." >&2
  exit 1
}

ensure_docker() {
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    return
  fi

  if [[ "${AUTO_INSTALL_DOCKER}" != "1" ]]; then
    echo "Docker is required but not ready. Install and start Docker, then rerun." >&2
    exit 1
  fi

  if ! command -v docker >/dev/null 2>&1; then
    if [[ "$(uname -s)" != "Linux" ]]; then
      echo "Docker is required. Install Docker manually on this OS, then rerun." >&2
      exit 1
    fi
    install_docker_linux
  elif command -v systemctl >/dev/null 2>&1; then
    echo "Docker CLI detected but daemon is not running. Starting docker service"
    run_as_root systemctl enable --now docker
  elif command -v service >/dev/null 2>&1; then
    echo "Docker CLI detected but daemon is not running. Starting docker service"
    run_as_root service docker start
  else
    echo "Docker CLI detected but daemon is not running. Start Docker and rerun." >&2
    exit 1
  fi

  if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    echo "Docker is still not available after setup. Verify docker daemon and permissions, then rerun." >&2
    exit 1
  fi
}

main() {
  local os arch release_tag version archive url tmpdir extracted binary_target wrapper

  os="$(detect_os)"
  arch="$(detect_arch)"
  ensure_docker

  release_tag="${OPENSBX_VERSION:-}"
  if [[ -z "$release_tag" ]]; then
    release_tag="$(fetch_latest_version)"
  fi
  if [[ "$release_tag" != v* ]]; then
    release_tag="v${release_tag}"
  fi
  version="${release_tag#v}"

  archive="${BINARY_NAME}_${version}_${os}_${arch}.tar.gz"
  url="https://github.com/${REPO}/releases/download/${release_tag}/${archive}"

  tmpdir="$(mktemp -d)"
  trap '[[ -n "${tmpdir:-}" ]] && rm -rf "${tmpdir}"' EXIT

  echo "Downloading ${url}"
  download "$url" "$tmpdir/$archive"

  tar -xzf "$tmpdir/$archive" -C "$tmpdir"
  extracted="$tmpdir/$BINARY_NAME"
  if [[ ! -f "$extracted" ]]; then
    echo "Binary not found inside archive: $archive" >&2
    exit 1
  fi

  binary_target="$LIB_DIR/$BINARY_NAME"
  wrapper="$tmpdir/$BINARY_NAME-wrapper"
  cat >"$wrapper" <<EOF
#!/usr/bin/env bash
set -euo pipefail

BIN_PATH="$binary_target"
LOG_PATH="\${OPENSBX_LOG_FILE:-opensbx.log}"

if [[ "\${OPENSBX_FOREGROUND:-0}" == "1" ]]; then
  exec "\$BIN_PATH" "\$@"
fi

if pgrep -f "\$BIN_PATH" >/dev/null 2>&1; then
  echo "opensbx is already running"
  exit 0
fi

nohup "\$BIN_PATH" "\$@" >>"\$LOG_PATH" 2>&1 &
pid="\$!"
disown "\$pid" 2>/dev/null || true

echo "opensbx started in background (pid: \$pid, log: \$LOG_PATH)"
EOF

  if ! mkdir -p "$LIB_DIR" 2>/dev/null || ! install -m 0755 "$extracted" "$binary_target" 2>/dev/null || ! install -m 0755 "$wrapper" "$INSTALL_DIR/$BINARY_NAME" 2>/dev/null; then
    echo "Installing to $INSTALL_DIR and $LIB_DIR requires sudo"
    run_as_root mkdir -p "$LIB_DIR"
    run_as_root install -m 0755 "$extracted" "$binary_target"
    run_as_root install -m 0755 "$wrapper" "$INSTALL_DIR/$BINARY_NAME"
  fi

  echo "Installed wrapper: $INSTALL_DIR/$BINARY_NAME"
  echo "Installed binary: $binary_target"
  echo "Run in background: $BINARY_NAME"
  echo "Run in foreground: OPENSBX_FOREGROUND=1 $BINARY_NAME"
}

main "$@"
