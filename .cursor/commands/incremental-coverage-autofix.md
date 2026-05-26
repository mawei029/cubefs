---
description: 自动补齐测试用例并校验 Go 增量覆盖率（别名 /icover）
---

# 增量覆盖率自动补测

用于“只针对增量改动补测试并校验覆盖率”的场景。

> 短别名：`/icover` 与 `/incremental-coverage-autofix` 完全等价，参数和行为一致。

命令参数：

- `/incremental-coverage-autofix [base_commit] [target_coverage]`
- `/icover [base_commit] [target_coverage]`（短别名，等价）
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
/incremental-coverage-autofix 6101cc536350bac8ba84c57e45b01dc9731da805
/incremental-coverage-autofix 6101cc536350bac8ba84c57e45b01dc9731da805 90
/incremental-coverage-autofix 85
/incremental-coverage-autofix

# 等价短别名：
/icover 6101cc536350bac8ba84c57e45b01dc9731da805
/icover 90
/icover
```

执行要求：

1. 先校验并确定 diff 基线：
   - 如果用户给了 commit，就用它作为 `--base`
   - 且必须先执行：`git rev-parse --verify <base_commit>`
   - 如果 commit 不存在：立即报错并停止，不要继续跑测试/覆盖率
   - 报错时同时输出正确用法：
     - `incremental-coverage-autofix <valid_commit> [target_coverage]`
     - `incremental-coverage-autofix [target_coverage]`
   - 否则默认检查当前未提交代码
2. 校验阈值参数：
   - `target_coverage` 未传入时默认 `80`
   - 必须是 `0-100` 的数字；非法时立即报错并停止
   - 达标判断以 `target_coverage` 为准（默认 `80`）
   - 不再将固定 `target+20` 作为最终硬门槛
3. 优先按**最小变更包范围**生成 `coverage.txt`，不要默认整仓 `bash build/build.sh testcover`
   - 只关注以下业务目录，其他 Go 改动忽略：`client`、`client/common`、`client/fdstore`、`client/fs`、`client/gosdk`、`client/libsdk`、`datanode`、`datanode/repl`、`datanode/storage`、`lcnode`、`master`、`metanode`、`objectnode`、`remotecache/flashgroupmanager`、`remotecache/flashnode`、`remotecache/flashnode/cachengine`、`sdk/data/blobstore`、`sdk/data/manager`、`sdk/data/stream`、`sdk/data/wrapper`、`sdk/httpclient`、`sdk/master`、`sdk/meta`、`sdk/remotecache`
4. 用现有脚本检查增量覆盖率：
   - `python3 .cursor/skills/incremental-go-coverage/scripts/check_incremental_go_coverage.py ...`
   - 除整体增量覆盖率外，每个有可执行增量行的关注目录也必须达到 `target_coverage`，否则继续补测该目录
5. 如果低于目标阈值：
   - 根据未覆盖 changed lines 补测试
   - 优先追加到已有 `_test.go`，不要无意义拆很多新测试文件
   - 优先覆盖关键逻辑、关键分支、配置透传和并发/状态变化路径
   - 执行分阶段策略：
     - 阶段1（自动）：只做非侵入测试补齐（测试用例、测试夹具、mock/fake）
     - 阶段2（可选）：注入/侵入式可测试性改造（mock-like seam），必须先征得用户同意
6. 每补一轮测试都重新执行：
   - 定向 `go test -coverprofile`
   - 增量覆盖率脚本
   - 自动补测采用自适应轮次：最少观察 3 轮，最多 5 轮（不含初始基线检测）
   - 任意一轮达到阈值立即停止
   - 达到收敛条件（低增益/高重合）可提前停止；第 5 轮后仍未达标必须停止并输出原因分析
   - 每轮必须按“自动策略决策层”选择下一步，不允许随意跳过
   - 若用户要求“只改测试，不改生产流程”，必须启用“热点分包补测”：
     - 每轮只选 1 个热点文件（优先 `TOPN_RISK_UNCOVERED` 第一项）
     - 分包顺序：`PACK_A` 异步编排 -> `PACK_B` consumer 生命周期 -> `PACK_C` 写入错误恢复 -> `PACK_D` flush/cleanup 收敛
     - 每个分包都执行：定向 `go test` -> 覆盖率脚本校验 -> 记录分包增益
     - 分包增益规则：`>= 3.0pp` 可继续同热点；`< 1.5pp` 切下个分包；连续两个 `< 1.0pp` 判定收敛
   - 攻破执行支持自动 10 步推进（无需每步询问用户）：
     - 单热点最多 10 步微迭代：补测/验证/覆盖率复算
     - 当前步有收益则继续进攻；无收益自动切换下一可攻破点
     - 达到 10 步或收敛条件即停止并汇报
   - 阶段1若收敛但未达标：必须先提示用户是否进入阶段2，禁止直接改生产代码
   - 默认在用户当前分支直接推进；禁止主动 `git checkout -b` / `git worktree add` / `gh pr create`，仅在用户显式要求时才拆 PR 或切分支
7. 结束时明确给出：
   - 当前增量覆盖率
   - 用户阈值 `target_coverage`（最终达标阈值）
   - 运行过的测试命令
   - 还没覆盖到的关键文件/关键路径
   - 若未达标：给出失败原因分类（环境阻塞/覆盖范围不匹配/测试用例缺口）
   - 若启用了热点分包补测：额外给出 `ROUND_HOTSPOT_FILE`、`PACK_INDEX`、`PACK_GAIN_PP`
   - 若准备进入阶段2：必须输出 `PHASE2_CONSENT_REQUIRED: true`、`PHASE2_PLAN`、`PHASE2_SAFETY_NOTE`
   - 若阶段2已执行：必须输出 `PHASE: 2`、`PHASE2_SAFE_GUARD`

自动策略决策层（每轮必走）：

1. 收集状态：
   - 当前 coverage、target_coverage
   - `missing from coverprofile` 文件列表
   - 未覆盖 changed lines（按文件/函数聚合）
   - go test 是否有构建/依赖阻塞
2. 状态到策略映射：
   - `ENV_BLOCKED`：构建或依赖阻塞 -> 立即停止并报告阻塞命令/错误
   - `SCOPE_MISMATCH`：存在 missing-from-coverprofile -> 先修正 `pkgs/coverpkg` 覆盖范围后再跑
   - `TEST_GAP_FOCUSED`：缺口集中 -> 优先在已有 `_test.go` 补 focused 分支/错误路径
   - `TEST_GAP_BROAD`：缺口分散 -> 增加一个更宽场景用例（同包）
   - `COMPLEX_CORE_FOCUSED`：复杂核心文件缺口集中 -> 按热点分包策略执行 `PACK_A..PACK_D`
   - `CONVERGED_NO_GAIN`：连续两轮覆盖率增量很小且未覆盖集合几乎不变 -> 提前停止并报告原因
3. 固定输出字段（强制）：
   - 若 `coverage < target_coverage` 且 `round < max_rounds`：
     - `NEXT_ROUND_REQUIRED: true`
     - `NEXT_ROUND_STRATEGY: <STATE>`
     - `NEXT_ROUND_COMMANDS: <下一轮命令>`
   - 否则：
     - `NEXT_ROUND_REQUIRED: false`

推荐命令模板：

```bash
arg1="${1:-}"
arg2="${2:-}"
base=""
threshold="80"

if [ -n "$arg1" ]; then
  if [[ "$arg1" =~ ^([0-9]|[1-9][0-9]|100)(\.[0-9]+)?$ ]]; then
    threshold="$arg1"
  else
    base="$arg1"
    [ -n "$arg2" ] && threshold="$arg2"
  fi
fi

. build/cgo_env.sh

if ! [[ "$threshold" =~ ^([0-9]|[1-9][0-9]|100)(\.[0-9]+)?$ ]]; then
  echo "ERROR: invalid target coverage: $threshold"
  echo "Usage:"
  echo "  incremental-coverage-autofix <valid_commit> [target_coverage]"
  echo "  incremental-coverage-autofix [target_coverage]"
  exit 2
fi

filter_focus_go_files() {
  while read -r f; do
    [ -n "$f" ] || continue
    case "$(dirname "$f")" in
      client|client/common|client/fdstore|client/fs|client/gosdk|client/libsdk|datanode|datanode/repl|datanode/storage|lcnode|master|metanode|objectnode|remotecache/flashgroupmanager|remotecache/flashnode|remotecache/flashnode/cachengine|sdk/data/blobstore|sdk/data/manager|sdk/data/stream|sdk/data/wrapper|sdk/httpclient|sdk/master|sdk/meta|sdk/remotecache)
        printf '%s\n' "$f"
        ;;
    esac
  done
}

if [ -n "$base" ]; then
  if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
    echo "ERROR: invalid base commit: $base"
    echo "Usage:"
    echo "  incremental-coverage-autofix <valid_commit> [target_coverage]"
    echo "  incremental-coverage-autofix [target_coverage]"
    exit 2
  fi
  changed_go_files=$(git diff --name-only "$base" -- '*.go' | filter_focus_go_files)
else
  changed_go_files=$(
    {
      git diff --name-only HEAD -- '*.go'
      git ls-files --others --exclude-standard -- '*.go'
    } | filter_focus_go_files
  )
fi

pkgs=$(printf '%s\n' "$changed_go_files" | while read -r f; do
        [ -n "$f" ] || continue
        d=$(dirname "$f")
        [ "$d" = "." ] && echo "./" || echo "./$d"
      done | sort -u | xargs -r go list | sort -u
)
coverpkg=$(printf '%s\n' "$pkgs" | paste -sd, -)
go test -covermode=count -coverprofile coverage.txt -coverpkg="$coverpkg" $pkgs
check_cmd=(python3 .cursor/skills/incremental-go-coverage/scripts/check_incremental_go_coverage.py --repo . --coverprofile coverage.txt --threshold "$threshold")
[ -n "$base" ] && check_cmd+=(--base "$base")
"${check_cmd[@]}"
```
