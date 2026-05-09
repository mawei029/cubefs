---
description: incremental-coverage-autofix 的短别名（Go 增量覆盖率自动补测）
---

# /icover — 增量覆盖率自动补测（短别名）

`/icover` 是 `/incremental-coverage-autofix` 的短别名，参数与行为完全一致。

命令参数：

- `/icover [base_commit] [target_coverage]`
  - `base_commit` 可选
    - 传入时：检查 `<base_commit>..工作区(含未提交)` 的增量覆盖率
    - 不传时：默认只检查当前未提交改动（相对 `HEAD`）
  - `target_coverage` 可选，范围 `0-100`，默认 `80`
  - 参数解析规则：
    - 仅传 1 个参数且是数字：视为 `target_coverage`
    - 仅传 1 个参数且非数字：视为 `base_commit`
    - 传 2 个参数：依次为 `base_commit target_coverage`

示例：

```bash
/icover 6101cc536350bac8ba84c57e45b01dc9731da805
/icover 6101cc536350bac8ba84c57e45b01dc9731da805 90
/icover 85
/icover
```

## 执行要求

按 `/incremental-coverage-autofix` 的完整流程执行，**不要重复实现**。先读取并严格遵循：

- `.cursor/commands/incremental-coverage-autofix.md`（命令执行清单与脚本模板）
- `.cursor/skills/incremental-go-coverage/SKILL.md`（自动策略决策层、阶段 1/2、热点分包、自动攻破等）
- `.cursor/rules/incremental-go-coverage-workflow.mdc`（强制工作流规则）

**关键约束**（与主命令保持一致）：

- 默认在用户当前分支直接推进，禁止主动 `git checkout -b` / `git worktree add` / `gh pr create`
- 阶段 1 自动执行；阶段 2 必须先取得用户显式同意
- 最多 5 轮自动补测，达到 `target_coverage` 提前结束
- 收敛或达标后输出 `NEXT_ROUND_REQUIRED` / `STOP_REASON` 等标准字段
