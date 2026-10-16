# 主密码接口及 Web UI 接入契约

状态：开发分支已注册接口及 `storage.html` 表单，并接入迁移和启动状态门禁。尚未发布。
用户确认使用自定义密码，由 Argon2id 派生 KEK；不要求用户输入 32 字节 Base64 数据主密钥。

## 接口

所有操作通过已认证的 REST 请求执行，密码只放在 JSON body 中。
认证、同源/CSRF 防护、请求大小及频率限制属于接口接入条件；请求和错误日志不得记录 body 或密码。
远程访问使用 HTTPS，存储密码独立于 Web UI 登录凭据，不提供 MCP 密码工具。
实际 TLS 连接或 loopback 来源可以提交密码；远程反向代理只有在来源匹配 `TERMCP_STORAGE_TRUSTED_PROXIES` 中的 CIDR 且声明 `X-Forwarded-Proto: https` 时才被信任。默认不信任远程转发头，否则返回 426 `secure_transport_required`。
JSON 请求上限 16 KiB；初始化、解锁、改密合计每个服务进程每分钟最多 5 次，包括成功的尝试。未知字段和尾随 JSON 均拒绝。

| 方法及路由 | 请求 | 约束 |
| --- | --- | --- |
| `GET /api/storage/status` | 无 | 返回状态、是否已设置 password slot、迁移阶段和可公开的进度，不返回密钥封装、salt、密码或历史内容 |
| `POST /api/storage/initialize` | `new_password`、`confirm_password` | 仅真实未初始化且已取得存储排他锁时允许；后端验证两次相同并生成随机数据主密钥；不能覆盖已有封装文件 |
| `POST /api/storage/unlock` | `password` | 仅解锁已存在的 password slot；失败不能生成新主密钥；成功后先迁移/恢复，再进入 READY |
| `POST /api/storage/password` | `current_password`、`new_password`、`confirm_password` | 仅 READY/CLEANUP_PENDING 且存在 password slot；即使全局存储已解锁，也必须重新验证当前密码 |

接口不接受算法、KDF 成本或数据主密钥字段。初始和修改操作均使用服务端默认 KDF 参数与随机 salt。
ENV/系统托管模式不通过“初始化”接口覆盖既有存储；为已存在的纯托管存储添加 password slot 需要独立的托管凭据验证流程。
该添加流程目前不提供；需要用户自定义密码的部署应从默认 password 模式初始化。

修改接口调用 `KeyManager.ChangePasswordAndSave`，只有其持久化成功后返回成功。
确认不匹配返回可识别的 `password_confirmation_mismatch`；当前密码错误返回 `invalid_current_password`，不触发 Web UI 登录态退出。
未取得锁、初始化冲突、存储非 READY、尝试过频、普通保存失败和 `ErrDurability` 要区分，不能全部返回“密码错误”。
若文件已替换而目录同步失败，报告 `key_save_uncertain`：更改尚未得到持久化确认，需要保留旧、新密码用于恢复验证；不能显示“修改成功”。

## Web UI

首次设置表单包含「主密码」「确认主密码」两个 `type=password` 输入框。
解锁表单只输入主密码；设置和解锁成功后按状态显示迁移或恢复进度，不能提前显示正常工作区。
历史迁移提示遵守 [存储设计](encrypted-storage.md) 和 [Release 升级提示](../release-notes/encrypted-storage-migration.md)。

修改表单包含「当前主密码」「新主密码」「确认新主密码」三个输入框。
前端即时检查新密码确认，后端重复校验全部条件；提交期间禁用重复提交，成功后清空全部输入。
字段不回填真实密码，不进入 URL、localStorage、sessionStorage 或错误上报；自动填充语义使用 `current-password` / `new-password`。
持久化未确认时持续显示恢复提示，保留用户理解错误状态所需的说明，不把密码值存入提示内容。

文案纳入现有三语言词表。关闭表单、退出浏览器登录或浏览器断开均不主动锁定全局加密存储，也不提供运行期锁定按钮。

## 接入验收

除了底层测试，启用路由时必须验证：已初始化后不能再次初始化；HTTP 认证通过仍需输入当前主密码；前端绕过确认校验也会被拒绝；保存失败和并发修改不能假成功；LOCKED/MIGRATING/RECOVERING 不能启动业务；日志和浏览器持久化无密码；迁移提示覆盖首次升级、续迁、失败和明文清理未完成。
