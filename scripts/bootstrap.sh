#!/bin/bash
# ============================================================
# CPA-Manager-Plus curl | bash 一键安装引导脚本
#
# 用法（在目标服务器上执行）:
#   curl -fsSL https://raw.githubusercontent.com/ckldy/cpa-manager-plus-deploy/main/scripts/bootstrap.sh | bash
#
# 作用:
#   1. 下载仓库 tarball（HTTPS 校验）
#   2. 解压到 /opt/cpa-manager-plus
#   3. 以 --auto 模式调用 install.sh（缺失密钥自动生成，全程零交互）
#   4. 打印验证命令
#
# 安全说明:
#   - 仅通过 HTTPS 从 raw.githubusercontent.com 拉取
#   - tarball 来自公开仓库 main 分支，内容与仓库一致
#   - 自动生成的密钥(api-key/admin-key)只写在目标机本地
# ============================================================
set -euo pipefail

REPO_OWNER="ckldy"
REPO_NAME="cpa-manager-plus-deploy"
BRANCH="main"
DEST="/opt/cpa-manager-plus"
TARBALL="/tmp/${REPO_NAME}.tar.gz"

echo "============================================================"
echo " CPA-Manager-Plus 一键部署"
echo " 来源: github.com/${REPO_OWNER}/${REPO_NAME}@${BRANCH}"
echo " 目标: ${DEST}"
echo "============================================================"

# ---- 1. 前置检查 ----
for cmd in curl docker tar; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "!! 缺少命令: $cmd（请先安装）" >&2
    exit 1
  fi
done

if ! docker info >/dev/null 2>&1; then
  echo "!! Docker 未运行或当前用户无权限（请确认 docker 已启动且可访问）" >&2
  exit 1
fi
# compose v2 插件（docker compose）
docker compose version >/dev/null 2>&1 || { echo "!! 需要 Docker Compose v2（docker compose）" >&2; exit 1; }

# ---- 2. 下载仓库 tarball ----
echo ">>> 下载仓库 ..."
curl -fsSL -o "$TARBALL" \
  "https://github.com/${REPO_OWNER}/${REPO_NAME}/archive/refs/heads/${BRANCH}.tar.gz" \
  || { echo "!! 下载失败" >&2; exit 1; }

# 校验是 gzip 压缩包
if ! gzip -t "$TARBALL" 2>/dev/null; then
  echo "!! 下载内容不是有效的 gzip 包，中止（请检查网络/代理）" >&2
  rm -f "$TARBALL"
  exit 1
fi

# ---- 3. 解压到目标目录 ----
echo ">>> 解压到 ${DEST} ..."
mkdir -p "$DEST"
# 用临时目录解压，避免覆盖已有文件出错
TMP_DIR=$(mktemp -d)
tar xzf "$TARBALL" -C "$TMP_DIR"
SRC_DIR="$TMP_DIR/${REPO_NAME}-${BRANCH}"

# 复制（不覆盖已存在的 .env / secrets / cliproxyapi/config.yaml）
cp -rn "$SRC_DIR"/. "$DEST"/ 2>/dev/null || true
chmod +x "$DEST"/scripts/*.sh 2>/dev/null || true
rm -rf "$TMP_DIR" "$TARBALL"

cd "$DEST"

# ---- 4. 一键安装（自动模式）----
echo ">>> 执行安装 ..."
bash "$DEST/scripts/install.sh" --auto

echo ""
echo "安装脚本完成。若上方有红色提示，请按提示补充配置。"
