# 远程接听：第七批配对与安装身份

日期：2026-10-01。分支：`codex/remote-iphone-callkit`。

本批新增独立 Relay 身份服务、SQLite 持久化、单次配对、Ed25519 持有证明、凭证轮换与撤销，以及供 macOS/iOS 共用的 Swift 签名库。**当前仍不能通过 iPhone 接听实际电话：公网通话信令、Mac 配对界面、APNs、TURN 配置和 iOS App 尚未接入。** 原有远程能力状态继续返回不可用，AI 路由隔离保持不变。

## 实现入口

| 路径 | 职责 |
| --- | --- |
| `cmd/djonehub-relay` | 独立服务启动、离线签发 Hub 注册凭据、离线撤销 |
| `internal/relay/identity` | 身份、授权范围、事务兑换、刷新与撤销 |
| `internal/relay/server.go` | 有大小与频率限制的 HTTP 身份 API |
| `shared/DJOneHubIdentity` | CryptoKit Ed25519 签名、规范消息编码，支持 macOS 13 / iOS 17 及以上 |

Relay 身份库与现有 Mac 配置分开。一个租户可以有多个 Hub；安装身份绑定一个 Hub 和明确的模块范围。请求不能自行指定租户、Hub 归属或扩大权限。APNs token 不作为身份凭据。

## 注册、配对与授权

1. 操作者在 Relay 主机运行 `provision`，指定租户和模块 ID，得到有效期 10 分钟的一次性 Hub 注册凭据。该管理操作不暴露为未认证 HTTP 接口。
2. Hub 生成 Ed25519 私钥，以注册凭据和签名调用 `/v1/hubs/enroll`。服务端生成 Hub ID 并固定其公钥，返回访问及刷新凭据。
3. Hub 用访问凭据创建有效期 5 分钟的配对凭据，模块范围必须是自身范围的子集。未来 Mac 界面会把 Relay 地址和配对凭据交付给 iPhone；二维码和深链尚未实现。
4. iPhone 生成自己的私钥，用配对凭据和签名调用 `/v1/pairings/redeem`。服务端生成安装 ID 和授权范围；多个请求同时兑换，只允许一个事务成功。
5. Hub 可以列出、撤销自己的安装身份；安装身份不能创建配对、列出其他设备或自行提升权限。

每份授权最多包含 32 个模块；每个 Hub 最多 64 个未撤销安装身份、16 个未消费且未过期配对凭据。离线 Hub 注册凭据另有全库 16 个待消费上限。模块 ID 使用实际的 `dji-...` 标识，不使用可变的显示别名。现有单模块空 ID 到稳定模块 ID 的映射留待 Hub 信令适配时实现，当前不能直接用空 ID 注册。

撤销保留身份记录与版本，重启不会恢复授权。同一个 Hub 下已使用过的安装公钥不能再次配对；重新安装或恢复配对需要生成新密钥，并撤销旧安装身份。撤销 Hub 同时使子安装、访问/刷新凭据及未兑换配对失效。

## 凭据与消息格式

这是版本化的专用身份协议，不宣称兼容 OAuth 或 DPoP。

- 随机凭据使用 32 字节密码学随机数，以无填充 Base64URL 编码，长度 43 字符。数据库只保存凭据和 challenge 的 SHA-256 摘要。
- 访问凭据有效期 15 分钟，放在 `Authorization: Bearer ...` 中。当前访问凭据是 bearer token；私钥持有证明用于注册、配对和刷新，不用于每个普通 HTTP 请求。
- 刷新凭据有效期 30 天，每次成功刷新都轮换，并重新计算有效期。刷新前申请有效期 60 秒的一次性 challenge，同一身份最多 8 个待使用 challenge。
- 刷新需要同时持有刷新凭据及对应私钥。签名绑定身份、刷新凭据、challenge ID 和 nonce；无效签名不能消耗合法 challenge。
- 刷新后旧访问凭据可继续用到自身到期；撤销会立即让新旧访问凭据均无法通过鉴权。

注册与配对请求是 `GrantProof`：`id`、`secret`、`public_key`、`signature`。公钥和签名使用标准 Base64；签名消息为 UTF-8 JSON，字段顺序固定且无额外空白：

```text
{"domain":"DJOneHub/identity/grant/v1","kind":"pairing","id":"...","secret":"...","public_key":"..."}
```

Hub 注册使用 `kind=enrollment`。刷新请求是 `RefreshProof`：`identity_id`、`refresh_token`、`challenge_id`、`challenge`、`signature`，其签名消息为：

```text
{"domain":"DJOneHub/identity/refresh/v1","identity_id":"...","refresh_token":"...","challenge_id":"...","challenge":"..."}
```

请求体本身不要求 JSON 字段顺序；验签时服务端按上述格式重建消息。调用方应使用 Go 的 `GrantMessage` / `RefreshMessage` 或 Swift 的 `IdentityKey`，避免自行编码差异。Swift 库不写文件、不输出凭据；App 集成时仍需实现 Keychain 保存私钥与刷新凭据。没有 Secure Enclave 存储实现。

共享测试向量位于 `internal/relay/identity/testdata/proof-v1.json`，使用公开的固定测试密钥。测试严格比较消息字节，并分别用 Go 验证 CryptoKit 签名、用 CryptoKit 验证 Go 签名，不假定不同签名实现的输出字节相同。

### 刷新重试的当前边界

客户端必须串行刷新并原子保存新凭据。重复提交已经消费的同一个 challenge 会返回 401，但不会撤销成功轮换后的身份；用另一个有效 challenge 对已经轮换掉的旧刷新凭据签名提交，则判定为重用并撤销身份。

当前没有刷新结果幂等恢复：若服务端完成轮换，但客户端丢失响应或未保存新凭据，需要重新注册/配对，不能用旧凭据不断重试。后续客户端必须明确处理该错误，并在长通话接入前完善刷新与连接续期策略；本批不构成“凭据到期不会中断长通话”的验收。

## HTTP API

| 接口 | 输入 / 权限 | 成功响应 |
| --- | --- | --- |
| `GET /healthz` | 无身份凭据 | `identity=true`、`signaling=false`、`voip_push=false` |
| `POST /v1/hubs/enroll` | 注册 `GrantProof` | 201，身份及访问/刷新凭据 |
| `POST /v1/pairings` | Hub bearer；`{"modules":["dji-..."]}` | 201，单次配对凭据 |
| `POST /v1/pairings/redeem` | 配对 `GrantProof` | 201，身份及访问/刷新凭据 |
| `POST /v1/auth/challenge` | `identity_id`、`refresh_token` | challenge 的 `id`、`value`、`expires_at` |
| `POST /v1/auth/refresh` | `RefreshProof` | 轮换后的凭据 |
| `GET /v1/identity` | 有效 bearer | 当前身份、模块范围、版本、访问到期时间 |
| `GET /v1/installations` | Hub bearer | 自己未撤销安装身份的 ID 与模块范围 |
| `DELETE /v1/installations/{id}` | 所属 Hub bearer | `{"revoked":true}`，重复撤销幂等 |

所有响应 `Cache-Control: no-store`。JSON 最大 8 KiB，拒绝未知字段、额外 JSON 值、Origin 和 query string。401 表示凭据无效/过期/撤销；400 表示非法请求；429 表示频率或容量限制；内部错误不返回数据库错误细节。

当前全局 50 请求/秒、突发 100；每个真实 TCP peer IP 4 请求/秒、突发 20，IP 表最多 2048 项。健康检查不计入该限流。不信任 `X-Forwarded-For`：反向代理后的请求共用代理 IP 限额；面向多客户端部署前需配置代理侧限流并设计可信代理边界。此处没有自动信任转发头。

## 持久化与连接授权

SQLite 使用 WAL、`synchronous=FULL` 和 immediate 事务，注册/配对兑换与发放凭据一起提交，失败全部回滚。状态目录必须为私有目录（0700），数据库为私有普通文件（0600），拒绝数据库及邻接 WAL/SHM/journal 符号链接或公开权限文件。凭据不写入服务日志；运行目录应由服务专用账户控制。

固定依赖 `github.com/mattn/go-sqlite3 v1.14.52`，本次检验其绑定 SQLite 为 3.53.4。Relay 构建和运行需要 CGO 与 C 工具链，未使用系统 SQLite 动态库覆盖默认实现。参考 [go-sqlite3 官方说明](https://github.com/mattn/go-sqlite3) 与 [SQLite WAL 文档](https://www.sqlite.org/wal.html)。数据库应位于本机磁盘；备份需要使用 SQLite 一致性备份或停服后复制，不能只复制活动库的主文件而漏掉 WAL。

`AuthorizeModule` 每次从数据库校验存活身份、父 Hub、凭据期限和模块范围；不依赖客户端申报的身份。`AuthorizeContext` 提供撤销或过期后取消的授权上下文：同进程撤销主动唤醒，另一进程修改数据库约每秒复核。后续 WSS 必须订阅该取消、逐条命令重新鉴权，并将取消接入会话清理。这些接口当前还没有连接实际 WebSocket 或电话，不能据此声称“撤销已能挂断真实远程通话”。

## 开发启动

以下仅启动身份 API；需先安装 Go 和 C 工具链。示例模块 ID 必须替换为实际模块 ID。

```sh
go build -o /tmp/djonehub-relay ./cmd/djonehub-relay
mkdir -p "$HOME/Library/Application Support/DJOneHub/relay"
chmod 700 "$HOME/Library/Application Support/DJOneHub/relay"

/tmp/djonehub-relay provision \
  --state-dir "$HOME/Library/Application Support/DJOneHub/relay" \
  --tenant personal --modules dji-012345abcdef \
  --out "$HOME/Library/Application Support/DJOneHub/relay/enrollment.json"
```

`--out` 必须是新的绝对路径文件，以 0600 创建，拒绝覆盖已有文件。凭据不输出到 stdout；注册消费后应删除交付文件。当前没有已接入的 Mac 注册客户端，签名兑换由协议调用方完成。

仅本机开发：

```sh
/tmp/djonehub-relay serve \
  --state-dir "$HOME/Library/Application Support/DJOneHub/relay" \
  --listen 127.0.0.1:8443 --insecure-loopback
```

公网监听必须配置有效证书与私钥：

```sh
/tmp/djonehub-relay serve \
  --state-dir "$HOME/Library/Application Support/DJOneHub/relay" \
  --listen :8443 --tls-cert /absolute/path/fullchain.pem \
  --tls-key /absolute/path/privkey.pem
```

不带 TLS 的启动只允许显式指定 `--insecure-loopback` 和字面量 loopback IP；`0.0.0.0`、空 host 或 `localhost` 均不作为明文例外。若用反向代理终止 TLS，外部 HTTPS、证书续期、访问日志凭据脱敏由部署侧配置。

紧急撤销可以在同一主机另开进程执行：

```sh
/tmp/djonehub-relay revoke \
  --state-dir "$HOME/Library/Application Support/DJOneHub/relay" \
  --id hub_REPLACE_WITH_ACTUAL_ID
```

## 验证与后续

新增 16 项 Go 测试覆盖跨租户/同租户跨 Hub/跨模块授权、并发兑换单次消费、无效证明不烧毁凭据、事务故障回滚、过期与授权取消、刷新轮换/重用、跨连接撤销、重启持久化、无明文凭据落库、HTTP 输入限制/限流、TLS 启动约束，以及离线注册文件不覆盖和实际可兑换。Swift 新增 3 项测试覆盖跨语言验签、规范编码、字段边界及私钥恢复。

已通过整体 Go 回归、带 `remoteopus` 的受影响包竞态测试、Swift 身份库测试，并成功构建 Relay 可执行程序：

```sh
go test ./...
go test -race -tags remoteopus ./internal/relay/... ./cmd/djonehub-relay ./internal/remotecall ./internal/remotemedia/... ./internal/callcontrol ./cmd/djonehub-macos -timeout 90s
swift test --package-path shared/DJOneHubIdentity
go build -o /tmp/djonehub-relay ./cmd/djonehub-relay
```

测试在 macOS 上完成，没有部署公网服务或操作真实电话。本仓库已有精简后的 `third_party/x-sys` / `third_party/x-text` 替换目录导致 `go mod tidy` 无法完整遍历依赖；本批保留原替换，实际构建和上述测试通过。

鉴权 WSS、在线邀请及命令路由已继续实现并接入会话协调，见 [第八批鉴权 WSS 与 Hub 会话适配](remote-calls-eighth-batch.md)；常驻 Hub 连接管理和实际来电自动分发仍待完成。然后接 APNs/TURN、Keychain、CallKit 和真机验收。弱网自适应、时钟补偿、网络恢复与发布检查仍待完成。
