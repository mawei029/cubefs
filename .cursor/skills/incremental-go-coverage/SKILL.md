---
name: incremental-go-coverage
description: 检查并改进 Go 增量测试覆盖率（针对当前未提交改动或自指定 commit 起的改动），基于 changed-line 覆盖率剖析。当用户要求“提升增量覆盖率/自动补齐改动代码测试/校验改动代码是否达到 80% 阈值”等场景时使用。
---

# 增量 Go 覆盖率（Incremental Go Coverage）

## 何时使用

当用户希望：

- 提升当前未提交 Go 改动的覆盖率
- 检查自某个 commit 起的增量覆盖率
- 自动补齐/扩展测试，直到改动代码覆盖率达到给定阈值
- 在提交或合并前达到目标阈值（如 80%）

## 工作流（Workflow）

1. 确定 diff 基线：
   - 当前未提交改动：不传 `--base`
   - 自某个 commit 至当前工作区：传 `--base <commit>`
2. 确定目标阈值：
   - 默认阈值 `80`
   - 允许用户自定义阈值（`0-100`）
   - `target_threshold` 视为**最终验收阈值**，**不要**作为每轮的硬门槛
   - 基于初始覆盖率 + 改动复杂度计算每轮里程碑目标 `round_target[i]`，仅最终验收时使用 `target_threshold`
3. 优先使用**最小变更包范围**生成 coverprofile，而不是默认整仓 `testcover`，除非范围确实不清晰。
4. 调用增量覆盖率检查脚本。
5. 若低于阈值，按未覆盖 changed lines 补齐 focused 测试，并在有界循环中重跑。
6. 自动补测最多 5 轮（`round1..round5`，不含初始基线）：
   - 每轮：补测 → 重跑定向覆盖率 → 重跑校验脚本
   - 一旦达到最终目标提前结束
   - 检测到收敛（如连续两轮低增益且未覆盖集合几乎不变）也提前结束
   - 第 5 轮仍未达标则停止并汇报阻塞与剩余未覆盖 changed lines
7. 每轮之间必须执行**自动策略决策层**（基于状态机，禁止凭手感）：
   - 收集本轮状态：当前覆盖率、轮间增益、`missing from coverprofile` 文件、按文件/函数聚合的未覆盖 changed lines、与上一轮未覆盖集合的重合率、go test 失败类别
   - 依据下面的状态机决策下一步
   - 若当前策略低增益，自动切换到下一档策略，禁止重复同一战术
   - 显式输出本轮目标和下一轮决策字段，便于追溯

### 两阶段执行策略（必须遵循）

覆盖率自动补测分为两个明确阶段：

1. **阶段 1：非侵入 / 仅测试侧（默认自动执行）**
   - 仅修改 `_test.go` 与测试夹具
   - 禁止改变生产路径行为
   - 优先手段：场景扩展、夹具复用、确定性调度、测试侧的 mock-like 桩/伪实现
   - 此阶段无需额外确认即可自动推进
2. **阶段 2：可测性注入 / 侵入式（必须用户同意）**
   - 仅当阶段 1 收敛且收益过小时，才允许在生产代码中引入 seam（接缝）
   - 例子：可注入的函数变量、适配器接口、面向时序/故障/顺序控制的 hook 点
   - 必须做到 **mock-like 且安全可控**：不改业务语义，仅提升测试侧的可控性/可观测性
   - **每次进入阶段 2 都必须先获得用户的明确同意**
   - 若用户不同意，阶段 1 收敛即停止，并汇报剩余未覆盖风险

### 轮内策略状态机（自动决策层）

- `STATE=ENV_BLOCKED`
  触发条件：go test/构建依赖失败（如缺头文件/工具链）。
  动作：立即停止重试；汇报阻塞与失败命令。
- `STATE=SCOPE_MISMATCH`
  触发条件：脚本提示有文件 `missing from coverprofile`。
  动作：先扩大 package/coverpkg 范围，包含所有改动 Go 包；先重跑覆盖率，再补测试。
- `STATE=TEST_GAP_FOCUSED`
  触发条件：无环境阻塞、无范围不匹配，未覆盖集中在少数文件/函数。
  动作：优先在已有 `_test.go` 内补 focused 测试，针对头部目标的分支/错误路径。
- `STATE=TEST_GAP_BROAD`
  触发条件：focused 一轮后未覆盖仍然分散。
  动作：在改动包内增加 1 个更宽的场景用例（integration-style）后重跑。
- `STATE=COMPLEX_CORE_FOCUSED`
  触发条件：低增益持续，未覆盖集中在复杂核心文件（并发/IO/状态机重头模块）。
  动作：停止铺面式补测，本轮只针对一个复杂目标文件/函数族打专用夹具。
- `STATE=CONVERGED_NO_GAIN`
  触发条件：连续两轮无明显增益且未覆盖集合几乎不变。
  动作：提前停止，汇报疑似不可达行/结构性阻塞。

### 策略切换触发器（必须自动套用）

- 满足**任一**条件即触发策略切换：
  - 轮间增益 `< 1.5` 个百分点
  - 与上一轮未覆盖集合重合率 `> 90%`
  - 新增测试主要命中低价值行，关键未覆盖文件原地不动
- 推荐切换链：
  - `TEST_GAP_FOCUSED -> TEST_GAP_BROAD -> COMPLEX_CORE_FOCUSED -> CONVERGED_NO_GAIN`
- 每次切换后只允许 **1 个观察轮**：
  - 若增益仍 `< 1.5pp`，再次切换或终结到 `CONVERGED_NO_GAIN`

### 收敛后的通用兜底选项

当进入 `CONVERGED_NO_GAIN` 但仍未达标时，必须从下面三个兜底选项里挑一个继续，禁止再次重复同一循环：

1. **分阶段验收**
   - 按包/领域/风险层级拆分为多个里程碑
   - 本阶段只用临时阶段目标，达成后再推进到下一阶段（**始终在当前分支直接推进**，不要切分支或拆 PR）
2. **单热点冲刺**
   - 暂停铺面式补测，整轮聚焦最高风险的单个未覆盖热点（单文件或单函数族）
   - 为该热点构建紧凑的场景矩阵（正常/错误/清理/重试/顺序）
3. **加权成功标准**
   - 保留 changed-line 覆盖率作为基础指标，叠加加权风险指标（高风险函数覆盖率 / 行为断言命中率）
   - 用风险下降效果评估进展，而不是仅看行覆盖率百分比

### 复杂路径补测的四层能力

当未覆盖集中在复杂核心逻辑时，**必须**联用以下四层，禁止逐行打补丁：

1. **复杂度标签化**
   - 按行为类型打标签：`concurrency` / `state-machine` / `error-injection` / `timing/order`
   - 用标签选择测试模板，而不是只看文件路径
2. **夹具优先**
   - 优先复用 fixture/hook（fake clock、fake queue/channel、fault injector、确定性调度封装）
   - 保持生产语义不变，必要时只引入最小可测性 seam
3. **行为覆盖目标**
   - 除了行覆盖率，还要跟踪行为断言：
     - 关键状态迁移
     - 并发下的关键不变量
     - 重试 / 回滚 / 幂等结果
   - 复杂目标每轮至少落地 1 条新的高价值行为断言
4. **风险优先 backlog**
   - 按 `risk = complexity * change_size * impact_surface` 给未覆盖目标排序
   - 每轮至少先打掉 1 个 Top 目标，再去碰低风险尾巴
   - 每轮输出 `TOPN_RISK_UNCOVERED`（默认 N=5）
   - 下一轮必须显式选中并覆盖前一轮 `TOPN_RISK_UNCOVERED` 中至少 1 个

### 仅测试模式：热点分包扫描策略（强制）

当用户明确要求“不改生产流程/代码”时，把以下流程当作正式策略（不是手工探索）：

1. **每轮只选 1 个热点文件**
   - 选 `TOPN_RISK_UNCOVERED` 第 1 名
   - 同一轮内禁止跨文件铺面，除非脚本汇报 `SCOPE_MISMATCH`
2. **按固定顺序跑场景包**
   - `PACK_A`：异步编排（requeue / stale / queue-full / ordering）
   - `PACK_B`：consumer 生命周期（owner handoff / done drain / nil request / fallback）
   - `PACK_C`：写入错误与恢复（closed status / push failure / recover-discard）
   - `PACK_D`：flush/cleanup 收敛（wait/drain/cleanup handoff invariants）
3. **分包执行规则**
   - 在已有 `_test.go` 中追加 2~6 个 focused 测试
   - 先跑定向 package 测试，再跑增量覆盖率脚本
   - 切换下一分包前，先计算本分包增益
4. **分包停止/切换规则**
   - 单分包增益 `>= 3.0pp`：继续打同一热点
   - 单分包增益 `< 1.5pp`：切换链上下一个分包
   - 连续两个分包都 `< 1.0pp`：判定 `CONVERGED_NO_GAIN`
5. **仅测试模式必须输出的字段**
   - `ROUND_HOTSPOT_FILE: <文件>`
   - `PACK_INDEX: <A|B|C|D>`
   - `PACK_GAIN_PP: <增益>`
   - `PACK_TEST_COMMANDS: <定向 go test 命令>`

### 分支与 PR 策略（默认禁止主动拆分）

为避免“为了刷覆盖率突然给用户冒出一堆分支/PR”，本 skill 默认**在当前分支直接推进**，不做任何分支/PR 切分动作：

1. **默认行为**
   - 全程在用户当前分支上补测、补 seam、跑覆盖率
   - 禁止 `git checkout -b`、`git worktree add`、`gh pr create` 等任何会创建新分支或新 PR 的操作
2. **何时可以拆分**
   - 仅当用户**显式要求**“拆 PR / 切分支 / 分批合并”时才执行
   - 此时由用户决定切分粒度，skill 不主动建议数量
3. **阶段 2 收益处理**
   - 阶段 2 拿到收益后**就地提交**，不要再开新分支堆叠
   - 是否合并 / 提 PR 由用户在当前分支基础上自行决定

### 阶段 2 安全清单（每次注入前必检）

进入阶段 2（已获用户同意）后，**所有项**都必须满足：

1. **语义安全**
   - 注入 seam 在生产路径下默认关闭
   - 不启用 seam 时不允许任何分支改变业务结果
2. **范围安全**
   - 改动局限在热点文件/函数族
   - 禁止大范围重构、签名级联变更
3. **可运维安全**
   - 每个 seam 必须配对：默认路径等价测试 + 注入路径行为测试
   - 在轮内输出中说明 seam 用途与回滚方式
4. **用户可见门控**
   - 编辑前输出 `PHASE2_CONSENT_REQUIRED: true`
   - 用户同意后再输出 `PHASE: 2` 与 `PHASE2_SAFE_GUARD: <用了哪种保险>`

### 高收益 seam 模式（来自实战）

阶段 2 推荐优先使用以下几种 mock-like seam 模式，已在本仓库 `sdk/data/stream/extent_handler.go` 等场景验证有效：

1. **时序 seam（time/sleep 替换）**
   - 把 `time.Sleep(retryInterval)` 改写为 `var fooSleep = time.Sleep`，生产路径默认调用真 `time.Sleep`
   - 测试侧把 `fooSleep` 替换为计数桩，立即结束等待
2. **空操作 hook seam（concurrency 协作点）**
   - 在并发关键节点插入 `var fooHook = func() {}` 作为默认空 hook
   - 测试侧替换为“在该点改写 owner/状态”的桩，确定性触发 inner-stale-owner、requeue 等罕见分支
3. **goroutine 静默退出 seam（worker 启动）**
   - 启动 worker（如 `startIOWorkers()`）后立刻 `handoffIOOwner()` 让 worker 看到 stale epoch 自然退出
   - 不需要新增 seam 也能覆盖 worker 启动入口，避免在测试里跑真 IO/真连接
4. **后台 reporter 全生命周期 seam（channel + closeC）**
   - 对“`for { select { closeC; ch } }`”形态的 reporter，直接构造最小 `Super`/`Worker`：只填 `closeC` 与 `metricCh`
   - 推入多种 metric 类型（含 `nil`、空结构、TimePoint、Counter）覆盖全部 case，再 `close(closeC)` 让 reporter 自然退出
   - 这种模式单测往往一次能拿走数十行覆盖
5. **非导出符号内部测试 seam**
   - `package foo_test` 触不到 unexported helper 时，新增同包内部测试 `package foo` 文件
   - 可直接覆盖 `retainMessage` / `putMessage` / `fileMode` 等内部辅助
6. **预置 recoveryHandler 桩**
   - 错误恢复路径（`recoverPacket` 等）默认会 `NewExtentHandler()` 启动新 worker，测试里大概率 panic
   - 通过预先把 `eh.recoverHandler` 设为只带 channel 的轻量桩，跳过新 worker 启动，专测 `pushToRequest` 成败两条分支

> 注意：上面的 hook/sleep seam 都属于 mock-like 接缝，必须保证默认值等于真实行为，且产线路径在不替换默认值时**字节级等价**。

### Seam 引入后的副作用提醒

- 引入 `var(...)` 声明会**改变源文件行号**，覆盖率脚本输出的“未覆盖行号区间”相对会整体下移
- 每轮重新读取未覆盖区间时，**不要把上轮的行号当真**，必须以最新 coverprofile 的行号为准
- seam 默认值（`= time.Sleep` / `= func(){}`）本身需要被产线代码至少调用一次才能算覆盖；只有测试替换变量、不实际调用产线路径时，对应那行仍会显示未覆盖

### 自动攻破迭代策略（最多 10 步）

热点突破阶段使用有界自动循环，禁止每步都问用户：

1. **步预算**
   - 单热点最多 **10 步微迭代**
   - 每步 = 补测/补 seam → 跑定向测试 → 重算增量覆盖率
2. **继续规则**
   - 当前步有正向增益：继续打同一热点
   - 当前步无增益：自动切换下一可攻破点
3. **停止规则**
   - 达到收敛、出现硬阻塞或步数到达 10 时停止
   - 汇报最佳收益与下一个推荐热点

### 轮目标设计（自适应里程碑）

- 由 changed-line 数、changed-file 数，以及未覆盖是否集中在高复杂度核心路径（并发/IO/状态机重模块）综合评估复杂度
- 构造单调递增的 `round_target[i]`，逐步逼近 `target_threshold`
- 推荐画像：
  - 低复杂度：阶梯少而陡
  - 中/高复杂度：阶梯多而平
- **禁止**用固定的 `target+20` 当作每轮硬门槛

## 覆盖率生成

测试日志与 coverprofile 文件**必须分离**。不要把 stdout/`tee` 重定向到 `coverage.txt`。

本仓库推荐的定向工作流：

```bash
base=<commit>
threshold=80
. build/cgo_env.sh
pkgs=$(
  {
    git diff --name-only "$base" -- '*.go'
    git ls-files --others --exclude-standard -- '*.go'
  } | while read -r f; do
        [ -n "$f" ] || continue
        d=$(dirname "$f")
        [ "$d" = "." ] && echo "./" || echo "./$d"
      done | sort -u | xargs -r go list | sort -u
)
coverpkg=$(printf '%s\n' "$pkgs" | paste -sd, -)
go test -covermode=count -coverprofile coverage.txt -coverpkg="$coverpkg" $pkgs
python3 .cursor/skills/incremental-go-coverage/scripts/check_incremental_go_coverage.py --repo . --coverprofile coverage.txt --base "$base" --threshold "$threshold"
```

当包范围太广或不确定时，可回退到整仓命令：

```bash
bash build/build.sh testcover > /tmp/testcover.log 2>&1
```

```bash
bash build/build.sh testcovercubefs > /tmp/testcover.log 2>&1
```

只有当 coverprofile 一定包含全部改动 Go 包时，才允许用定向 package 覆盖率。

## 命令

当前未提交 Go 改动：

```bash
python3 .cursor/skills/incremental-go-coverage/scripts/check_incremental_go_coverage.py --coverprofile coverage.txt --threshold <target_coverage>
```

自某 commit 起的改动（含已提交、staged、unstaged、untracked）：

```bash
python3 .cursor/skills/incremental-go-coverage/scripts/check_incremental_go_coverage.py --coverprofile coverage.txt --base <commit> --threshold <target_coverage>
```

## 脚本输出包含

- 总体增量覆盖率百分比
- 已覆盖 / 相关 changed lines
- 未覆盖 changed lines（按文件分组）
- 缺失于 coverprofile 的改动文件
- 因不映射到语句块而被忽略的 changed lines

## Agent 行为约束

- `_test.go` 改动**不计入**改动代码分母
- `missing from coverprofile` 视为必须最先修复的实际缺口（通常是包没被纳入覆盖率运行）
- 优先在已有 `_test.go` 追加用例，再考虑新建测试文件
- 改动包集合较小时优先走定向 package 覆盖率；范围太广或不确定时再用 `build/build.sh testcover` 或 `testcovercubefs`
- 禁止无界重试。基线之后最多 5 轮自动补测
- `target_threshold` 为最终验收阈值；用 `round_target` 作为中间里程碑
- 低增益时禁止死磕同一战术，必须按上面触发器自动切换策略档位
- 把策略切换视为一等决策，必须在 `NEXT_ROUND_STRATEGY` 中显式标注
- 复杂路径缺口必须先套用四层能力（复杂度标签 / 夹具优先 / 行为目标 / 风险优先）再考虑铺面低价值用例
- 用户要求“仅测试改动”时，把热点分包（`PACK_A..PACK_D`）作为一等策略，并显式输出分包增益和切换决策
- 阶段 1 是默认值；未在同一会话中获得用户明确同意时，禁止启动阶段 2 注入
- 阶段 2 优先用 mock-like seam（hook/接口/函数变量），避免结构级重写
- 热点突破阶段不要每步问用户；用 10 步自动循环并阶段性汇报
- **始终在用户当前分支上推进**：禁止主动 `git checkout -b` / `git worktree add` / `gh pr create`；只有用户显式要求“拆 PR / 切分支”时才执行
- 收敛但未达标时，先选用一个通用兜底选项（分阶段验收 / 单热点冲刺 / 加权成功标准），再决定是否永久停止
- 第 5 轮仍未达标必须显式归因（环境/构建阻塞、范围不匹配、真实测试缺口、收敛无收益）并列出剩余未覆盖 changed lines
- 描述“关键路径”时统一用文件级聚合措辞：
  - 推荐：“仍有未覆盖 changed lines 的关键路径”
  - 禁止：暗示整文件/路径零覆盖的措辞
- 仍需继续的轮次必须输出：
  - `NEXT_ROUND_REQUIRED: true`
  - `ROUND_INDEX: <n>`
  - `ROUND_TARGET: <round_target_n>`
  - `FINAL_TARGET: <target_threshold>`
  - `TOPN_RISK_UNCOVERED: <Top 5 高风险未覆盖函数清单>`
  - `NEXT_ROUND_FOCUS_TARGET: <下一轮至少覆盖前一轮 TopN 中的 1 个>`
  - `NEXT_ROUND_STRATEGY: <STATE>`
  - `NEXT_ROUND_COMMANDS: <下一轮命令清单>`
- 终止条件（`coverage >= target_threshold`、`round == 5`、`CONVERGED_NO_GAIN`、硬阻塞）必须输出：
  - `NEXT_ROUND_REQUIRED: false`
  - `STOP_REASON: <REACHED_TARGET|CONVERGED_NO_GAIN|MAX_ROUNDS_REACHED|ENV_BLOCKED|SCOPE_MISMATCH>`

## 复盘提示（每次执行后建议附带）

- **本次起始 vs 终止覆盖率**：例如 `25.75% -> 82.19%`
- **阶段 1 / 阶段 2 各贡献多少 pp**：便于评估“是否还需要再做阶段 2”
- **最高单轮收益的策略**：通常是某一轮命中“后台 reporter 全生命周期 seam”或“非导出符号内部测试 seam”，应当作模板沉淀复用
- **遗留风险**：哪些行属于真实不可达 vs 暂时未补的真测试缺口，明确分类，便于下一次入场
