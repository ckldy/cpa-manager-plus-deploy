#!/bin/bash
# ============================================================
# CPA-Manager-Plus 一键安装脚本
# 用法:  ./scripts/install.sh
# 前置:  已 clone 本仓库; 已 cp .env.example .env 并填写; 已放好密钥
# 作用:  按服务器标准目录结构布局 → 校验配置 → docker compose up -d → 打印验证命令
# ============================================================
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
echo ">>> 工作目录: $ROOT"

# ---- 1. 检查 .env ----
if [ ! -f .env ]; then
  echo "!! 缺少 .env，请先执行: cp .env.example .env  然后填写 GITHUB_TOKEN 等"
  exit 1
fi

# ---- 2. 按服务器目录结构布局（compose.yaml 引用 ./cliproxyapi/...）----
echo ">>> 布局 cliproxyapi/ 目录..."
mkdir -p cliproxyapi/auths cliproxyapi/logs cliproxyapi/plugins
if [ ! -f cliproxyapi/config.yaml ]; then
  if [ -f config/config.yaml.example ]; then
    cp config/config.yaml.example cliproxyapi/config.yaml
    echo "!! 已从模板生成 cliproxyapi/config.yaml —— 请编辑它填入 api-keys 与 secret-key！"
  else
    echo "!! 缺少 config/config.yaml.example，无法生成配置"; exit 1
  fi
fi

# ---- 3. 检查 secrets ----
if [ ! -f secrets/cpamp-admin-key ] || [ ! -f secrets/cpa-management-key ]; then
  echo "!! 缺少 secrets 密钥文件:"
  echo "     secrets/cpamp-admin-key     (面板管理员密钥)"
  echo "     secrets/cpa-management-key  (面板 API 管理密钥)"
  echo "    可从加密备份还原，或手动创建（内容为任意随机串）"
  exit 1
fi
chmod 600 secrets/cpamp-admin-key secrets/cpa-management-key

# ---- 4. 校验 config.yaml 是否还有占位空值 ----
if grep -qE 'secret-key: ""' cliproxyapi/config.yaml; then
  echo "!! 警告: config.yaml 的 remote-management.secret-key 仍为空"
fi
if grep -qE '^  - ""$' cliproxyapi/config.yaml; then
  echo "!! 警告: config.yaml 的 api-keys 仍为空 —— 调用 /v1 会 401"
fi

# ---- 5. 启动 ----
echo ">>> docker compose up -d ..."
docker compose up -d
sleep 5

# ---- 6. 打印验证 ----
echo ""
echo "================ 部署完成 ================"
echo "容器状态:"
docker compose ps
echo ""
API_KEY=$(grep -A2 'api-keys:' cliproxyapi/config.yaml | grep -oE 'sk-[A-Za-z0-9]+' | head -1)
echo "验证 CLIProxyAPI (本机):"
echo "  curl -H \"Authorization: Bearer ${API_KEY:-<你的API_KEY>}\" http://127.0.0.1:${CPA_PORT:-8317}/v1/models"
echo "面板地址:"
echo "  http://<服务器IP>:${CPAMP_PORT:-18317}"
echo "=========================================="
