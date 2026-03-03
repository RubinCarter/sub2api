# Fork 维护指南

本仓库是从 [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) fork 而来的定制版本。

## 仓库远程配置

```
origin    → https://github.com/RubinCarter/sub2api.git  (fetch/push)
upstream  → https://github.com/Wei-Shaw/sub2api.git     (fetch only, push 已禁用)
```

upstream 的 push 已设置为 `no_push`，防止误推到上游仓库。

### 如果需要重新配置

```bash
# 添加 upstream（仅首次）
git remote add upstream https://github.com/Wei-Shaw/sub2api.git

# 禁止向 upstream 推送
git remote set-url --push upstream no_push

# 验证配置
git remote -v
```

## 同步上游更新

### 同步代码（main 分支）

GitHub 页面的 "Sync fork" 按钮可以同步 main 分支的代码。

### 同步 Tags

**GitHub "Sync fork" 不会同步 tags**，需要手动操作：

```bash
git fetch upstream --tags
git push origin --tags
```

## 发布自定义版本

当上游发布新版本（如 `v0.1.75`），想要在其基础上叠加自定义修改并发布：

### 步骤

```bash
# 1. 获取上游最新 tags
git fetch upstream --tags

# 2. 基于上游 tag 创建发布分支
git checkout -b release/v0.1.75.1 v0.1.75

# 3. Cherry-pick 自定义 commit（从 main 分支挑选）
git cherry-pick <commit-hash>
# 如有冲突，解决后：
#   git add <冲突文件>
#   git cherry-pick --continue

# 4. 验证编译
cd backend && go build ./... && cd ..

# 5. 创建 tag
git tag -a v0.1.75.1 -m "v0.1.75.1: custom release based on upstream v0.1.75"

# 6. 推送分支和 tag
git push origin release/v0.1.75.1
git push origin v0.1.75.1

# 7. 切回 main
git checkout main
```

### 版本命名规则

| 场景 | 命名 | 示例 |
|------|------|------|
| 基于上游 vX.Y.Z 的第一个定制版 | vX.Y.Z.1 | v0.1.74.1 |
| 同一上游版本的后续定制 | vX.Y.Z.2 | v0.1.74.2 |

### 注意事项

- Tag 推送后会自动触发 `release.yml` 构建（匹配 `v*` 模式）
- **Fork 仓库首次使用时**，tag push 可能不会自动触发 workflow，需要手动触发：
  ```bash
  gh workflow run release.yml --repo RubinCarter/sub2api -f tag=v0.1.75.1
  ```
- 查看构建状态：
  ```bash
  gh run list --repo RubinCarter/sub2api --limit 5
  ```

## CI/CD 说明

| Workflow | 触发条件 | 作用 |
|----------|----------|------|
| `backend-ci.yml` | 每次 push / PR | 仅运行测试和 lint |
| `release.yml` | push `v*` tag 或手动触发 | 完整构建 + 发布（二进制、Docker、Telegram 通知） |

### 需要配置的 GitHub Secrets

| Secret | 用途 | 是否必须 |
|--------|------|----------|
| `DOCKERHUB_USERNAME` | DockerHub 用户名 | 推送 DockerHub 镜像时需要 |
| `DOCKERHUB_TOKEN` | DockerHub Access Token | 推送 DockerHub 镜像时需要 |
| `TELEGRAM_BOT_TOKEN` | Telegram 机器人 Token | 发送通知时需要（可选） |
| `TELEGRAM_CHAT_ID` | Telegram 聊天 ID | 发送通知时需要（可选） |

GHCR（GitHub Container Registry）使用自动提供的 `GITHUB_TOKEN`，无需额外配置。

DockerHub Token 获取路径：[DockerHub](https://hub.docker.com) → 头像 → Account Settings → Security → New Access Token

## 自定义功能清单

| 功能 | 说明 | 涉及 commit |
|------|------|-------------|
| show_github_button | 系统设置中控制是否显示 GitHub 按钮 | `187c7604` |
| github_repo | 系统设置中配置更新源仓库地址 | `187c7604` |
