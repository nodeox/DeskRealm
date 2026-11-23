---
phase: 03-m2
plan: "06"
status: complete
completed: "2026-11-23"
source_commit: c37fde79c99c840cfc234b8e17e9fd15499665af
---

# 03-06 Windows 受管连接入口与本机确认

| 工作 | 提交 | 开发验证 |
| --- | --- | --- |
| 通讯录条目连接 | `cad98bb` | Windows 主控提交来源通讯录与条目；中心回归覆盖签发、核销和续租时重查授权。 |
| 无人值守发起与逐项确认 | `c2fd1c9` | 主控可显式申请无人值守。有人值守默认启用，端点可选择请求中允许的动作。 |
| 会合登记保活 | `c37fde7` | 在隔离回环直连及强制中继中验证重复登记、在线期限及会话准入。 |

确切候选的六项全量开发门禁及 Windows x64、ARM64 原生构建校验通过。每个 Windows 目标运行 12 项桌面、4 项租约和 1 项恢复复核测试。测试日志与产物摘要见 [候选复验](03-VALIDATION.md#03-06-同候选全量复验)。[Windows 构建](https://github.com/nodeox/DeskRealm/actions/runs/45233877838)

Phase 5 继续验证双 Windows 实际操作、重建后沿用公钥、真实连接路径及端点撤销和停控时序。
