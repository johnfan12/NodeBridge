#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'TEXT'
NodeBridge Linux/systemd 安装
  sudo ./install.sh hub --public-url https://VPS_IP:9443
  sudo ./install.sh node
  sudo bash scripts/install.sh hub --binary ./bin/nodebridge --public-url https://VPS_IP:9443

可选：--binary /path/to/nodebridge，其余参数传递给 nodebridge。
联网安装：设置 NODEBRIDGE_RELEASE_BASE_URL 为 Release 资产下载地址。
例如 https://github.com/你的账号/NodeBridge/releases/download/v0.1.0
TEXT
}

[[ ${1:-} == -h || ${1:-} == --help ]] && { usage; exit 0; }
mode=${1:-}
[[ $mode == hub || $mode == node ]] || { usage; exit 1; }
shift
[[ $(uname -s) == Linux ]] || { echo '安装服务需要 Linux；其他系统可直接运行二进制。' >&2; exit 1; }
[[ $EUID == 0 ]] || { echo '请使用 sudo 运行安装脚本。' >&2; exit 1; }
command -v systemctl >/dev/null || { echo '需要 systemd；可直接运行 nodebridge。' >&2; exit 1; }
command -v runuser >/dev/null || { echo '需要 util-linux 提供的 runuser。' >&2; exit 1; }

binary=''
args=()
while (($#)); do
  case "$1" in
    --binary) [[ $# -ge 2 ]] || exit 1; binary=$2; shift 2 ;;
    --data-dir|--data-dir=*|--init-only|--init-only=*) echo '安装模式固定使用 /var/lib/nodebridge/<mode>，不接受此参数。' >&2; exit 1 ;;
    *) args+=("$1"); shift ;;
  esac
done

script_dir=''
if [[ -n ${BASH_SOURCE[0]:-} ]]; then
  script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  if [[ -z $binary && -f $script_dir/nodebridge ]]; then binary=$script_dir/nodebridge; fi
  if [[ -z $binary && -f $script_dir/../bin/nodebridge ]]; then binary=$script_dir/../bin/nodebridge; fi
fi

temp_dir=''
trap '[[ -z $temp_dir ]] || rm -rf -- "$temp_dir"' EXIT
if [[ -z $binary ]]; then
  base=${NODEBRIDGE_RELEASE_BASE_URL:-}
  [[ $base == https://* ]] || { echo '未找到二进制。请使用 Release 安装包、--binary，或设置 NODEBRIDGE_RELEASE_BASE_URL。' >&2; exit 1; }
  case $(uname -m) in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo '支持 amd64 和 arm64。' >&2; exit 1 ;; esac
  temp_dir=$(mktemp -d)
  asset=nodebridge-linux-$arch.tar.gz
  curl --proto '=https' --tlsv1.2 -fsSL "${base%/}/$asset" -o "$temp_dir/$asset"
  curl --proto '=https' --tlsv1.2 -fsSL "${base%/}/SHA256SUMS" -o "$temp_dir/SHA256SUMS"
  # Extract only the expected checksum; never execute or trust paths from a checksum file.
  checksum=$(awk -v name="$asset" '$2 == name && $1 ~ /^[0-9a-f]+$/ && length($1) == 64 { print $1 }' "$temp_dir/SHA256SUMS")
  [[ ${#checksum} == 64 ]] || { echo '缺少有效的发布校验和。' >&2; exit 1; }
  actual=$(sha256sum "$temp_dir/$asset"); actual=${actual%% *}
  [[ $actual == "$checksum" ]] || { echo '发布文件校验失败。' >&2; exit 1; }
  tar -xzf "$temp_dir/$asset" -C "$temp_dir" nodebridge
  binary=$temp_dir/nodebridge
fi
[[ -f $binary && -x $binary ]] || { echo '未找到可执行的 nodebridge。' >&2; exit 1; }

target=/usr/local/bin/nodebridge
dir=/var/lib/nodebridge/$mode
if ! id nodebridge >/dev/null 2>&1; then useradd --system --home-dir /var/lib/nodebridge --shell /usr/sbin/nologin nodebridge; fi
install -d -m 700 -o nodebridge -g nodebridge "$dir"
if [[ $(readlink -f "$binary") != "$target" ]]; then
  install -m 755 "$binary" "$target.new"
  mv -f "$target.new" "$target"
fi

# Validate/init before replacing service configuration. The service itself runs unprivileged.
runuser -u nodebridge -- "$target" "$mode" --data-dir "$dir" --init-only "${args[@]}"

cat > "/etc/systemd/system/nodebridge-$mode.service" <<UNIT
[Unit]
Description=NodeBridge $mode
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=nodebridge
Group=nodebridge
ExecStart=$target $mode --data-dir $dir
Restart=always
RestartSec=3
TimeoutStopSec=10
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$dir
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable "nodebridge-$mode.service"
systemctl restart "nodebridge-$mode.service"
systemctl is-active --quiet "nodebridge-$mode.service"
echo "安装完成：nodebridge-$mode.service"
if [[ $mode == hub ]]; then
  if [[ -f $dir/initial-admin.txt ]]; then
    echo '首次管理员账号：'
    cat "$dir/initial-admin.txt"
  else
    echo '管理员账号已初始化，请使用已有密码。'
  fi
  echo '浏览器访问配置的 HTTPS 地址。默认使用自签证书，首次访问需核对证书指纹。'
  echo "VPS 防火墙/云安全组需放行控制台端口（默认 9443）和配置的 SSH 端口池（默认 30000–39999）。"
else
  echo '执行 nodebridge pair，粘贴 VPS 控制台生成的配对链接即可。'
  echo '本地维护页面：http://127.0.0.1:9899'
fi
