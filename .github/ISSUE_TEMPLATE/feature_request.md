---
name: "Feature Request (AI-Native)"
about: "提出架构改进、新协议支持或功能建议"
title: "[FEAT]: <特性简述>"
labels: ["enhancement"]
---

### 1. 背景与痛点 / Context & Problem
<!-- 描述在什么开发/攻击/运维场景下遇到了限制？现有方案为何不满足？ -->

### 2. 方案设想 / Proposed Solution
<!-- 接口设计、命令行参数变更或 MCP Schema 扩展 -->

### 3. 架构不变式与兼容性 / Invariants & Compatibility
<!-- 是否影响现有 Session 状态持久化？是否破坏终端转义处理？ -->
- [ ] 保持零外部强依赖或轻量原则
- [ ] 保持对现有 MCP 协议契约向后兼容
- [ ] 不破坏终端 PTY / Raw 模式控制

### 4. 验收基准 / Acceptance Criteria
<!-- Agent 编写测试时所需覆盖的基准 -->
1. 给定输入 X，期望输出 Y
2. 新增单元测试与 E2E 验证脚本
