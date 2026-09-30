#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'TEXT'
NodeBridge Linux/systemd 卸载（Web + SSH 转发）
  sudo bash uninstall.sh node
  sudo bash uninstall.sh hub
  sudo bash uninstall.sh all --purge

默认保留配置和数据库；--purge 永久删除所选模式的数据及配对信息。
同机另一模式仍安装时保留共享二进制和系统账号。
TEXT
}

[[ ${1:-} == -h || ${1:-} == --help ]] && { usage; exit 0; }
mode=${1:-}
[[ $mode == hub || $mode == node || $mode == all ]] || { usage; exit 1; }
shift
purge=false
if [[ ${1:-} == --purge ]]; then purge=true; shift; fi
[[ $# == 0 ]] || { usage; exit 1; }
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo '请在 Linux 上使用 sudo 运行。' >&2; exit 1; }
command -v systemctl >/dev/null || { echo '需要 systemd。' >&2; exit 1; }

modes=("$mode")
[[ $mode != all ]] || modes=(hub node)
for selected in "${modes[@]}"; do
  unit=nodebridge-$selected.service
  # Fail before deleting anything if a running service could not be stopped.
  if [[ -e /etc/systemd/system/$unit ]] || systemctl cat "$unit" >/dev/null 2>&1; then
    systemctl stop "$unit"
    systemctl disable "$unit"
    rm -f -- "/etc/systemd/system/$unit"
    rm -rf -- "/etc/systemd/system/$unit.d"
  fi
done
systemctl daemon-reload
for selected in "${modes[@]}"; do
  if systemctl is-active --quiet "nodebridge-$selected.service"; then
    echo "服务仍在运行，停止卸载：nodebridge-$selected.service" >&2
    exit 1
  fi
  if $purge; then rm -rf -- "/var/lib/nodebridge/$selected"; fi
done

# Both modes use this binary. Never remove it while either unit remains installed.
if ! systemctl cat nodebridge-hub.service >/dev/null 2>&1 &&
   ! systemctl cat nodebridge-node.service >/dev/null 2>&1; then
  rm -f -- /usr/local/bin/nodebridge /usr/local/bin/nodebridge.new
  # Retained data needs the same uid when reinstalling.
  if [[ ! -e /var/lib/nodebridge/hub && ! -e /var/lib/nodebridge/node ]]; then
    rmdir -- /var/lib/nodebridge 2>/dev/null || true
    if id nodebridge >/dev/null 2>&1; then userdel nodebridge; fi
    if getent group nodebridge >/dev/null 2>&1; then groupdel nodebridge; fi
  fi
fi
echo "已卸载 $mode 的 Web 与 SSH 转发服务。"
if $purge; then
  echo '所选模式的配置、凭证和数据库已删除；再次安装需要重新配对。'
else
  echo '数据保留在 /var/lib/nodebridge/，重新安装可恢复。'
fi
