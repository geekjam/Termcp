# 主密码接口及 Web UI 接入契约

状态：开发分支已注册接口及 `storage.html` 表单，并接入迁移和启动状态门禁。尚未发布。
默认系统托管并自动解锁；用户可选择设置自定义恢复主密码，由 Argon2id 派生 KEK，不要求用户输入 32 字节 Base64 数据主密钥。

## 接口

所有操作通过已认证的 REST 请求执行，密码只放在 JSON body 中。
认证、同源/CSRF 防护、请求大小及频率限制属于接口接入条件；请求和错误日志不得记录 body 或密码。
远程访问使用 HTTPS，存储密码独立于 Web UI 登录凭据，不提供 MCP 密码工具。
实际 TLS 连接或 loopback 来源可以提交密码；远程反向代理只有在来源匹配 `TERMCP_STORAGE_TRUSTED_PROXIES` 中的 CIDR 且声明 `X-Forwarded-Proto: https` 时才被信任。默认不信任远程转发头，否则返回 426 `secure_transport_required`。
JSON 请求上限 16 KiB；初始化、解锁、改密及解锁方式修改合计每个服务进程每分钟最多 5 次，包括成功的尝试。未知字段和尾随 JSON 均拒绝。

| 方法及路由 | 请求 | 约束 |
| --- | --- | --- |
| `GET /api/storage/status` | 无 | 返回状态、是否已设置 password slot、`unlock_mode`、`unlock_mode_saved`、`auto_unlock`、可选 `auto_unlock_error`、迁移阶段和可公开的进度，不返回密钥封装、salt、密码或历史内容 |
| `POST /api/storage/initialize` | `new_password`、`confirm_password` | 真实未初始化时生成随机数据主密钥；或为已由系统/ENV 解锁且 READY/CLEANUP_PENDING 的无密码存储追加第一个密码 slot。均须持有排他锁、确认两次密码一致；不能覆盖已有密码或数据主密钥 |
| `POST /api/storage/unlock` | `password` | 仅解锁已存在的 password slot；失败不能生成新主密钥；选择自动方式时，成功后尝试建立所配置的自动解锁托管；手动方式保持不托管；随后迁移/恢复并进入 READY |
| `POST /api/storage/password` | `current_password`、`new_password`、`confirm_password` | 仅 READY/CLEANUP_PENDING 且存在 password slot；即使全局存储已解锁，也必须重新验证当前密码 |
| `POST /api/storage/unlock-mode` | `unlock_mode`；已有密码时 `current_password`；首次选择手动时 `new_password`、`confirm_password` | 仅 READY/CLEANUP_PENDING；方式为 `automatic` 或 `password`。验证当前密码或首次密码确认；仅持久化成功返回 200，原子替换策略及对应封装 |

接口不接受算法、KDF 成本或数据主密钥字段。初始和修改操作均使用服务端默认 KDF 参数与随机 salt。
未保存用户选择时，默认启动通过系统托管创建或解锁存储；非空 ENV KEK 优先。已保存的 `password` 方式优先于部署默认值，跳过系统/ENV 自动解锁且成功手动解锁后不自动托管；`automatic` 方式使用已配置的系统/ENV provider。没有可用自动解锁服务时保持 UNINITIALIZED/LOCKED，不能回退为明文存储。
为托管存储设置首个密码，要求本进程已通过托管凭据解锁并完成恢复；接口认证、同源及传输校验照常执行。该操作保留自动解锁 slot 和数据主密钥，并原子保存封装。已有密码不能通过初始化接口覆盖。
选择自动方式的已有密码存储成功手动解锁后自动建立系统/ENV 托管，以后无需每次输入密码；托管失败可继续使用密码恢复，并公开 `auto_unlock_error=automatic_unlock_unavailable`。`auto_unlock=true` 表示本次已成功使用或持久化可用托管 slot，不承诺其他机器或账户可解锁。

修改接口调用 `KeyManager.ChangePasswordAndSave`，只有其持久化成功后返回成功。
确认不匹配返回可识别的 `password_confirmation_mismatch`；当前密码错误返回 `invalid_current_password`，不触发 Web UI 登录态退出。
未取得锁、初始化冲突、存储非 READY、尝试过频、普通保存失败和 `ErrDurability` 要区分，不能全部返回“密码错误”。
若文件已替换而目录同步失败，报告 `key_save_uncertain`：更改尚未得到持久化确认，需要保留旧、新密码用于恢复验证；不能显示“修改成功”。

## Web UI

页面提供「自动解锁（默认）/ 每次重启输入主密码」选择框，显示当前选择及其影响，恢复未完成前禁用。轮询不覆盖尚未保存的选择。
保存时，已有密码显示当前密码框；没有密码而首次选择手动方式时显示两次新密码框。保存期间禁用重复提交，成功清空输入，不在浏览器持久化模式或密码；服务端状态为权威。
手动方式删除全部非密码 slot，防止 ENV 或系统服务绕过选择；策略随版本 2 密钥封装参与 AAD 认证，和封装单文件原子发布。修改密码保留选择，不重加密历史。
自动托管不可用返回 503 `automatic_unlock_unavailable`，不修改原选择；非法方式返回 400 `invalid_unlock_mode`。无额外密码字段的请求不会偷偷替换密码。

默认系统托管启动不显示密码门禁。用户打开存储设置页时，可选的首次设置表单包含「主密码」「确认主密码」两个 `type=password` 输入框；托管不可用时同一表单可初始化手动密码模式。
解锁表单只输入主密码；设置和解锁成功后按状态显示迁移或恢复进度，不能提前显示正常工作区。
历史迁移提示遵守 [存储设计](encrypted-storage.md) 和 [Release 升级提示](../release-notes/encrypted-storage-migration.md)。

修改表单包含「当前主密码」「新主密码」「确认新主密码」三个输入框。
前端即时检查新密码确认，后端重复校验全部条件；提交期间禁用重复提交，成功后清空全部输入。
字段不回填真实密码，不进入 URL、localStorage、sessionStorage 或错误上报；自动填充语义使用 `current-password` / `new-password`。
持久化未确认时持续显示恢复提示，保留用户理解错误状态所需的说明，不把密码值存入提示内容。

文案纳入现有三语言词表。关闭表单、退出浏览器登录或浏览器断开均不主动锁定全局加密存储，也不提供运行期锁定按钮。

## 接入验收

除了底层测试，启用路由时必须验证：已有密码不能再次初始化；纯托管存储可以设置第一个密码且保留自动解锁；默认首次启动和后续重启无需密码；手动选择在 ENV/系统托管可用时仍在每次重启要求密码，解锁不恢复自动托管；可安全切回自动方式；HTTP 认证通过仍需输入当前主密码；前端绕过确认校验也会被拒绝；保存失败和并发修改不能假成功；LOCKED/MIGRATING/RECOVERING 不能启动业务；日志和浏览器持久化无密码；迁移提示覆盖首次升级、续迁、失败和明文清理未完成。
