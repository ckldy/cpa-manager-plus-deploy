#!/bin/bash
# ============================================================
# CPA-Manager-Plus 一键安装脚本
#
# 用法:
#   ./scripts/install.sh            # 手动模式：需先准备 .env / config / secrets
#   ./scripts/install.sh --auto     # 自动模式：缺失的 .env / config / secrets 自动生成（curl|bash 用）
#
# 作用:
#   按服务器标准目录结构布局 → 校验/生成配置 → docker compose up -d → 打印验证命令
# ============================================================
set -euo pipefail

AUTO="${1:-}"
cd "$(dirname "$0")/.."
ROOT="$(pwd)"
echo ">>> 工作目录: $ROOT"

# ================= 辅助函数 =================
gen_rand() { # 生成随机十六进制串（无 openssl 时用 /dev/urandom 兜底）
  local n="${1:-24}"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$n"
  else
    head -c "$((n*2))" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

gen_bcrypt() { # 生成 bcrypt hash（多级探测），失败返回空
  local plain="$1"
  if command -v htpasswd >/dev/null 2>&1; then
    htpasswd -bnBC 10 "" "$plain" 2>/dev/null | tr -d ':\n' | sed 's/^/ /'
    return 0
  fi
  if command -v python3 >/dev/null 2>&1 && python3 -c "import bcrypt" 2>/dev/null; then
    python3 -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt(10)).decode())" "$plain" 2>/dev/null
    return 0
  fi
  return 1
}

# ================= 1. .env =================
if [ ! -f .env ]; then
  if [ "$AUTO" = "--auto" ] && [ -f .env.example ]; then
    cp .env.example .env
    # 自动生成 GITHUB_TOKEN 占位（若无真实 PAT，插件 store 限流规避会失效，但不阻塞启动）
    echo "!! [自动] 已从 .env.example 生成 .env —— GITHUB_TOKEN 为空"
  else
    echo "!! 缺少 .env，请先执行: cp .env.example .env  然后填写 GITHUB_TOKEN 等"
    exit 1
  fi
fi

# ================= 2. 目录布局 =================
echo ">>> 布局 cliproxyapi/ 目录..."
mkdir -p cliproxyapi/auths cliproxyapi/logs cliproxyapi/plugins

# ================= 3. config.yaml =================
if [ ! -f cliproxyapi/config.yaml ]; then
  if [ "$AUTO" = "--auto" ] && [ -f config/config.yaml.example ]; then
    cp config/config.yaml.example cliproxyapi/config.yaml
    # 自动填充 api-keys（随机 sk- key）
    NEWKEY="sk-$(gen_rand 24)"
    sed -i.bak "s/^  - \"\"$/  - \"$NEWKEY\"/" cliproxyapi/config.yaml && rm -f cliproxyapi/config.yaml.bak
    # 自动生成 secret-key（bcrypt，若工具可用）
    if SECRET_PLAIN=$(gen_rand 24) && SECRET_HASH=$(gen_bcrypt "$SECRET_PLAIN"); then
      sed -i.bak "s|secret-key: \"\"|secret-key: \"$SECRET_HASH\"|" cliproxyapi/config.yaml && rm -f cliproxyapi/config.yaml.bak
    fi
    echo "!! [自动] 已生成 cliproxyapi/config.yaml"
    echo "       API Key: $NEWKEY"
    [ -n "${SECRET_HASH:-}" ] && echo "       remote-management.secret-key: 已生成 bcrypt（明文=$(gen_rand 6)...）"
    echo "       ★ 请立即保存上面的 API Key！它只在本次安装显示一次。"
  else
    echo "!! 缺少 config/config.yaml.example，无法生成配置"; exit 1
  fi
fi

# ================= 4. secrets =================
if [ ! -f secrets/cpamp-admin-key ] || [ ! -f secrets/cpa-management-key ]; then
  if [ "$AUTO" = "--auto" ]; then
    [ -d secrets ] || mkdir -p secrets
    [ -f secrets/cpamp-admin-key ] || { echo "cpamp_$(gen_rand 20)" > secrets/cpamp-admin-key; echo "!! [自动] 已生成 secrets/cpamp-admin-key"; }
    [ -f secrets/cpa-management-key ] || { echo "cpa_$(gen_rand 20)" > secrets/cpa-management-key; echo "!! [自动] 已生成 secrets/cpa-management-key"; }
  else
    echo "!! 缺少 secrets 密钥文件:"
    echo "     secrets/cpamp-admin-key     (面板管理员密钥)"
    echo "     secrets/cpa-management-key  (面板 API 管理密钥)"
    echo "    可从备份还原，或手动创建（内容为任意随机串）"
    echo "    或改用: ./scripts/install.sh --auto 自动生成"
    exit 1
  fi
fi
chmod 600 secrets/cpamp-admin-key secrets/cpa-management-key 2>/dev/null || true

# ================= 5. 校验配置 =================
if grep -qE 'secret-key: ""' cliproxyapi/config.yaml; then
  echo "!! 警告: config.yaml 的 remote-management.secret-key 仍为空"
  echo "   （面板远程管理鉴权不可用；如需可用，用 htpasswd/python bcrypt 生成后填入）"
fi
if grep -qE '^  - ""$' cliproxyapi/config.yaml; then
  echo "!! 警告: config.yaml 的 api-keys 仍为空 —— 调用 /v1 会 401"
fi

# ================= 6. 启动 =================
echo ">>> docker compose up -d ..."
docker compose up -d
sleep 5

# ================= 7. 验证 =================
echo ""
echo "================ 部署完成 ================"
echo "容器状态:"
docker compose ps
echo ""
API_KEY=$(grep -E '^  - "sk-' cliproxyapi/config.yaml | grep -oE 'sk-[A-Za-z0-9]+' | head -1)
echo "验证 CLIProxyAPI (本机):"
echo "  curl -H \"Authorization: Bearer ${API_KEY:-<你的API_KEY>}\" http://127.0.0.1:${CPA_PORT:-8317}/v1/models"
echo "面板地址:"
echo "  http://<服务器IP>:${CPAMP_PORT:-18317}"
echo "  - 登录面板后，到【模型/凭据】页扫码登录 workbuddy / qoderwork 即可用模型"
echo "=========================================="
