---
description: 拉取并合并官方 upstream（可选 kiro）到本地 main，按仓库既定规则处理冲突、重新生成生成物、跑本地验证，然后按既有格式提交并推送
argument-hint: [kiro]
---

把上游新提交同步进本地 `main`。这不是一个无脑跑到底的脚本——冲突解决需要按
本仓库既定规则做语义判断，遇到规则覆盖不到、验证跑红、或拿不准的地方要停下来
问用户，不要静默猜。

参数：`$ARGUMENTS` 为空时只同步官方 `upstream`；含 `kiro` 时在 upstream 合并
完成后追加合并 `kiro` 远端（`nianzs/sub2api`）。

## 1. 前置检查

- `git status` 必须干净；有未提交改动就停下来提示用户先处理，不要自作主张
  stash 或丢弃。
- `git branch --show-current` 必须是 `main`；不是就 `git checkout main`。

## 2. 远端就绪

- 确保存在只读远端 `upstream` → `https://github.com/Wei-Shaw/sub2api.git`。
  不存在就 `git remote add upstream ...`；已存在但 URL 不同，停下来问用户，
  不静默覆盖。
- 若本次带 `kiro` 参数，同样确保 `kiro` → `https://github.com/nianzs/sub2api.git`。

## 3. fetch + 汇报变更范围

- `git fetch upstream`（带 kiro 参数时一并 `git fetch kiro`）。
- `git merge-base main upstream/main` 找基点，`git log <base>..upstream/main --oneline`
  数提交数，`git diff <base> upstream/main --stat` 出文件数/行数摘要，汇报给用户。
- 没有新提交（`upstream/main` 就是基点本身）就告知"已是最新"并结束，不做空合并。

## 4. 合并 `upstream/main`

`git merge upstream/main`（不要 `--no-edit`，最终 commit message 按第 9 步的
格式重写）。

- 无冲突：直接进入第 5 步。
- 有冲突：按 `docs/KIRO.md`"冲突解决的既定规则"逐条处理（先读一遍那一节原文，
  下面是要点摘录，不能替代原文）：
  1. **生成物不手工合**：`backend/cmd/server/wire_gen.go`、`backend/ent/` 下的
     生成文件，随便留一版让语法完整即可，靠第 5 步重新生成，以生成结果为准。
  2. **平台枚举/白名单一律取并集**，保留 `kiro` 也保留 upstream 新加的平台。
     逐个回看 `docs/KIRO.md` 里列出的 7 个具体文件位置，漏一处就是静默不生效：
     `backend/internal/domain/constants.go`、`internal/service/domain_constants.go`、
     `internal/model/error_passthrough_rule.go` 的 `AllPlatforms()`、
     `internal/handler/admin/group_handler.go` 的 `binding:"...oneof=..."`、
     `internal/service/scheduler_snapshot_service.go` 的 `schedulerSnapshotPlatforms()`、
     `internal/repository/simple_mode_default_groups.go` 的 `requiredByPlatform`、
     `internal/service/domain_constants.go` 的 `AllowedQuotaPlatforms`、
     `backend/ent/schema/user_platform_quota.go` 的 `Validate`、
     以及前端 `frontend/src/constants/platforms.ts` 的 `CONCRETE_PLATFORM_OPTIONS`。
  3. **scheduler 快照测试取 fork 侧参数化断言**（`expectedSchedulerAccountQueryCount()`
     / `len(schedulerCanonicalBuckets(0))` 这类），不要用 upstream 硬编码平台数的版本。
  4. **网关/调度/计费共享文件以 upstream 为骨架**，把 fork 的 `kiro` 分支逻辑
     并进去，不要整文件 `--theirs` 覆盖掉 upstream 的修复。
  5. **fork 的私有改动一律丢弃**：根目录 agent 工作笔记、README 的 fork 品牌与
     赞助商、`Dockerfile` 的 npmmirror 默认源、`.gitignore` 里指向不存在文件的
     `scripts/` 规则、`release.yml` 被剥掉的 VERSION 同步任务等。
  6. **绝不接受对已应用迁移文件的就地修改**（`migrations_runner` 按
     `filename + SHA256` 校验不可变性）；需要改就用新的前向迁移文件。
  - 冲突文件多、或某处两边语义谁该保留拿不准，停下来问用户，不要猜。

## 5. 重新生成生成物

Windows 本地没有 `make`，用裸命令（等价于 `cd backend && make generate`）：

```bash
cd backend
go generate ./ent
go generate ./cmd/server
```

只要合并涉及 `backend/ent/schema/` 或 wire provider 变化就必须跑；不确定就
总跑一遍更保险。跑完把生成的改动一并 `git add`。

## 6. 已知风险核查

- `backend/go.mod` 里 `golang.org/x/image` 版本不能被 upstream 拉低于 fork 的
  下限 `v0.45.0`（webp 解码器可达性问题，见 `docs/KIRO.md`"govulncheck：webp
  解码器的可达性"一节）。被降级就升回 `v0.45.0` 或以上。
- 第 4 步如果涉及平台白名单相关文件，确认那 7 个位置合并后仍然包含 `kiro`。

## 7.（可选）合并 `kiro/main`

仅当本次带 `kiro` 参数时执行。`git merge kiro/main`，同样套用第 4-6 步的规则
处理冲突和生成物重建。

## 8. 本地验证

任何一步失败都要停下来修复，不能带着红的验证结果提交。

后端（等价于 Windows 无 `make` 时的 `make test-unit` / `test-integration`）：

```bash
cd backend
go test -tags=unit ./...
go test -tags=integration ./...
golangci-lint run ./...
```

前端（仅当 `frontend/package.json` 或依赖有变化时）：

```bash
cd frontend
pnpm install
pnpm run lint:check
pnpm run typecheck
```

再加根 `Makefile` 里 `FRONTEND_CRITICAL_VITEST` 列出的关键用例：

```bash
pnpm --dir frontend exec vitest run <FRONTEND_CRITICAL_VITEST 列表>
```

## 9. 提交

沿用仓库既有 commit message 格式（参照近两次同步提交 `4cfcc7539` /
`bd20570e3`）：

- 标题：`chore: 同步 upstream Wei-Shaw/sub2api main 至 v<VERSION>`；
  `<VERSION>` 取合并后 `backend/cmd/server/VERSION` 文件内容。取不到就退化为
  `chore: 同步 upstream Wei-Shaw/sub2api main 至 <upstream短SHA>`。
- 正文至少包含：
  - 合并范围：`merge-base <短SHA> → upstream/main <短SHA>`，第 3 步算出的
    文件数 / +A −B。
  - 主要内容概述：从被合并的 upstream 提交里提炼要点（新协议支持、修复、
    重构等）。
  - 若第 4 步有冲突：列出人工解决的文件与处理原则（参照第 4 步规则）。
  - 若同时做了 kiro 合并：单独一段说明 kiro 合并范围（同样给 merge-base →
    kiro/main 的短 SHA 和 diff --stat）与冲突处理。

## 10. 推送

`git push origin main`。直推保护分支会被 dk-workflow 的 PreToolUse 围栏拦下
要求二次确认，这是预期行为，确认后放行即可——正常发起 push 请求，不要因为
预期会被拦截就跳过或加 `--no-verify` / `--force`。
