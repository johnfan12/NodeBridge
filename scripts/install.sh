#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'TEXT'
NodeBridge Linux/systemd 安装
  sudo ./install.sh hub --public-url https://VPS_IP:9443
  sudo ./install.sh node
  sudo ./install.sh hub --public-url https://ssh.example.com --reconfigure
  sudo bash scripts/install.sh hub --binary ./bin/nodebridge --public-url https://VPS_IP:9443

可选：--binary /path/to/nodebridge，其余参数传递给 nodebridge。
联网安装：默认下载 https://github.com/johnfan12/NodeBridge/releases/latest/download
可选：用 NODEBRIDGE_RELEASE_BASE_URL 指定其他 Release 或镜像地址。
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
command -v flock >/dev/null || { echo '需要 util-linux 提供的 flock。' >&2; exit 1; }

binary=''
args=()
while (($#)); do
  case "$1" in
    --binary) [[ $# -ge 2 ]] || exit 1; binary=$2; shift 2 ;;
    --data-dir|--data-dir=*|--init-only|--init-only=*) echo '安装模式固定使用 /var/lib/nodebridge/<mode>，不接受此参数。' >&2; exit 1 ;;
    *) args+=("$1"); shift ;;
  esac
done

# Check the shared install directory before downloading or changing any service.
# Its group ownership and write policy may be intentional on multi-user hosts.
check_root_path() {
  local path=$1 owner perms
  owner=$(stat -c %u -- "$path")
  perms=$(stat -c %a -- "$path")
  if [[ $owner != 0 ]] || (( (8#$perms & 022) != 0 )); then
    echo "$path 的 uid=$owner 权限=$perms。面板升级要求 root 所有且组和其他用户不可写。" >&2
    echo "请确认此路径的共享策略后再执行：sudo chown root -- '$path' && sudo chmod go-w -- '$path'" >&2
    return 1
  fi
}
target=/usr/local/bin/nodebridge
dir=/var/lib/nodebridge/$mode
if [[ ! -d /usr/local/bin || -L /usr/local/bin ]]; then
  echo '/usr/local/bin 必须是实际目录，请检查安装路径。' >&2
  exit 1
fi
check_root_path /usr/local/bin
for path in /var/lib/nodebridge "$dir" /var/lib/nodebridge-updater "/var/lib/nodebridge-updater/$mode" "/var/lib/nodebridge-updater/$mode/private"; do
  if [[ -L $path || ( -e $path && ! -d $path ) ]]; then
    echo "$path 必须是实际目录，请检查安装路径。" >&2
    exit 1
  fi
done
if [[ -L $target || ( -e $target && ! -f $target ) ]]; then
  echo "$target 必须是普通文件，请检查安装路径。" >&2
  exit 1
fi

script_dir=''
if [[ -n ${BASH_SOURCE[0]:-} ]]; then
  script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  if [[ -z $binary && -f $script_dir/nodebridge ]]; then binary=$script_dir/nodebridge; fi
  if [[ -z $binary && -f $script_dir/../bin/nodebridge ]]; then binary=$script_dir/../bin/nodebridge; fi
fi

temp_dir=''
trap '[[ -z $temp_dir ]] || rm -rf -- "$temp_dir"' EXIT
if [[ -z $binary ]]; then
  base=${NODEBRIDGE_RELEASE_BASE_URL:-https://github.com/johnfan12/NodeBridge/releases/latest/download}
  [[ $base == https://* ]] || { echo '未找到二进制。请使用 Release 安装包、--binary，或设置 NODEBRIDGE_RELEASE_BASE_URL。' >&2; exit 1; }
  case $(uname -m) in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo '支持 amd64 和 arm64。' >&2; exit 1 ;; esac
  temp_dir=$(mktemp -d)
  asset=nodebridge-linux-$arch.tar.gz
  download() {
    local attempt
    for attempt in 1 2 3; do
      if curl --proto '=https' --tlsv1.2 --http1.1 --connect-timeout 15 --max-time 180 -fsSL "$1" -o "$2"; then return 0; fi
      echo "下载失败，正在重试（$attempt/3）：$1" >&2
      sleep 2
    done
    return 1
  }
  download "${base%/}/$asset" "$temp_dir/$asset"
  download "${base%/}/SHA256SUMS" "$temp_dir/SHA256SUMS"
  # Extract only the expected checksum; never execute or trust paths from a checksum file.
  checksum=$(awk -v name="$asset" '$2 == name && $1 ~ /^[0-9a-f]+$/ && length($1) == 64 { print $1 }' "$temp_dir/SHA256SUMS")
  [[ ${#checksum} == 64 ]] || { echo '缺少有效的发布校验和。' >&2; exit 1; }
  actual=$(sha256sum "$temp_dir/$asset"); actual=${actual%% *}
  [[ $actual == "$checksum" ]] || { echo '发布文件校验失败。' >&2; exit 1; }
  tar -xzf "$temp_dir/$asset" -C "$temp_dir" nodebridge
  binary=$temp_dir/nodebridge
fi
[[ -f $binary && -x $binary ]] || { echo '未找到可执行的 nodebridge。' >&2; exit 1; }

# Serialize manual installs with panel updates on the shared binary.
previous_umask=$(umask)
umask 077
exec 9>/run/nodebridge-update.lock
umask "$previous_umask"
flock -x 9

if ! id nodebridge >/dev/null 2>&1; then useradd --system --home-dir /var/lib/nodebridge --shell /usr/sbin/nologin nodebridge; fi
install -d -m 755 -o root -g root /var/lib/nodebridge
install -d -m 700 -o nodebridge -g nodebridge "$dir"
if [[ $(readlink -f "$binary") != "$target" ]]; then
  staged=$(mktemp /usr/local/bin/.nodebridge-XXXXXXXX)
  install -o root -g root -m 755 "$binary" "$staged"
  mv -fT "$staged" "$target"
else
  check_root_path "$target"
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

# The Web process queues only a version and job id. A separate root service
# downloads from the fixed official repository and verifies the archive again.
install -d -m 755 -o root -g root /var/lib/nodebridge-updater "/var/lib/nodebridge-updater/$mode"
install -d -m 700 -o root -g root "/var/lib/nodebridge-updater/$mode/private"
cat > "/etc/systemd/system/nodebridge-update-$mode.service" <<UNIT
[Unit]
Description=NodeBridge verified updater ($mode)
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=3600
StartLimitBurst=6

[Service]
Type=oneshot
User=root
ExecStart=$target update-worker --mode $mode
TimeoutStartSec=11min
UMask=0022
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/usr/local/bin /var/lib/nodebridge /var/lib/nodebridge-updater /run
UNIT
cat > "/etc/systemd/system/nodebridge-update-$mode.path" <<UNIT
[Unit]
Description=NodeBridge update requests ($mode)

[Path]
PathExists=$dir/update-request.json
Unit=nodebridge-update-$mode.service

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
for unit in "nodebridge-update-$mode.service" "nodebridge-update-$mode.path"; do
  if systemctl is-failed --quiet "$unit"; then systemctl reset-failed "$unit"; fi
done
systemctl enable --now "nodebridge-update-$mode.path"
if ! runuser -u nodebridge -- /bin/systemctl is-active --quiet "nodebridge-update-$mode.path"; then
  echo "nodebridge-update-$mode.path 未就绪，请查看 journalctl -u nodebridge-update-$mode.service" >&2
  exit 1
fi
systemctl enable "nodebridge-$mode.service"
systemctl restart "nodebridge-$mode.service"
systemctl is-active --quiet "nodebridge-$mode.service"
echo "安装完成：nodebridge-$mode.service"
echo '面板升级执行器已启用；管理员可在“版本更新”页检查并手动升级。'
if [[ $mode == hub ]]; then
  if [[ -f $dir/initial-admin.txt ]]; then
    echo '首次管理员账号：'
    cat "$dir/initial-admin.txt"
  else
    echo '管理员账号已初始化，请使用已有密码。'
  fi
  echo '直接访问 Hub 使用自签 HTTPS；域名反向代理使用 Nginx 的有效域名证书。'
  echo '注意：Hub 的 9443 是 HTTPS，Nginx 后端必须使用 https://，并传递 WebSocket Upgrade 头。'
  echo '生成 Nginx location 配置（加入已有 HTTPS server，替换旧 location /）：'
  echo "  sudo nodebridge nginx --data-dir $dir"
  echo "VPS 防火墙/云安全组需放行对外的 HTTPS 端口（直连默认 9443；Nginx 通常 443）和 SSH 端口池。"
  if ! "$target" doctor --data-dir "$dir"; then
    echo '服务已启动，但公网地址检查未通过；请修正 Nginx/DNS/域名证书后重试：' >&2
    echo "  sudo nodebridge doctor --data-dir $dir" >&2
  fi
else
  echo '执行 nodebridge pair，粘贴 VPS 控制台生成的配对链接即可。'
  echo '本地维护页面：http://127.0.0.1:9899'
fi
