<!--
[AI AGENT DIRECTIVE]
Before submitting this PR:
1. Ensure all new code adheres to TermCP core principles.
2. Must provide a verifiable "Proof of Work" (test logs, build output, or terminal session trace).
3. Do not submit speculative or hallucinated fixes.
-->

## 关联 Issue / Related Issue
Fixes #

## 改动动机与概述 / Summary
<!-- 简要说明改动内容及解决的核心问题 -->

## 改动分类 / Type of Change
- [ ] 🐛 Bug 修复 (Bug fix)
- [ ] ✨ 新特性 (New feature)
- [ ] ⚡ 性能优化 / 资源开销精简 (Performance / Footprint)
- [ ] 🛠️ 重构 / 架构清理 (Refactor)
- [ ] 📝 文档 / Agents.md 规范更新 (Documentation)

## Proof of Work (执行验证证据 - 必填)
<!--
PR 必须附带实际执行的验证证据，严禁无测试合入：
1. 本地构建与单元测试通过日志
2. 修复 bug 前后的实际运行输出对比
-->

```bash
# 1. 编译与测试命令
go test -v ./...
# 或 cargo test / npm test（按实际工具链）
```

<details>
<summary>点击展开验证日志 / Output Log</summary>

```text
PASTE VERIFICATION OUTPUT HERE
```

</details>

## 破坏性变更与安全自查 / Breaking Changes & Safety Checklist
- [ ] 是否涉及 Breaking Change（如涉及请详细说明迁移方式）
- [ ] 检查并确保无未授权的 Shell 注入与提权风险
- [ ] 检查 PTY / 进程退出生命周期，无进程/文件句柄泄漏
- [ ] 文档与 AGENTS.md 规则已同步更新
