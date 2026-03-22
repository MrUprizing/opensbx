#!/usr/bin/env bash
set -euo pipefail

BINARY_NAME="opensbx"
REPO="${OPENSBX_REPO:-MrUprizing/opensbx}"
INSTALL_DIR="${OPENSBX_INSTALL_DIR:-/usr/local/bin}"

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

main() {
  local os arch version archive url tmpdir extracted

  os="$(detect_os)"
  arch="$(detect_arch)"
  version="${OPENSBX_VERSION:-}"
  if [[ -z "$version" ]]; then
    version="$(fetch_latest_version)"
  fi
  if [[ "$version" != v* ]]; then
    version="v${version}"
  fi

  archive="${BINARY_NAME}_${version}_${os}_${arch}.tar.gz"
  url="https://github.com/${REPO}/releases/download/${version}/${archive}"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT

  echo "Downloading ${url}"
  download "$url" "$tmpdir/$archive"

  tar -xzf "$tmpdir/$archive" -C "$tmpdir"
  extracted="$tmpdir/$BINARY_NAME"
  if [[ ! -f "$extracted" ]]; then
    echo "Binary not found inside archive: $archive" >&2
    exit 1
  fi

  if [[ -w "$INSTALL_DIR" ]]; then
    install -m 0755 "$extracted" "$INSTALL_DIR/$BINARY_NAME"
  else
    echo "Installing to $INSTALL_DIR requires sudo"
    sudo install -m 0755 "$extracted" "$INSTALL_DIR/$BINARY_NAME"
  fi

  echo "Installed: $INSTALL_DIR/$BINARY_NAME"
  echo "Run: $BINARY_NAME -addr :8080 -proxy-addr :3000 -base-domain localhost"
}

main "$@"
