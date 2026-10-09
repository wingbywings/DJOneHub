# 远程接听：第十批 Keychain 配置、刷新日志与原生命令

日期：2026-10-02。分支：`codex/remote-iphone-callkit`。

本批新增 macOS Keychain 凭据适配、跨进程刷新锁、重启后仍保留的事务状态，以及现有 `djonehub-macos` 程序中的注册/配对管理命令。现在可以通过明确的命令入口注册 Hub、生成配对交付文件、检查 WSS 鉴权及撤销安装身份。

**当前仍不能用 iPhone 接听真实来电。** 常驻 Hub 连接管理与实际来电分发、APNs、iOS CallKit App、TURN 和真机验收仍待完成，Mac 远程可用状态继续为 false。本批只增加凭据操作入口，没有自动启用来电通知。

## 实现

| 位置 | 作用 |
| --- | --- |
| `internal/relayprofile` | Hub profile、Keychain 适配、事务状态、私有文件检查和进程锁 |
| `internal/relayclient` | 带持久化日志的刷新构造器，创建配对、列出/撤销安装身份 |
| `cmd/djonehub-macos/remote_profile.go` | `remote` 子命令，在发现或打开电话模块之前执行并退出 |
| `internal/relay/identity` / `cmd/djonehub-relay` | 本机管理员按公钥指纹找回注册身份，以便撤销响应丢失后留下的身份 |

所有原生命令使用同一个 `djonehub-macos` 可执行程序，避免凭据管理工具与后端成为两个不同的 Keychain 应用身份。没有新增公开 Web 管理接口来读取或导出凭据。

## Keychain 与本地 profile

凭据放在当前用户默认的 macOS file-based Keychain 中，类型为 generic password，service 固定为 `org.djonehub.remote.identity.v1`，account 为 profile 名。名称只允许字母、数字、`_`、`-`，最多 64 字符。初次注册使用 Create，已有 profile 不能被覆盖。

单条 Keychain value 包含协议版本、profile 名、Relay origin、Ed25519 私钥种子、完整访问/刷新凭据以及事务阶段；替换一条记录同时更新这些字段。关闭 iCloud 同步，不使用 Secure Enclave，不给其他程序设置额外信任授权，也不降级为明文文件存储。Keychain password 数据由系统管理加密，参考 [Apple Keychain 添加凭据说明](https://developer.apple.com/documentation/security/adding-a-password-to-the-keychain)；macOS 两类 Keychain 的区别见 [TN3137](https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains)。

原生桥接使用 Security、Foundation 和 LocalAuthentication framework，并请求不进行交互式认证。Keychain 锁定、权限拒绝或系统错误返回失败和 OSStatus，不退回其他存储。该桥接需要 macOS + CGO；其他构建返回不支持。

**本批只完成原生桥接编译，未在真实登录 Keychain 上验证读写和授权弹窗行为。** 自动化测试使用隔离 Vault 替身，未读取、创建或修改你的真实 Keychain 项。开发构建重新签名/更换二进制后是否仍能访问旧项，以及发行版签名与 ACL 行为，还需要实际验证；不承诺临时构建之间自动继承访问权限。

profile 状态输出仅包含 Relay 地址、阶段、身份/模块范围、到期时间、原注册 grant ID 和公钥 SHA-256 指纹，不包含种子、访问 token 或刷新 token。

## 跨进程刷新与崩溃恢复

本机合作进程使用固定目录下的空锁文件串行操作同名 profile：

```text
~/Library/Application Support/DJOneHub/remote-profile-locks/<profile>.lock
```

目录必须由当前用户拥有且不向组/其他用户开放；锁文件为当前用户拥有的 0600 普通文件，拒绝符号链接和多硬链接。进程退出由内核释放 flock；保留空文件，避免删除锁 inode 导致并发进程拿到不同锁。目录内不写凭据。

每次获取/刷新凭据都在锁内重新读取 Keychain。另一个 CLI/后端进程刚完成轮换时，不会继续使用旧的内存副本。刷新获取锁会遵守 context 等待；只读状态或重复注册可能返回正在使用，而不会强行覆盖。

持久化阶段：

```text
不存在 → enrolling → ready
ready → refresh_pending → ready（新凭据）
```

- 注册前先保存新私钥与 `enrolling`；服务端返回有效身份且本地替换成功后才进入 ready。
- 刷新先申请并验证 challenge，再保存 `refresh_pending`，之后才提交已签名的轮换请求。
- pending 写入失败时不发送轮换。刷新成功后，把新凭据和 ready 一起写入 Keychain，再把新访问 token 返回给 WSS 续期逻辑。
- 响应丢失、进程退出或最终保存失败，重启仍会看到 enrolling / refresh_pending，拒绝生成连接配置或再次刷新旧凭据。
- 状态不确定时不做自动重试，不覆盖 profile，也不自动删除云端身份。恢复流程为确认并撤销原身份，然后使用新注册凭据和新 profile 名重新注册。

这解决的是“重启后盲目重用旧凭据”的问题，**没有实现丢失响应的幂等取回或无感恢复**。即使进程在保存 pending 后、发送请求前退出，也保守进入恢复流程。进程锁只协调使用此实现的程序，不能约束用户在 Keychain Access 中手动编辑项目。

## 使用命令

先构建同一个 Mac 程序。这里使用临时输出路径演示开发入口，正式使用应换成稳定签名的发行程序：

```sh
go build -o /tmp/djonehub-macos ./cmd/djonehub-macos
/tmp/djonehub-macos remote --help
```

### 注册 Hub

在 Relay 主机按第七批的 `provision` 生成 10 分钟有效的注册文件，通过你选择的安全方式交付到 Mac。文件必须是当前用户拥有、权限不向组/其他用户开放的普通文件；拒绝符号链接、多硬链接及超过 8 KiB 的输入。

```sh
/tmp/djonehub-macos remote enroll \
  --profile home \
  --relay https://relay.example.com \
  --enrollment-file "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"

/tmp/djonehub-macos remote status --profile home
/tmp/djonehub-macos remote check --profile home
```

`check` 必要时先刷新凭据，再建立受认证 WSS、检查 welcome 后立即关闭，不创建来电或媒体。若该 Hub 已在别处建立信令连接，Relay 的单连接限制可能拒绝检查，不会顶替已有连接。

这里的注册命令不会删除输入文件；成功后应移除已消费的交付文件。不会把文件中的 secret、私钥、token 放到命令行或标准输出。

### 生成配对交付文件

模块 ID 使用真实的 `dji-...` 标识，不能用显示别名。输出父目录必须是当前用户拥有的私有目录，输出路径必须是新的绝对路径：

```sh
mkdir -p "$HOME/Library/Application Support/DJOneHub/private"
chmod 700 "$HOME/Library/Application Support/DJOneHub/private"

/tmp/djonehub-macos remote pair \
  --profile home --modules dji-012345abcdef \
  --out "$HOME/Library/Application Support/DJOneHub/private/iphone-pairing.json"
```

输出文件以 0600 创建，包含 `{version:1, relay_url, grant}`，grant 为第七批的一次性配对凭据，有效期 5 分钟。当前还没有 App 扫码或导入界面。若输出失败，已经生成的 grant 仍会按期限失效，不覆盖原文件或把 secret 打印出来。

### 列出与撤销安装身份

```sh
/tmp/djonehub-macos remote installations --profile home
/tmp/djonehub-macos remote revoke --profile home --installation ios_ACTUAL_ID
```

撤销调用 Relay 的已认证权限接口，作用于指定安装身份；既有 WSS 撤销通知和会话收尾继续生效。本批没有真机通话可供执行挂断验收。

仅本机 HTTP/WS 开发时，相关命令需显式附加 `--insecure-loopback`，且 Relay origin 必须使用字面量 loopback IP。该选项不允许公网明文连接。所有命令拒绝不适用的参数；不接受 `--token` / `--private-key` 之类把秘密放进进程参数的入口。

## 不确定状态的恢复

1. 在 Mac 执行 `remote status --profile <name>`，读取 phase、已知 Hub ID 和 public_key_fingerprint。
2. 在 Relay 主机通过本地状态目录执行 lookup，找到对应身份；该操作没有暴露到公网 API：

```sh
./djonehub-relay lookup \
  --state-dir /absolute/private/relay-state \
  --key-fingerprint ACTUAL_64_HEX_SHA256
```

3. 确认结果后，使用已有 `djonehub-relay revoke --state-dir ... --id hub_ACTUAL_ID` 撤销身份。若有子安装，它们也会失效。lookup 返回 ID、租户、角色、Hub 归属和撤销状态，不返回密钥/凭据。
4. 重新 provision，用新的 profile 名重新 enroll；旧 profile 保留供排查，不自动删除。lookup 无结果时仍不重复消费原 grant，使用新的注册凭据。

这让服务端已经注册成功但客户端还不知道 Hub ID 的情形也有明确的查找与撤销路径。实际操作应由拥有 Relay 状态目录权限的操作者完成。

## 验证及边界

新增 9 项自动化测试，覆盖：

- 注册、重新加载、刷新保存、状态无 secret、文件系统只出现空锁文件。
- 丢失刷新响应后重新构造本地 Store，仍拒绝旧凭据；pending 写入失败时零轮换请求；最终保存失败时保留 pending。
- 注册响应丢失后按指纹查询并撤销；多个 Store 实例竞争同一实际 flock，等待取消和重新加载最新凭据。
- 私有输入/输出、符号链接、公开权限、路径穿越与禁止覆盖。
- 实际 TLS Relay 下执行注册、配对、列表、撤销和 WSS 检查的 CLI 流程；help/错误参数不访问 Keychain、不输出参数中的秘密。

已通过全仓库 Go 回归、受影响包竞态及真实 WSS/Pion/Opus 回归；Mac 与 Relay 程序构建成功，构建出的 Mac 程序 `remote --help` 正常退出。Keychain 原生桥接编译通过，实际存储/签名验证仍待完成。

```sh
go test ./...
go test -race -tags remoteopus ./internal/relayprofile ./internal/relayclient ./internal/callsignal ./internal/relay/... ./internal/remotecall ./cmd/djonehub-macos ./cmd/djonehub-relay ./internal/remotemedia/... ./internal/callcontrol -timeout 90s
```

下一步将 profile 接入常驻 Hub 连接管理、配对界面和来电调度，并继续 APNs/iOS App 集成。当前只提供可调用的命令和存储组件，没有启动后台常驻连接或真实来电处理。
