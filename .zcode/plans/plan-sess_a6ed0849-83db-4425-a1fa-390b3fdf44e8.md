# DJOneHub 多域名 + 局域网管理访问 + 凭据指南 实施计划

## Context（背景与目标）

排查确认：服务运行正常，但（1）"远程接听与配对"页 `/remote/` 的 Guard 只认 Host `127.0.0.1:7575`，用 `localhost` 打开即 403；（2）Hub 由 herdr（后台守护会话）派生，Keychain 因 `interactionNotAllowed=YES` 立即拒绝（`keychain_unavailable`，12ms 失败），导致远程连接 `disabled`、凭据从未自动刷新；（3）孤儿 audio-host 已清理（2611/13993，均需 SIGKILL）。

用户决策：**回环访问支持 localhost + 自定义域名别名**；**局域网设备经 imacmini.sgponte 访问全部管理功能（HTTP + 密码，接受家用局域网明文风险）**；另需凭据更新指南与长期有效评估。

## A. webadmin：多主机名 + 局域网密码模式（internal/webadmin/auth.go）

1. `Options` 新增字段：
   - `HostAliases []string`：额外允许的 Host（裸主机名，Open 时统一补上 origin 端口并小写化；校验字符集/长度 ≤253，去重）
   - `AllowInsecureHTTP bool`：显式允许非回环 HTTP origin——仅密码模式（`LocalNoPassword` 组合时 Open 返回 ErrConfig，保持"免密=仅回环"不变量）
   - `AcceptAnyHost bool`：跳过 Host 白名单（供 LAN 密码实例；防 DNS-rebinding 由强制登录 + SameSite=Strict Cookie + 同源 Origin 校验承担）
2. `Open()`：允许集合 = `{origin.Host}` ∪ aliases；放宽 auth.go:100-102 的回环 IP 限制（仅当 AllowInsecureHTTP 且非 LocalNoPassword）。
3. `Guard()`（auth.go:196-207）：Host 命中集合或 AcceptAnyHost；Origin 校验改为「等于 options.Origin **或** 等于 `scheme://+r.Host`（同源）」，`same_origin_json_required` 检查同规则。回环免密行为不变。

## B. Mac Hub：双监听架构（cmd/djonehub-macos）

新配置文件 `~/Library/Application Support/DJOneHub/web-access.json`（0600，加载失败仅告警不启用，fail-safe）：
```json
{ "version": 1, "lan_listen": "0.0.0.0:7576", "hosts": ["imacmini.sgponte"] }
```
- **回环监听（现状不动）**：127.0.0.1:7575 免密；隐式加入 `localhost` + `hosts` 别名（供 /etc/hosts 本机域名）。
- **局域网监听（新增，lan_listen 存在才启用）**：第二个 `webadmin.Open`（密码模式，Directory=`.../DJOneHub/web-lan`，`AllowInsecureHTTP:true, AcceptAnyHost:true`）；同一套页面与 API，但整体包 `Guard(Require(...))`——含目前**完全无鉴权**的主页面 `/api/*`（短信/拨号/设备）。首次使用走既有 setup token 流程（setup.json 落在 web-lan 目录，启动日志提示路径）。

代码落点：
- 新文件 `web_access.go`：配置加载+校验（模仿 device_hub.go:560 loadAliases，但值严格校验）+ 测试
- `remote_web.go`：`newRemoteWebManager(..., aliases []string)`；`remote_web_http.go` 的 `register(mux)` 改为 `register(mux, auth *webadmin.Auth)` 以便 LAN mux 复用（会话端点在密码模式自动变为 setup/login，auth.go:174-183 已支持）
- `device_hub.go:799` serveUSBDeviceHub：装配两个 listener（复用 relay 已有的双 server 模式，relay/main.go:161 附近），shutdown 一并收敛
- 前端：`web/remote/app.js`+`index.html` 会话面板加账号密码登录表单（密码模式且未认证时显示）；`web/app.js:87` 中心 `api()` 封装加 401→登录浮层→重试，POST 自动附 `X-DJOneHub-CSRF`（与 /api/remote-management/session 共用 Cookie 命名空间，一次登录两页通用）

## C. 测试

- `internal/webadmin/auth_test.go`（模仿 :138 TestLoopbackHTTPAndRebindingGuard）与 `local_test.go`：别名 Host/同源 POST 通过、未知 Host 403、免密+AllowInsecureHTTP 拒绝、AcceptAnyHost 未登录 401→登录 200→CSRF 强制
- `cmd/djonehub-macos`：remote_web_test.go:121 签名适配；新增 web_access_test.go；httptest 验证 LAN mux `/api/devices` 未登录 401、setup+login 后 200

## D. 构建、部署与验证

1. `go build ./...`、`go test ./internal/webadmin/... ./cmd/djonehub-macos/...`（全绿）
2. 核对 dist 构建方式（BUILD_AND_RUN_CN.md / scripts/，确认 opus 构建要求，避免 `opus.Available()` 失效），用同等方式构建
3. 备用端口实测（不碰运行中服务）：`--demo --listen 127.0.0.1:7999` + 测试配置 → curl 矩阵：`Host: localhost:7999` 200 / `evil.test` 403 / LAN 端口无 Cookie 401 → setup+login → 200
4. 部署：备份并替换 `/usr/local/libexec/djonehub/bin/djonehub-macos`（该目录 root 属主，可能需要你输入管理员密码），写入 web-access.json，**从 Terminal.app（GUI 会话）**执行 `djonehub stop && djonehub start --remote-profile web_71cb…`——同时根治 Keychain 会话问题；最终核对 `connection_state` 变 online、`access_expires_at` 持续滚动
5. 用另一台局域网设备实测 http://imacmini.sgponte:7576 登录访问

## E. 任务 3 交付：docs/CREDENTIAL_UPDATE_GUIDE.md（新文档）

- **体系事实表**：access 15 分钟 / refresh 30 天滑动窗口（store.go:34-35，每次刷新轮换+重置）/ pairing grant 一次性 5 分钟 / enrollment 10 分钟；iPhone 安装身份同 30 天规则
- **日常更新**：常驻连接自动续期（到期前 1 分钟，callsignal/client.go:248）；手动 `djonehub remote check` / 网页"刷新"
- **恢复流程**：`credential_recovery_required`/refresh_pending 处理；401 invalid_credential（30 天未刷新）→ 重新登记；**绝不恢复旧 refresh token**（重用即整身份撤销，store.go:527）
- **本机根因专节**：herdr 后台会话 Keychain 不可用 → 必须从 GUI 会话终端或 LaunchAgent 运行；附 LaunchAgent plist 模板（仓库目前无 launchd 模板，deploy/ 只有 relay 的 systemd）
- **长期有效评估结论**：设计上已支持（滑动窗口+自动续期），三个前提：GUI 会话可访问 Keychain、30 天内至少连接一次、不恢复旧凭据；如需更长可改 Relay 常量（附位置与安全权衡，默认不建议）
- 同步小改 MACOS.md:159 的 localhost-only 表述与 REMOTE_WEB_OPERATIONS_GUIDE.md 的访问地址说明

## 明确不做（可后续叠加）

HTTPS 传输（本方案 AllowInsecureHTTP 留有升级路径）、Relay TTL 参数化、launcher 脚本改动、iPhone Web 端适配。

## 风险提示

- 主页面 `/api/*` 从无鉴权变为 LAN 强制登录仅影响 LAN 监听；回环路径行为完全不变
- HTTP 明文为用户已确认的取舍，文档中将明示风险
- 替换系统安装目录二进制需管理员权限；替换前保留备份可回滚