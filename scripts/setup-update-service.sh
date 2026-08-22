#!/bin/bash
# ============================================================
# 一键更新服务安装脚本（可选组件）—— 在【新机】上执行
# 作用:  把 update/ 下的更新服务装到 /opt/cpa-update + systemd
# 说明:  update_server.py 从 config.yaml 读管理密钥做鉴权（不硬编码）
#        需配合 nginx /cpa-update 端点 + 面板注入脚本使用（见 README）
# ============================================================
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

echo ">>> 安装 /opt/cpa-update ..."
mkdir -p /opt/cpa-update
cp -a update/update.sh update/update_server.py /opt/cpa-update/
chmod +x /opt/cpa-update/update.sh

echo ">>> 安装 systemd 服务 cpa-update.service ..."
cp -a update/cpa-update.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable cpa-update.service
systemctl start cpa-update.service

sleep 1
echo ">>> 服务状态:"
systemctl status cpa-update.service --no-pager | head -8
echo ">>> 健康检查:"
curl -s http://127.0.0.1:18444/health || echo "(本地健康检查失败，请查看日志)"

echo ""
echo "================ 完成 ================"
echo "后续还需(参考 nginx/cpa.vhost.conf.example):"
echo "  1. nginx 加 /cpa-update 与 /cpa-update-inject.js 两个 location"
echo "  2. 面板 HTML sub_filter 注入 cpa-update-inject.js"
echo "======================================"
