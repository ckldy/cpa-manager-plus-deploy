#!/bin/bash
# ============================================================
# CPA 全量备份脚本 —— 在【服务器】上执行
# 用法:  ./scripts/backup.sh [加密口令]
#   不带参数: 生成明文 tar.gz
#   带口令:    生成 openssl AES-256 加密包（推荐，含真实密钥）
# 备份内容: 配置(.env/compose/config) + 密钥(secrets) + 插件(plugins) + 数据卷(usage.sqlite/data.key)
# 输出:     /root/backups/cpa-full-<时间戳>.tar.gz[.enc]
# ============================================================
set -euo pipefail

PASS="${1:-}"
TS=$(date +%Y%m%d-%H%M%S)
BK="/root/backups/cpa-full-$TS"
STAGE="/tmp/cpa-backup-$TS"
mkdir -p "$BK" "$STAGE"

echo ">>> 备份到 $BK"

# 1. 配置 + 密钥
cp -a /root/cpa-manager-plus/.env "$BK/" 2>/dev/null || echo "  (跳过 .env)"
cp -a /root/cpa-manager-plus/compose.yaml "$BK/" 2>/dev/null || echo "  (跳过 compose.yaml)"
cp -a /root/cpa-manager-plus/cliproxyapi/config.yaml "$BK/" 2>/dev/null || echo "  (跳过 config.yaml)"
cp -a /root/cpa-manager-plus/secrets "$BK/" 2>/dev/null || echo "  (跳过 secrets)"

# 2. 插件二进制（auths 里的登录 token 有时效，不打包，新机建议重新扫码登录）
cp -a /root/cpa-manager-plus/cliproxyapi/plugins "$BK/" 2>/dev/null || echo "  (跳过 plugins)"

# 3. 数据卷（usage.sqlite + data.key —— 保留历史用量统计）
docker run --rm \
  -v cpamp_cpa-manager-plus-data:/data \
  -v "$BK":/backup \
  alpine tar czf /backup/cpamp-data.tar.gz -C /data . 2>/dev/null \
  && echo "  (数据卷已打包)" || echo "  !! 数据卷打包失败(容器名/卷名可能不同)"

# 4. 打包
echo ">>> 打包..."
tar czf "$BK.tar.gz" -C /root/backups "$(basename "$BK")"

# 5. 可选加密
if [ -n "$PASS" ]; then
  openssl enc -aes-256-cbc -salt -pbkdf2 \
    -in "$BK.tar.gz" -out "$BK.tar.gz.enc" -pass pass:"$PASS"
  rm -f "$BK.tar.gz"
  echo ">>> 完成（已加密）: $BK.tar.gz.enc"
  echo ">>> 还原口令请牢记，丢失无法解密！"
else
  echo ">>> 完成（明文）: $BK.tar.gz"
  echo ">>> 提示: 含真实密钥，建议用加密方式（传入口令参数）"
fi

# 清理暂存
rm -rf "$STAGE"
ls -lh /root/backups/cpa-full-*
