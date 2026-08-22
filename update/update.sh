#!/bin/bash
# CPA 一键更新脚本 — 备份配置、拉取新镜像、重建容器、验证
set -uo pipefail

LOG=/opt/cpa-update/update.log
: > "$LOG"

echo "===== CPA Update $(date '+%Y-%m-%d %H:%M:%S') =====" >> "$LOG"

# 1. 备份
BK=/root/backups/cpamp-$(date +%Y%m%d-%H%M%S)
mkdir -p "$BK"
cp -a /root/cpa-manager-plus/.env /root/cpa-manager-plus/compose.yaml /root/cpa-manager-plus/cliproxyapi/config.yaml "$BK/" 2>>"$LOG"
echo "[1/4] 配置备份 -> $BK" >> "$LOG"

# 2. 拉取新镜像
echo "[2/4] docker compose pull ..." >> "$LOG"
cd /root/cpa-manager-plus || { echo "ERROR: cd failed" >> "$LOG"; exit 1; }
docker compose pull >> "$LOG" 2>&1
echo "[2/4] pull 完成" >> "$LOG"

# 3. 重建容器
echo "[3/4] docker compose up -d ..." >> "$LOG"
docker compose up -d >> "$LOG" 2>&1
echo "[3/4] up -d 完成" >> "$LOG"

# 4. 验证
sleep 6
V=$(docker logs cpamp-cli-proxy-api-1 2>&1 | grep -oE "CLIProxyAPI Version: v[0-9.]+" | tail -1)
S=$(docker ps --format "{{.Names}} {{.Status}}" | grep cpamp)
echo "[4/4] 内核: ${V:-未获取到}" >> "$LOG"
echo "$S" >> "$LOG"
echo "===== DONE =====" >> "$LOG"
