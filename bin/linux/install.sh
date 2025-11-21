#!/usr/bin/env bash
# 安装 Ftcp 服务端为 systemd 服务，收集必填参数并默认开机启动

set -euo pipefail

# 运行前检查 root
if [[ $EUID -ne 0 ]]; then
  echo "请以 root 权限运行 (使用 sudo)。"
  exit 1
fi

# 默认值
SERVER_ADDR=":7000"
HTTP_ADDR=":7001"
ADMIN_PASS=""
DB_PATH="/var/lib/ftcp/ftcp.db"
BIN_SRC="$(dirname "$0")/ftcps-amd64"
BIN_DST="/usr/local/bin/ftcps"
SERVICE_NAME="ftcps"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
DATA_DIR="$(dirname "$DB_PATH")"

# 检查是否已安装
if systemctl is-active --quiet "${SERVICE_NAME}"; then
  echo "服务 ${SERVICE_NAME} 已经安装并正在运行。"
  echo "请选择操作:"
  echo "1) 重新安装服务"
  echo "2) 卸载服务"
  echo "3) 退出"
  read -r -p "请输入选择 (1-3): " choice
  case "$choice" in
    1)
      echo "重新安装服务..."
      ;;
    2)
      read -r -p "确认卸载服务并删除相关文件? (y/n): " confirm
      if [[ "$confirm" == "y" || "$confirm" == "Y" ]]; then
        # 执行卸载操作
        systemctl disable --now "${SERVICE_NAME}"
        rm -f "$SERVICE_FILE"
        rm -f "$BIN_DST"
        systemctl daemon-reload
        echo "服务已卸载并删除相关文件。"
      else
        echo "取消卸载。"
      fi
      exit 0
      ;;
    3)
      exit 0
      ;;
    *)
      echo "无效选择，退出。"
      exit 0
      ;;
  esac
fi

# 解析命令行参数
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server) SERVER_ADDR="$2"; shift 2;;
    --http) HTTP_ADDR="$2"; shift 2;;
    --adminpass) ADMIN_PASS="$2"; shift 2;;
    --db) DB_PATH="$2"; shift 2;;
    --bin) BIN_SRC="$2"; shift 2;;
    *) echo "未知参数: $1"; exit 1;;
  esac
done

# 交互式输入
read -r -p "服务端监听地址 [默认 :7000]: " inp || true
SERVER_ADDR="${inp:-$SERVER_ADDR}"

read -r -p "监控HTTP地址 [默认 :7001]: " inp || true
HTTP_ADDR="${inp:-$HTTP_ADDR}"

read -r -p "管理登录密码（留空随机生成）: " inp || true
ADMIN_PASS="${inp:-$ADMIN_PASS}"

read -r -p "授权数据库路径 [默认 $DB_PATH]: " inp || true
DB_PATH="${inp:-$DB_PATH}"
DATA_DIR="$(dirname "$DB_PATH")"

# 校验二进制
if [[ ! -f "$BIN_SRC" ]]; then
  echo "未找到服务端二进制: $BIN_SRC"
  exit 1
fi

# 拷贝二进制
install -Dm0755 "$BIN_SRC" "$BIN_DST"

# 准备数据目录与权限
install -d -m 0755 "$DATA_DIR"

# 随机密码
gen_pass() {
  # 生成16字节随机base64url字符
  head -c 16 /dev/urandom | base64 | tr -d '\n' | tr '+/' '-_' 
}

if [[ -z "$ADMIN_PASS" ]]; then
  ADMIN_PASS="$(gen_pass)"
  echo "已生成随机管理密码: $ADMIN_PASS"
fi

# 写入 systemd 服务单元
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Ftcp Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN_DST} -server ${SERVER_ADDR} -http ${HTTP_ADDR} -adminpass ${ADMIN_PASS} -db ${DB_PATH}
Restart=on-failure
RestartSec=3s
# 可选超时
TimeoutStartSec=30s
TimeoutStopSec=30s

# 运行用户/组（如需非root，请预先创建用户并赋予数据目录权限）
User=root
Group=root

[Install]
WantedBy=multi-user.target
EOF

# 刷新并启用服务
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

# 显示状态与摘要
echo "安装完成："
echo "- 二进制：$BIN_DST"
echo "- 数据库：$DB_PATH"
echo "- 服务：$SERVICE_NAME (已启用开机启动)"
echo "- 监听：server=${SERVER_ADDR} http=${HTTP_ADDR}"
echo "- 管理密码：${ADMIN_PASS}"
echo
systemctl --no-pager status "${SERVICE_NAME}" || true

# 提供管理界面
manage_service() {
  echo "请选择操作:"
  echo "1) 查看服务状态"
  echo "2) 重启服务"
  echo "3) 停止服务"
  echo "4) 启动服务"
  echo "5) 禁用开机启动"
  echo "6) 启用开机启动"
  echo "7) 卸载服务"
  echo "8) 退出"
  
  read -r -p "请输入选择 (1-8): " choice
  case "$choice" in
    1)
      systemctl --no-pager status "${SERVICE_NAME}"
      manage_service
      ;;
    2)
      systemctl restart "${SERVICE_NAME}"
      echo "服务已重启"
      manage_service
      ;;
    3)
      systemctl stop "${SERVICE_NAME}"
      echo "服务已停止"
      manage_service
      ;;
    4)
      systemctl start "${SERVICE_NAME}"
      echo "服务已启动"
      manage_service
      ;;
    5)
      systemctl disable "${SERVICE_NAME}"
      echo "已禁用开机启动"
      manage_service
      ;;
    6)
      systemctl enable "${SERVICE_NAME}"
      echo "已启用开机启动"
      manage_service
      ;;
    7)
      read -r -p "确认卸载服务并删除相关文件? (y/n): " confirm
      if [[ "$confirm" == "y" || "$confirm" == "Y" ]]; then
        # 执行卸载操作
        systemctl disable --now "${SERVICE_NAME}"
        rm -f "$SERVICE_FILE"
        rm -f "$BIN_DST"
        systemctl daemon-reload
        echo "服务已卸载并删除相关文件。"
      else
        echo "取消卸载。"
      fi
      manage_service
      ;;
    8)
      echo "退出"
      exit 0
      ;;
    *)
      echo "无效选择，请重新选择。"
      manage_service
      ;;
  esac
}

# 启动管理界面
manage_service
