# 加密文件基础库与事件日志格式

状态：开发分支已实现并接入服务存储，首次解锁后会执行受控旧文件迁移；尚未发布。部署和升级见 [`../encrypted-storage.md`](../encrypted-storage.md)。
实现位于 `internal/encryptedfs` 和 `internal/sessionlog`；整体接入与迁移协议见 [存储设计](encrypted-storage.md)。
所有多字节二进制数均采用小端序。下述格式不依赖数据库、FUSE 或 Afero。

## 密钥封装

`KeyManager` 随机生成 32 字节数据主密钥和 16 字节 Store ID。
`master.key` 是有大小限制的 JSON 信封，**只包含封装后的主密钥**；JSON 中的字节数组使用标准 Base64。
兼容版本 1，包含 `version`、`store_id`、`slots`；用户保存解锁方式后使用版本 2，增加 `unlock_mode`（`automatic` / `password`）。最多 8 个 slot，文件最大 64 KiB。版本 2 的 `password` 方式仅允许密码 slot。此版本升级仅影响密钥信封，数据文件、事件和 HKDF 仍为版本 1。

每个 slot 包含随机 16 字节 ID、provider 名称、provider 参数、随机 24 字节 nonce 和 48 字节主密钥密文及认证标签。
封装使用 XChaCha20-Poly1305。AAD 是固定 Go struct 的规范 JSON 编码，字段顺序和名称依次为
`Version`、`StoreID`、`SlotID`、`Provider`、`Parameters`；版本 2 在末尾增加 `UnlockMode`，版本 1 完全省略该字段以保持原有 AAD 字节兼容。不是原始信封的 JSON 字节，也不含其他 slot。
因此修改 Store ID、slot ID、provider 参数、nonce、密文或版本 2 的解锁方式都会导致该 slot 解锁失败。不能通过编辑 JSON 或降级信封版本来绕过密码。
删除某个完整 slot 属于可用性损失，不承诺借此检测整套文件回滚。

已实现的 provider：

| Provider | KEK 来源 | 持久化参数 |
| --- | --- | --- |
| `env` | `TERMCP_STORAGE_KEK` 或明确指定的环境变量；严格 Base64 解码后恰好 32 字节 | 空参数；不保存环境变量内容 |
| `password` | 用户主密码经 Argon2id 派生 | 随机 16 字节 salt，以及 `time`、`memory_kib`、`threads` |
| `dpapi` | 当前 Windows 账户 DPAPI 保护的随机 KEK | `protected` 保存 DPAPI 密文 |
| `keychain` | macOS 登录 Keychain 中的随机 KEK | `salt` 保存随机 slot 引用，不含本机登录名 |
| `secret-service` | Linux Secret Service 中的随机 KEK | `salt` 保存随机 slot 引用，不含本机登录名 |

生产默认 Argon2id 参数为 3 次迭代、64 MiB 内存、1 个线程。
读取磁盘参数时，先检查迭代 1–10、内存 8–256 MiB、线程 1–8 的边界，防止未经检查的参数触发无界工作。
测试显式使用 8 MiB / 1 次迭代，REST 接口不应接收调用方提供的 KDF 参数。
参数边界不能代替接口的串行化、请求大小限制及尝试频率限制。
macOS 通过 `/usr/bin/security -i` 的 stdin 写入秘密；Linux 使用 libsecret 的 `secret-tool` stdin。秘密不进入进程参数、额外环境变量或临时文件；工具错误不带入诊断日志。系统凭据创建后信封保存失败，可能留下未被引用的系统凭据；不会将新 KEK 写到数据目录作为回退。

`Derive(purpose, identity)` 使用 HKDF-SHA256：salt 为 Store ID；info 是
`termcp/encryptedfs/v1` 后接两个「uint32 字节长度 + UTF-8 原始字节」字段。
用途及逻辑身份均不可为空，最长各 4096 字节；调用方使用稳定的逻辑 ID，不能使用机器相关的绝对目录名。
扩展模板使用 `purpose=config`、`identity=extensions/<name>` 派生用途密钥，文件认证身份也为 `extensions/<name>`；不同模板之间不能直接替换密文。
配置、事件和索引使用不同用途；每个文件还以随机 File ID 为 salt 再派生文件密钥。

`NewPasswordKeyManager` 检查首次密码确认；`ChangePasswordAndSave` 串行执行当前密码验证、两次新密码核对、重封装和原子保存。
即使主密钥已在内存中，也不能跳过当前密码验证。重封装保留数据主密钥及非 password slot。
保存失败且尚未替换文件时，内存和磁盘均保留原 slot；替换完成但目录同步失败时，内存跟随已发布文件，返回 `ErrDurability`，不能确认持久化成功。
`SetUnlockModeAndSave` 重新验证已有主密码；首次进入手动方式时验证两次新密码。它保留 Store ID 和数据主密钥，用所需 provider 生成版本 2 封装，原子保存策略和 slot。手动方式移除所有自动 slot，自动方式须确认 provider 可用后才发布，并保留密码恢复；保存失败时不退役原方式。系统中的旧孤立 KEK 或旧备份不属于安全擦除保证。
低层 `ChangePassword` / `Rewrap` 仅修改内存，不可直接作为 HTTP 成功响应的依据。

密码 provider 可在操作后 `Destroy`，清理其保留的密码副本；这不锁定全局存储。
Go 不能保证完全擦除进程内存，HTTP 调用方还需及时释放自己的输入缓冲，不记录或持久化密码。

## 认证文件头：80 字节

| 位置 | 字节数 | 内容 |
| --- | --- | --- |
| 0 | 6 | ASCII `TCPEFS` |
| 6 | 2 | 格式版本 1 |
| 8 | 32 | 随机 File ID |
| 40 | 24 | 随机文件头 nonce |
| 64 | 16 | 对空明文执行 AEAD 得到的认证标签，AAD 为前 64 字节 |

文件密钥由传入的用途密钥、File ID 和预期逻辑身份派生。
读取方必须传入预期身份，不能信任文件自报的 session/shell 身份。错误密钥和跨身份文件替换均不能通过文件头认证。
原子替换会产生新的 File ID，历史文件代际彼此分开。

## 追加记录

```text
uint32 总物理长度 | 24 字节随机 nonce | AEAD 密文及 16 字节认证标签
```

加密前的记录正文：

```text
uint64 sequence | uint64 generic_plain_offset | uint32 data_length | data
```

`sequence` 从 0 连续增加；通用明文偏移按 `data` 长度连续推进。
每次 `Append` 对应一条记录，`data` 为 1–1,048,576 字节；空追加不生成记录。
固定物理开销为 64 字节，总长度字段包含本字段；读取时先验证上下限，再分配内存。
序号、偏移、长度和正文均在密文内，公开记录头不会暴露事件时间和类型。

记录 AAD 为「完整 80 字节文件头 + 前一条记录的 16 字节认证标签 + 当前 28 字节公开记录头」。
首条记录使用文件头标签作为前驱。认证后还要检查序号、偏移、数据长度，拒绝中部删除、重排和拼接。
每次追加使用 `crypto/rand` 新生成 nonce，包括恢复后继续追加。
完整尾部删除和整套存储回滚仍需外部可信检查点才能检测。

`File` 提供 `Read`、`ReadAt`、`Seek`、`Size`、`Append`、`Sync`、`Close`。
`ReadRecord` 和 `Snapshot` 只向业务编码器及索引器提供已认证的位置；不接受索引自报的认证前驱，也不提供公共 `WriteAt` 或 `Truncate`。
读到的 record data 属于调用方，使用后可以清理。

## Session 事件与输出视图

每条通用加密记录承载一个事件；事件头也在密文内：

| 位置 | 字节数 | 内容 |
| --- | --- | --- |
| 0 | 2 | 事件版本 1 |
| 2 | 1 | Output=1、Mark=2、Resize=3 |
| 3 | 1 | stdout=0、stderr=1；非输出事件必须为 0 |
| 4 | 8 | Unix 毫秒时间戳，非负 |
| 12 | 8 | 此事件之前累计的公开输出字节数 |
| 20 | 4 | 事件 payload 长度 |

Output payload 是原始输出字节，最大长度为通用记录上限减 24。
Mark payload 是现有 `api.LogMark` 的 JSON，只保存来源/审批状态、时间、输出偏移，不新增原始 stdin 录制。
Mark 中的偏移可以指向已写入输出的起点，不能大于当前输出大小。
Resize payload 是两个 uint16（列、行），必须均非零。

`Log.ReadAt` 只返回 Output payload，跨事件拼接，跳过 Mark 和 Resize。
通用明文偏移计算事件头及所有 payload，不能把它传给现有的 shell 历史 API。
`ReadByTime` 查找追加顺序中第一个时间戳不小于目标值的输出事件，再返回有大小上限的连续输出窗口。
检查点保存时间戳的前缀最大值，以二分查找处理系统时钟回拨；原始时间戳仍被保留。

## 可重建的加密索引

每 64 个事件生成一个检查点。`SaveIndex` 先同步日志，再通过加密临时文件原子发布索引。
索引使用单独的用途密钥和 `<events identity>/index` 逻辑身份。
解密后的固定头长 104 字节：

| 字段 | 字节数 |
| --- | --- |
| ASCII `TCPSIDX` + NUL | 8 |
| 版本 1、保留 0 | 2 + 2 |
| stride=64 | 4 |
| 事件日志 File ID | 32 |
| 事件数、公开输出大小、通用明文大小、物理文件大小 | 4 × 8 |
| 日志末尾认证标签 | 16 |
| 检查点数 | 8 |

每个条目 40 字节：uint64 事件序号、int64 通用明文位置、int64 物理位置、int64 输出偏移、int64 时间前缀最大值。
没有输出时最后一个字段为 -1。
File ID、完整前缀长度及末尾标签把缓存绑定到确切的日志代际。

当前 `Open` 仍完整扫描认证日志，并重建业务检查点。`ValidateIndex` 对比重建结果，绝不从 sidecar 导入未验证的位置。
缓存缺失、落后、损坏或属于另一代文件时，可以重新写出，不影响日志；它不是历史内容的权威来源。
底层暂时保留每条 AEAD 记录的内存引用，超长日志的内存仍随记录数增长；持久化索引尚不用于跳过启动扫描。
按行索引、来源区间/审批窗口查询已接入现有存储接口；启动时仍需扫描记录重建内存视图。

## 并发、持久化和恢复边界

单个文件只有一个写入者，Unix 使用 `flock`，Windows 使用文件字节范围锁；独立读者可以读取。
同一对象上的读写受锁协调，只在完整记录写出后发布内存中的可读末尾；部分写入后拒绝继续追加。
独立打开的只读对象是打开时的前缀快照；活会话查询应复用共享的 `Log` / `File` 对象。

`Append` 成功只代表可读，不代表断电后必然保留；调用方需要等待 `Sync` 成功。
同步失败会返回错误，`Close` 即使同步失败也会释放写锁和文件句柄。
基础库提供显式同步；存储适配层按每 1 秒或累计 256 KiB 执行 Sync，显式同步与关闭也会确认数据写入。索引失败只重试缓存，不把已同步的权威历史当成写入失败。

`Inspect` 只校验；`OpenAppend` 不自动修复。`RecoverTail` 是显式操作，只处理有界但未写完整的末尾框架。
截断前复制完整原始密文到权限为 0600 的随机 recovery 文件，先同步副本及平台支持的目录，再截断并同步日志。
完整记录认证失败、非法长度或业务事件不连续不会被自动截断。
不完整框架也可能来自恶意修改；调用方必须明确选择恢复策略，不能仅凭 EOF 宣称一定发生了崩溃。

Unix 原子替换在临时文件同步、关闭、rename 后同步父目录；Windows 使用带 `WRITE_THROUGH` 的替换和有界共享冲突重试。
Windows 目录持久化语义不同，子进程强制终止测试不能充当真实断电测试。

现有用例覆盖任意字节偏移、跨记录读取、控制序列/非 UTF-8、并发追加、竞争写入者、短写、Sync 错误、完整密文破坏、重排、中部删除、逐字节截断、保留恢复副本、子进程强制终止及恢复后追加。
密码用例覆盖重复确认、已解锁时仍验证当前密码、保存失败不退休旧密码、并发修改、保留其他 provider，以及修改密码不重写已有数据。
系统托管、真实电源故障、历史迁移与 Web UI 的行为不能据此宣称已通过测试。

密码学原语依据：[XChaCha20-Poly1305](https://pkg.go.dev/golang.org/x/crypto/chacha20poly1305)、[Argon2id](https://pkg.go.dev/golang.org/x/crypto/argon2)、[HKDF](https://pkg.go.dev/crypto/hkdf)。
