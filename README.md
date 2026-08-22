# CPA-Manager-Plus 可复现部署模板

> 将 CLIProxyAPI + CPA-Manager-Plus 面板这套 CPA 系统固化为**可复现部署模板**：新机 clone → 填密钥 → 一键起。
> 仓库内**不含任何真实密钥/凭据/服务器信息**，全部为 `.example` 模板；真实密钥由你在新机自行填写。
> 面向公开，欢迎参考部署。

## 组件与版本

| 组件 | 镜像 | 本机当前版本 | 端口 |
|---|---|---|---|
| CLIProxyAPI（内核） | `eceasy/cli-proxy-api:latest` | v7.2.139 | 8317 |
| CPA-Manager-Plus（面板） | `seakee/cpa-manager-plus:latest` | v1.12.2 | 18317 |
| 插件 | workbuddy / qoderwork / privacyfilter | — | — |

> 镜像 tag 用 `latest`，版本会漂移。出问题时按上面记录回滚到对应 tag。

## 🚀 快速部署（curl | bash，全新机器推荐）

前置：x86_64 Linux（Debian/Ubuntu），已装 Docker + Compose v2。

```bash
curl -fsSL https://raw.githubusercontent.com/ckldy/cpa-manager-plus-deploy/main/scripts/bootstrap.sh | bash
```

脚本会：下载仓库 → 解压到 `/opt/cpa-manager-plus` → 自动生成缺失的 `.env` / `config.yaml`（含随机 API Key） / `secrets` → `docker compose up -d`。

安装完成末尾会打印**你的 API Key**（仅本次显示），请立即保存。之后：

```bash
# 验证
curl -H "Authorization: Bearer <API Key>" http://127.0.0.1:8317/v1/models
# 面板（浏览器）
# http://<服务器IP>:18317  → 登录后到【模型/凭据】扫码登录 workbuddy / qoderwork
```

> 从**现有部署迁移**（带原密钥）请看下面「一、手动安装」或「二、备份与还原」。
> `curl | bash` 会执行远程脚本，请确认来源为本仓库 raw 链接后再运行。

## 目录结构

```
cpa-manager-plus-deploy/
├── compose.yaml                     # 双容器编排（端口/镜像走 .env）
├── .env.example                     # 环境变量模板（无真实值）
├── .gitignore                       # 屏蔽 secrets/ *.env 等
├── config/config.yaml.example       # 内核配置模板（无 api-key/密钥）
├── nginx/cpa.vhost.conf.example     # nginx 反代模板（脱敏域名 + 删 PAT）
├── update/                          # 一键更新服务（可选组件，无密钥）
│   ├── update.sh
│   ├── update_server.py
│   ├── cpa-update.service
│   └── cpa-update-inject.js
├── scripts/
│   ├── bootstrap.sh               # curl|bash 一键部署引导（下载仓库→自动安装）
│   ├── install.sh                 # 一键安装（--auto 自动生成密钥）
│   ├── backup.sh                  # 服务器全量备份（可选加密）
│   ├── restore.sh                 # 新机还原
│   └── setup-update-service.sh    # 安装一键更新服务（可选）
└── secrets/                         # gitignore；新机放真实密钥
```

---

## 一、新机安装步骤（核心流程）

前置：一台 **x86_64 Linux**（Debian/Ubuntu 均可），已装 Docker + Docker Compose。

### 1. 获取仓库（公开仓库，直接 clone）

```bash
git clone https://github.com/ckldy/cpa-manager-plus-deploy.git
cd cpa-manager-plus-deploy
```

> 私有使用场景可自行 fork 或设为 private；此处为公开模板仓库。

### 2. 配置环境变量

```bash
cp .env.example .env
vim .env   # 填 GITHUB_TOKEN（你的 GitHub fine-grained PAT）
```

### 3. 放好配置与密钥

```bash
# 方式A：从服务器现有部署还原（完整继承配置与密钥）
# 服务器上先跑 ./scripts/backup.sh 生成备份包，再在还原机上：
./scripts/restore.sh /path/to/cpa-full-xxx.tar.gz

# 方式B：全新安装
mkdir -p cliproxyapi/auths cliproxyapi/logs cliproxyapi/plugins secrets
cp config/config.yaml.example cliproxyapi/config.yaml
#   编辑 cliproxyapi/config.yaml 填 api-keys 与 remote-management.secret-key
printf '%s\n' '<随机串>' > secrets/cpamp-admin-key       # 面板管理员密钥
printf '%s\n' '<随机串>' > secrets/cpa-management-key    # 面板 API 管理密钥
chmod 600 secrets/cpamp-admin-key secrets/cpa-management-key
```

### 4. 一键安装

```bash
chmod +x scripts/*.sh
./scripts/install.sh
```

### 5. 验证

```bash
docker compose ps
curl -H "Authorization: Bearer <你的API_KEY>" http://127.0.0.1:8317/v1/models
# 浏览器打开 http://<服务器IP>:18317 进入面板
```

### 6.（可选）配 nginx 反代 + 一键更新服务

- 参考 `nginx/cpa.vhost.conf.example`：改域名、证书路径、GitHub PAT，放 BT 面板 vhost 目录。
- `./scripts/setup-update-service.sh` 装一键更新服务（systemd + /opt/cpa-update）。

---

## 二、备份与还原

### 服务器上全量备份（含真实密钥，仅本地留存）

```bash
cd cpa-manager-plus-deploy
./scripts/backup.sh
# 输出: /root/backups/cpa-full-<时间戳>.tar.gz
```

备份内容：`.env` / `compose.yaml` / `config.yaml` / `secrets/` / `plugins/` / `auths/`（登录态）/ 数据卷（usage.sqlite + data.key）。
> 备份含真实密钥，请只保留在服务器/私密存储，勿上传公开仓库。
> auths 里的第三方登录 token 有时效，新机还原后建议重新扫码登录。

### 新机还原

```bash
./scripts/restore.sh /root/backups/cpa-full-xxx.tar.gz
```

---

## 三、安全说明（必读）

1. **真实密钥绝不上传本仓库**：`secrets/`、`*.env`、`config/config.yaml` 都在 `.gitignore`，仓库里只有 `.example` 模板。
2. **GITHUB_TOKEN 是 GitHub PAT**：泄露=账号被操作。只在服务器 `.env` / nginx / 容器 env 里，建议用 fine-grained token 只授该用的仓库权限。
3. **第三方账号 token 有时效**：qoderwork / workbuddy 的登录态会过期。新机**到面板【模型/凭据】重新扫码登录**最稳。
4. **备份包含真实密钥**：明文包只保留在服务器/私密存储，勿上传公开仓库；必要时自行加密。

---

## 四、故障排查

| 现象 | 排查 |
|---|---|
| 面板打不开 | `docker compose ps` 看 cpa-manager-plus 是否 healthy；端口是否被占 |
| `/v1/models` 401 | `config.yaml` 的 `api-keys` 是否填对；容器内 `docker logs cpamp-cli-proxy-api-1` |
| 模型列表为空 | 是否在面板完成第三方账号扫码登录（auths 才有凭据） |
| 面板版本检查限流 | 确认 nginx `/github-latest` 已带你的 GITHUB_TOKEN |

---

*本仓库由实际运行系统整理为通用模板，结构与真实部署一致（compose 引用 `./cliproxyapi/` 目录），不含任何服务器真实信息。*
