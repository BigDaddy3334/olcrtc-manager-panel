#!/usr/bin/env bash
set -euo pipefail

random_hex() {
	local bytes="${1:-16}"
	od -An -N"$bytes" -tx1 /dev/urandom | tr -d ' \n'
}

random_port() {
	local raw port
	for _ in $(seq 1 20); do
		raw="$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')"
		port=$((20000 + raw % 40000))
		if ! ss -ltn "( sport = :$port )" 2>/dev/null | grep -q ":$port"; then
			printf '%s\n' "$port"
			return
		fi
	done
	printf '%s\n' 8888
}

PANEL_REPO="${PANEL_REPO:-https://github.com/oleg54644/olcrtc-manager-panel-GM.git}"
PANEL_REF="${PANEL_REF:-main}"
OLCRTC_REPO="${OLCRTC_REPO:-https://github.com/openlibrecommunity/olcrtc.git}"
OLCRTC_REF="${OLCRTC_REF:-master}"
GO_VERSION="${GO_VERSION:-1.26.3}"
MIN_BUILD_MEMORY_MB="${MIN_BUILD_MEMORY_MB:-2048}"
AUTO_SWAP="${AUTO_SWAP:-1}"
BUILD_SWAP_FILE="${BUILD_SWAP_FILE:-/swapfile}"
BUILD_SWAP_SIZE="${BUILD_SWAP_SIZE:-2G}"
GO_BUILD_P="${GO_BUILD_P:-}"
PANEL_ADDR="${PANEL_ADDR:-0.0.0.0}"
PANEL_PORT="${PANEL_PORT:-$(random_port)}"
PANEL_ADMIN_PATH="${PANEL_ADMIN_PATH:-${OLCRTC_MANAGER_ADMIN_PATH:-/admin-$(random_hex 4)}}"
case "$PANEL_ADMIN_PATH" in
	/*) ;;
	*) PANEL_ADMIN_PATH="/$PANEL_ADMIN_PATH" ;;
esac
INSTALL_SRC_DIR="${INSTALL_SRC_DIR:-/opt/olcrtc-manager-src}"
CONFIG_DIR="${CONFIG_DIR:-/etc/olcrtc-manager}"
CONFIG_PATH="${CONFIG_PATH:-$CONFIG_DIR/config.json}"
PANEL_ENV_PATH="${PANEL_ENV_PATH:-$CONFIG_DIR/panel.env}"
PANEL_TLS="${PANEL_TLS:-1}"
TLS_CERT_PATH="${TLS_CERT_PATH:-$CONFIG_DIR/tls.crt}"
TLS_KEY_PATH="${TLS_KEY_PATH:-$CONFIG_DIR/tls.key}"
PANEL_CERT_IP="${PANEL_CERT_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}"
PANEL_PUBLIC_HOST="${PANEL_PUBLIC_HOST:-$PANEL_CERT_IP}"
GENERATED_PANEL_ENV=0
GENERATED_ADMIN_USER=""
GENERATED_ADMIN_PASS=""
PANEL_SUPPORTS_ADMIN_PATH=1
PANEL_SUPPORTS_TLS=1
DISPLAY_SCHEME="http"
DISPLAY_ADMIN_PATH="$PANEL_ADMIN_PATH"
DISPLAY_HOST="$PANEL_ADDR"
if [ "$DISPLAY_HOST" = "0.0.0.0" ] || [ "$DISPLAY_HOST" = "::" ]; then
	DISPLAY_HOST="${PANEL_PUBLIC_HOST:-$DISPLAY_HOST}"
fi

log() {
	printf '[olcrtc-manager] %s\n' "$*"
}

die() {
	printf '[olcrtc-manager] ERROR: %s\n' "$*" >&2
	exit 1
}

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		die "run as root: curl -fsSL .../scripts/install.sh | sudo bash"
	fi
}

meminfo_mb() {
	local key="$1"
	awk -v key="$key" '$1 == key ":" {print int($2 / 1024); found=1} END {if (!found) print 0}' /proc/meminfo 2>/dev/null || printf '0\n'
}

ensure_build_memory() {
	local mem swap total
	mem="$(meminfo_mb MemTotal)"
	swap="$(meminfo_mb SwapTotal)"
	total=$((mem + swap))
	log "memory: RAM=${mem}MB swap=${swap}MB total=${total}MB"

	if [ -z "$GO_BUILD_P" ] && [ "$total" -lt 3072 ]; then
		GO_BUILD_P=1
		log "low-memory build mode enabled: go build -p 1"
	fi

	if [ "$total" -ge "$MIN_BUILD_MEMORY_MB" ]; then
		return
	fi

	if [ "$AUTO_SWAP" != "1" ]; then
		log "warning: less than ${MIN_BUILD_MEMORY_MB}MB RAM+swap; Go build may be very slow or fail with OOM"
		log "add swap or rerun with AUTO_SWAP=1"
		return
	fi

	if [ "$swap" -gt 0 ]; then
		log "warning: less than ${MIN_BUILD_MEMORY_MB}MB RAM+swap even with existing swap; build may be slow"
		return
	fi

	log "creating temporary build swap: ${BUILD_SWAP_FILE} (${BUILD_SWAP_SIZE})"
	if [ -e "$BUILD_SWAP_FILE" ]; then
		log "swap file exists; not overwriting: $BUILD_SWAP_FILE"
		return
	fi
	fallocate -l "$BUILD_SWAP_SIZE" "$BUILD_SWAP_FILE" || dd if=/dev/zero of="$BUILD_SWAP_FILE" bs=1M count=2048 status=progress
	chmod 600 "$BUILD_SWAP_FILE"
	mkswap "$BUILD_SWAP_FILE" >/dev/null
	swapon "$BUILD_SWAP_FILE"
	log "swap enabled for build"
}

install_packages() {
	if command -v apt-get >/dev/null 2>&1; then
		export DEBIAN_FRONTEND=noninteractive
		apt-get update
		apt-get install -y --no-install-recommends ca-certificates curl git tar xz-utils iproute2 iptables openssl
		return
	fi
	die "unsupported OS: this installer currently supports apt-based Linux distributions"
}

go_arch() {
	case "$(uname -m)" in
		x86_64|amd64) echo "amd64" ;;
		aarch64|arm64) echo "arm64" ;;
		*) die "unsupported CPU architecture: $(uname -m)" ;;
	esac
}

go_version_ok() {
	command -v go >/dev/null 2>&1 || return 1
	local current
	current="$(go env GOVERSION | sed 's/^go//')"
	[ "$(printf '%s\n%s\n' "$GO_VERSION" "$current" | sort -V | head -n1)" = "$GO_VERSION" ]
}

install_go() {
	if go_version_ok; then
		log "Go $(go env GOVERSION) found"
		return
	fi

	local arch archive url tmp
	arch="$(go_arch)"
	archive="go${GO_VERSION}.linux-${arch}.tar.gz"
	url="https://go.dev/dl/${archive}"
	tmp="/tmp/${archive}"

	log "installing Go ${GO_VERSION}"
	curl -fsSL "$url" -o "$tmp"
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tmp"
	ln -sf /usr/local/go/bin/go /usr/local/bin/go
	ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

clone_repo() {
	local repo="$1" ref="$2" dest="$3"
	rm -rf "$dest"
	git clone --depth 1 --branch "$ref" "$repo" "$dest"
}

build_olcrtc() {
	local src="$1"
	log "building olcrtc"
	if [ -n "$GO_BUILD_P" ]; then
		(cd "$src" && CGO_ENABLED=0 go build -p "$GO_BUILD_P" -o /tmp/olcrtc ./cmd/olcrtc)
	else
		(cd "$src" && CGO_ENABLED=0 go build -o /tmp/olcrtc ./cmd/olcrtc)
	fi
	install -m 0755 /tmp/olcrtc /usr/local/bin/olcrtc
}

build_manager() {
	local src="$1"
	log "building olcrtc-manager"
	if [ ! -f "$src/cmd/olcrtc-manager/web/dist/index.html" ]; then
		die "frontend bundle is missing in repository; build assets before publishing installer"
	fi
	if [ -n "$GO_BUILD_P" ]; then
		(cd "$src" && CGO_ENABLED=0 go build -p "$GO_BUILD_P" -o /tmp/olcrtc-manager ./cmd/olcrtc-manager)
	else
		(cd "$src" && CGO_ENABLED=0 go build -o /tmp/olcrtc-manager ./cmd/olcrtc-manager)
	fi
	install -m 0755 /tmp/olcrtc-manager /usr/local/bin/olcrtc-manager
}

detect_manager_features() {
	if grep -aq "OLCRTC_MANAGER_ADMIN_PATH" /usr/local/bin/olcrtc-manager; then
		PANEL_SUPPORTS_ADMIN_PATH=1
	else
		PANEL_SUPPORTS_ADMIN_PATH=0
		PANEL_ADMIN_PATH="/admin"
		DISPLAY_ADMIN_PATH="/admin"
		log "built manager does not support custom admin path; using /admin"
	fi

	if grep -aq "OLCRTC_MANAGER_TLS_CERT" /usr/local/bin/olcrtc-manager && grep -aq "OLCRTC_MANAGER_TLS_KEY" /usr/local/bin/olcrtc-manager; then
		PANEL_SUPPORTS_TLS=1
	else
		PANEL_SUPPORTS_TLS=0
		PANEL_TLS=0
		DISPLAY_SCHEME="http"
		log "built manager does not support built-in TLS; using HTTP"
	fi
}

read_config_port() {
	local port
	[ -f "$CONFIG_PATH" ] || return 0
	port="$(tr -d '\n\r' < "$CONFIG_PATH" | sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n1)"
	case "$port" in
		''|*[!0-9]*) return 0 ;;
	esac
	if [ "$port" -gt 0 ] && [ "$port" -le 65535 ]; then
		printf '%s\n' "$port"
	fi
}

apply_config_port() {
	local port
	port="$(read_config_port)"
	if [ -n "$port" ]; then
		PANEL_PORT="$port"
		log "using panel port from config: $PANEL_PORT"
	elif [ -f "$CONFIG_PATH" ]; then
		log "warning: could not read a valid port from $CONFIG_PATH; showing generated port $PANEL_PORT"
	fi
}

write_config_if_missing() {
	install -d -m 0755 "$CONFIG_DIR"
	install -d -m 0700 "$CONFIG_DIR/backups"

	if [ -f "$CONFIG_PATH" ]; then
		log "keeping existing config: $CONFIG_PATH"
		return
	fi

	log "creating initial config without rooms"

	cat > "$CONFIG_PATH" <<EOF
{
  "version": 1,
  "name": "OlcRTC VPS",
  "port": $PANEL_PORT,
  "clients": []
}
EOF
	chmod 0600 "$CONFIG_PATH"
	log "created config: $CONFIG_PATH"
}

write_tls_cert_if_missing() {
	if [ "$PANEL_TLS" != "1" ]; then
		return
	fi
	install -d -m 0755 "$CONFIG_DIR"
	if [ -f "$TLS_CERT_PATH" ] && [ -f "$TLS_KEY_PATH" ]; then
		log "keeping existing TLS certificate: $TLS_CERT_PATH"
		DISPLAY_SCHEME="https"
		return
	fi

	local san
	san="DNS:localhost"
	if [ -n "$PANEL_CERT_IP" ]; then
		san="IP:${PANEL_CERT_IP},DNS:localhost"
	fi
	log "creating self-signed TLS certificate: $TLS_CERT_PATH"
	openssl req -x509 -nodes -newkey rsa:2048 -sha256 -days 825 \
		-keyout "$TLS_KEY_PATH" \
		-out "$TLS_CERT_PATH" \
		-subj "/CN=olcrtc-manager" \
		-addext "subjectAltName=${san}" >/dev/null 2>&1
	chmod 0600 "$TLS_KEY_PATH"
	chmod 0644 "$TLS_CERT_PATH"
	DISPLAY_SCHEME="https"
}

read_env_value() {
	local key="$1" file="$2" value
	value="$(grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2- || true)"
	value="${value#\'}"
	value="${value%\'}"
	printf '%s\n' "$value"
}

ensure_env_value() {
	local key="$1" value="$2" file="$3"
	if grep -Eq "^${key}=" "$file" 2>/dev/null; then
		return
	fi
	cat >> "$file" <<EOF
${key}='$value'
EOF
}

write_panel_env_if_missing() {
	install -d -m 0755 "$CONFIG_DIR"
	if [ -f "$PANEL_ENV_PATH" ]; then
		log "keeping existing panel env: $PANEL_ENV_PATH"
		if [ "$PANEL_SUPPORTS_ADMIN_PATH" = "1" ]; then
			DISPLAY_ADMIN_PATH="$(read_env_value OLCRTC_MANAGER_ADMIN_PATH "$PANEL_ENV_PATH")"
			DISPLAY_ADMIN_PATH="${DISPLAY_ADMIN_PATH:-/admin}"
		else
			DISPLAY_ADMIN_PATH="/admin"
		fi
		if [ "$PANEL_SUPPORTS_TLS" = "1" ] && [ "$PANEL_TLS" = "1" ]; then
			ensure_env_value OLCRTC_MANAGER_TLS_CERT "$TLS_CERT_PATH" "$PANEL_ENV_PATH"
			ensure_env_value OLCRTC_MANAGER_TLS_KEY "$TLS_KEY_PATH" "$PANEL_ENV_PATH"
			chmod 0600 "$PANEL_ENV_PATH"
			DISPLAY_SCHEME="https"
		fi
		return
	fi

	GENERATED_PANEL_ENV=1
	GENERATED_ADMIN_USER="${PANEL_ADMIN_USER:-admin$(random_hex 3)}"
	GENERATED_ADMIN_PASS="${PANEL_ADMIN_PASS:-$(random_hex 16)}"
	cat > "$PANEL_ENV_PATH" <<EOF
OLCRTC_MANAGER_USER='$GENERATED_ADMIN_USER'
OLCRTC_MANAGER_PASS='$GENERATED_ADMIN_PASS'
EOF
	if [ "$PANEL_SUPPORTS_ADMIN_PATH" = "1" ]; then
		cat >> "$PANEL_ENV_PATH" <<EOF
OLCRTC_MANAGER_ADMIN_PATH='$PANEL_ADMIN_PATH'
EOF
	fi
	if [ "$PANEL_SUPPORTS_TLS" = "1" ] && [ "$PANEL_TLS" = "1" ]; then
		cat >> "$PANEL_ENV_PATH" <<EOF
OLCRTC_MANAGER_TLS_CERT='$TLS_CERT_PATH'
OLCRTC_MANAGER_TLS_KEY='$TLS_KEY_PATH'
EOF
	fi
	chmod 0600 "$PANEL_ENV_PATH"
	log "created panel env: $PANEL_ENV_PATH"
}

install_service() {
	log "installing systemd service"
	cat > /etc/systemd/system/olcrtc-manager.service <<EOF
[Unit]
Description=OlcRTC Manager Panel
Documentation=https://github.com/oleg54644/olcrtc-manager-panel-GM
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=OLCRTC_PATH=/usr/local/bin/olcrtc
Environment=OLCRTC_MANAGER_ADDR=$PANEL_ADDR
EnvironmentFile=-$PANEL_ENV_PATH
ExecStart=/usr/local/bin/olcrtc-manager -config $CONFIG_PATH
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=10s

[Install]
WantedBy=multi-user.target
EOF
	systemctl daemon-reload
	systemctl enable --now olcrtc-manager
}

sync_sources() {
	local src="$1"
	rm -rf "$INSTALL_SRC_DIR"
	mkdir -p "$INSTALL_SRC_DIR"
	tar --exclude='.git' --exclude='node_modules' -C "$src" -cf - . | tar -C "$INSTALL_SRC_DIR" -xf -
}

main() {
	need_root
	install_packages
	install_go
	ensure_build_memory

	local work panel_src olcrtc_src
	work="$(mktemp -d /tmp/olcrtc-manager-install.XXXXXX)"
	trap 'rm -rf "${work:-}"' EXIT
	panel_src="$work/panel"
	olcrtc_src="$work/olcrtc"

	clone_repo "$OLCRTC_REPO" "$OLCRTC_REF" "$olcrtc_src"
	clone_repo "$PANEL_REPO" "$PANEL_REF" "$panel_src"
	build_olcrtc "$olcrtc_src"
	build_manager "$panel_src"
	detect_manager_features
	write_config_if_missing
	apply_config_port
	write_tls_cert_if_missing
	write_panel_env_if_missing
	install_service
	sync_sources "$panel_src"

	log "done"
	log "service: systemctl status olcrtc-manager"
	log "Access URL: ${DISPLAY_SCHEME}://${DISPLAY_HOST}:${PANEL_PORT}${DISPLAY_ADMIN_PATH}"
	log "WebBasePath: ${DISPLAY_ADMIN_PATH#/}"
	if [ "$GENERATED_PANEL_ENV" = "1" ]; then
		log "Username: $GENERATED_ADMIN_USER"
		log "Password: $GENERATED_ADMIN_PASS"
	fi
	if [ "$PANEL_ADDR" = "127.0.0.1" ]; then
		log "the panel listens locally; reinstall with PANEL_ADDR=0.0.0.0 to expose it directly"
	fi
	if [ "$PANEL_TLS" = "1" ]; then
		log "TLS uses a self-signed certificate by default; browsers may ask you to accept it."
	fi
}

main "$@"
