# CLIProxyAPI 定制内核构建说明

本项目生产部署使用经过功能补丁强化的自建内核镜像（基于上游 [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)）。

当前最新已验证基线：**v8.0.44**（commit `1f917e77c4562623aa49e9804d999524151a2f96`）。

## 补丁内容概要（patchset5-v8.diff）

补丁集共 391 行，主要包含以下增强：
1. **插件调度与管理通道（`internal/pluginhost/`）**：
   - 暴露调度与路由追踪探针（`schedulerRecords` 指标）；
   - 优化插件管理路由分发与原始 JSON 响应支持。
2. **模型注册表（`internal/registry/`）**：
   - 优化模型注册缓存与动态模型发现兼容性；
   - 完善单元测试。

## 构建步骤

```bash
# 1. 克隆上游仓库并检出目标 tag
git clone https://github.com/router-for-me/CLIProxyAPI.git
cd CLIProxyAPI
git checkout v8.0.44

# 2. 应用补丁集
patch -p1 < /path/to/patchset5-v8.diff

# 3. 门禁验证（确保编译与测试全绿）
go build ./...
go vet ./...
go test ./internal/pluginhost/... ./internal/registry/...
go test ./...

# 4. 构建 Docker 镜像
TAG="v8.0.44-patched-$(date +%Y%m%d)"
COMMIT=$(git rev-parse HEAD)
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

docker build \
  --build-arg VERSION="$TAG" \
  --build-arg COMMIT="$COMMIT" \
  --build-arg BUILD_DATE="$BUILD_DATE" \
  -t "local/cli-proxy-api:$TAG" .

# 5. 更新 .env 中的 CPA_IMAGE 并启动
# CPA_IMAGE=local/cli-proxy-api:v8.0.44-patched-YYYYMMDD
docker compose up -d cli-proxy-api
```
