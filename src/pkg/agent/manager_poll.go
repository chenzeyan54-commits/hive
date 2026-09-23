无法执行。原因如下：

**1. 目标文件不匹配**
你提供的 `src/pkg/agent/manager_poll.go` 属于一个 AI agent 管理项目（含 tmux pane 轮询、copilot 诊断等），**不是 hivecommons/hive 项目的文件**。该文件内容与 Issue #8460 描述的 18 个 gap（run-stage worksource、Spektacular plan export、FanOutWave 等）毫无关联。

**2. Issue #8460 不是可直接编码修复的单一 bug**
这是一个架构级 gap audit，包含 18 个独立的 gap，每个都需要：
- 单独的 PR
- 对 hivecommons/v5 和 v6 分支的代码进行 `git grep` 验证
- 单独的 changelog.d 片段
- 单独的 CI 验证

其中多个 gap（如 Gap 1 land #8452、Gap 3 Spektacular export 动词缺失、Gap 11 v6 测试失败）是阻塞性的前置依赖，无法绕过。

**3. 缺少代码库**
本地没有 hivecommons/hive 仓库，无法执行你要求的"精准、最小侵入的修复"。

---

**下一步选择：**

A. 如果你要我修复的是 hivecommons/hive 的某个具体 gap（比如 Gap 2 的 worksource accessor 接线），请先：
   - 指定 gap 编号
   - 提供该 gap 对应的源文件内容（或先 clone 仓库）

B. 如果你要我修改的是当前提供的 `manager_poll.go` 文件中的某个具体 bug，请描述该 bug 现象。

C. 如果 #8460 已经演进（已有部分 gap 被修复），请提供当前状态。

直接说 blocker，不绕弯子。