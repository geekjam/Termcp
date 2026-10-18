---
name: "Bug Report (AI-Native)"
about: "报告 TermCP 的缺陷或异常行为（供人类与 Agent 快速定位与复现）"
title: "[BUG]: <简明描述问题>"
labels: ["bug", "triage"]
---

<!--
[AGENT INSTRUCTION]
If you are an AI Coding Agent filing this issue:
- Do NOT hallucinate reproduction steps or environment logs.
- Provide minimal reproducible command lines and MCP call traces.
- Keep the description self-contained.
-->

### 1. 运行环境 / Environment
- **TermCP Version / Commit**: (如 v0.4.2 或 git commit sha)
- **OS & Architecture**: (如 Linux x86_64 / macOS arm64 / Docker container)
- **Host / Terminal / Shell**: (如 bash 5.2 / zsh / tmux / headless)
- **MCP Client / Gateway (如有)**: (如 Claude Desktop / Cherry Studio / 自研网关)

### 2. 交互模式 / Execution Context
- [ ] Direct CLI
- [ ] MCP Server Mode (stdio)
- [ ] MCP Server Mode (SSE / Stream)
- [ ] Web / TUI Session

### 3. 复现步骤与调用链 / Minimal Reproduction
<!-- 提供能够精准复现的命令序列、MCP Tool Call 参数或输入数据 -->
```bash
# 复现命令示例
termcp session create --target ...
```

```json
// MCP tool_call 载荷（如有）
{
  "name": "termcp_exec",
  "arguments": {}
}
```

### 4. 实际行为与错误日志 / Actual Behavior & Raw Logs
<!-- 请附带完整的原始错误堆栈、退出码或 MCP 错误返回，严禁截断核心 trace -->
```text
<此处粘贴错误日志、stderr 或 panic 堆栈>
```

### 5. 预期行为 / Expected Behavior
<!-- 期望的正常输出、会话状态或退出行为 -->

### 6. 验证标准 / Verification Criteria
<!-- 用于 Agent 修复后自动化回归验证的判定条件 -->
- [ ] 执行特定命令不再报错并返回 code 0
- [ ] MCP session 正常回收，不残留孤儿进程
