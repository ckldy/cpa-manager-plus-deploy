#!/bin/bash
# ============================================================
# CPA 还原脚本 —— 在【新机】上执行
# 用法:  ./scripts/restore.sh <备份文件路径> [加密口令]
#   明文包:  restore.sh /path/cpa-full-xxxx.tar.gz
#   加密包:  restore.sh /path/cpa-full-xxxx.tar.gz.enc <口令>
# 作用:  解包 → 布局到 cliproxyapi/ → 还原数据卷 → docker compose up -d
# ============================================================
set -euo pipefail

SRC="${1:?用法: restore.sh <备份tar.gz[.enc]> [加密口令]}"
PASS="${2:-}"
TS=$(date +%Y%m%d-%H%M%S)
STAGE="/tmp/cpa-restore-$TS"
mkdir -p "$STAGE"

# 1. 解密(如需)
DEC="$SRC"
if [[ "$SRC" == *.enc ]]; then
  [ -z "$PASS" ] && { echo "!! 加密包需要口令"; exit 1; }
  DEC="$STAGE/backup.tar.gz"
  openssl enc -d -aes-256-cbc -pbkdf2 -in "$SRC" -out "$DEC" -pass pass:"$PASS"
  echo ">>> 解密完成"
fi

# 2. 解包
tar xzf "$DEC" -C "$STAGE"
CONTENT_DIR=$(find "$STAGE" -mindepth 1 -maxdepth 1 -type d | head -1)
echo ">>> 解包目录: $CONTENT_DIR"

# 3. 布局
ROOT="$(pwd)"
mkdir -p cliproxyapi/auths cliproxyapi/logs cliproxyapi/plugins secrets
[ -f "$CONTENT_DIR/.env" ] && cp -a "$CONTENT_DIR/.env" .env
[ -f "$CONTENT_DIR/compose.yaml" ] && cp -a "$CONTENT_DIR/compose.yaml" compose.yaml
[ -f "$CONTENT_DIR/config.yaml" ] && cp -a "$CONTENT_DIR/config.yaml" cliproxyapi/config.yaml
[ -d "$CONTENT_DIR/secrets" ] && cp -a "$CONTENT_DIR/secrets/." secrets/
[ -d "$CONTENT_DIR/plugins" ] && cp -a "$CONTENT_DIR/plugins/." cliproxyapi/plugins/
chmod 600 secrets/cpamp-admin-key secrets/cpa-management-key 2>/dev/null || true

# 4. 数据卷（历史用量统计）
if [ -f "$CONTENT_DIR/cpamp-data.tar.gz" ]; then
  docker volume create cpa-manager-plus-data >/dev/null 2>&1 || true
  docker run --rm \
    -v cpa-manager-plus-data:/data \
    -v "$CONTENT_DIR":/backup \
    alpine tar xzf /backup/cpamp-data.tar.gz -C /data
  echo ">>> 数据卷已还原"
fi

# 5. 启动
echo ">>> docker compose up -d ..."
docker compose up -d
sleep 5
docker compose ps

# 清理
rm -rf "$STAGE"
echo "================ 还原完成 ================"
echo "提示: 第三方账号(workbuddy/qoderwork)的登录 token 有时效，"
echo "      建议到面板【模型/凭据】页重新扫码登录以获取新 token。"
echo "=========================================="
