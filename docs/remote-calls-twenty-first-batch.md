# 第二十一批：R11 配对和配置入口、手机通话记录

日期：2026-10-12。沿用 `codex/remote-iphone-callkit` 工作区，未提交、未部署。

## 本批实现

- 新增 `macos/DJOneHubRemote` 原生 SwiftUI 窗口，随 Mac 包提供 `djonehub remote-ui`。
  支持读取已登记的 profile、选择模块、生成一次性配对二维码、列出安装身份和确认撤销。
  通过同目录的 Hub 可执行文件调用既有 CLI，保留 Hub 的 Keychain 访问身份和跨进程凭据锁；
  无 shell 拼接、无密钥 argv，不新增浏览器凭据接口。子进程有 30 秒上限，输出限制 128 KiB。
- 二维码使用系统 CoreImage 生成。配对文件暂存在随机 0700 目录、由原 CLI 以私有权限写入，
  读取并校验后删除；二维码只在内存展示，过期不再显示，切走窗口时隐藏。
  二维码容量有限，模块过多时提示减少模块；原有 CLI 文件方式保留。
- 网页“来电”页新增当前模块远程配置：开关、目标安装 ID、响铃超时、来电显示及有效路由。
  AI 接听优先规则保持不变；通话中由服务端拒绝配置修改。请求捕获模块路径，迟到读取/保存结果
  不覆盖新模块界面；读取失败时禁止保存，避免空表单覆盖旧配置。
- iPhone 使用 VisionKit 扫描二维码，显示 Relay origin、授权模块和到期时间，由用户确认后配对。
  复用原严格 PairingFile 校验，确认时再次校验有效期，不打开二维码 URL，不记录其内容。
  相机不可用、拒绝授权或硬件不支持时保留 JSON 文件导入。关闭扫码页面或进入后台即停止扫描。
- iPhone 新增最多 100 条本机通话记录：方向、模块、号码/显示名、开始时间、连接后时长和结束结果。
  以 UUID 去重和隔离；有效 PushKit 提示可在收到匹配模块邀请后补充显示名；迟到事件不能覆盖已结束记录。
  重启将未结束项标记为“App 退出，结束状态未确认”，不用于恢复或回拨。
  采用原子写和系统文件保护，排除备份，可清除已结束记录；读取失败时禁止覆盖原文件。
- 顺带将 iOS `.ended` 的 UUID 检查前移至清理动作之前，避免旧会话结束事件清理当前会话的恢复任务。
- 发行脚本、启动器和包自检已纳入新管理窗口，保留现有 AudioHost 和 remoteopus 构建。

## 验证

- PhoneCore：32 项通过，包含历史去重、连接时长、崩溃恢复、100 条上限、错误模块及迟到事件隔离。
- Mac 管理：5 项通过，包含实际子进程分块输出、空安装列表、失败退出、profile 参数验证，
  CoreImage 编码后由 Vision 实际解码并比对原始配对 JSON，以及过期/非 HTTPS origin 拒绝。
- iOS 模拟器：构建成功；6 项通过，1 项需要外部互通 fixture 的测试按预期跳过。
- Go：Hub 与 relayprofile 的 remoteopus/race 回归通过。
- 网页：`node scripts/test-remote-settings.mjs` 验证跨模块迟到读取和保存隔离；JS、shell 语法及 diff 检查通过。
- 日志：`/tmp/djonehub-r11-core-final.log`、`/tmp/djonehub-r11-mac-final.log`、
  `/tmp/djonehub-r11-ios-final.log`、`/tmp/djonehub-r11-go.log`。

## 使用与验收边界

1. Hub 首次 enrollment 仍使用既有 CLI；本窗口管理已登记 profile。
2. 运行 `djonehub remote-ui`，读取 profile、选择模块、生成二维码；手机扫码核对后确认。
3. Mac 刷新安装列表，复制安装 ID，打开网页对应模块远程设置并保存。不要开启 AI 接听来验证远程来电。
4. 撤销身份会使远程授权失效；随后关闭旧目标或选择新安装身份。

本批未替换正在运行的 Hub/AudioHost、未向真机安装、未变更 Relay、未开启恢复特性开关。
尚需实际相机扫码、Keychain 授权、撤销、模块配置和前台/后台通话记录的端到端验收。
手机未收到有效信令/推送的来电不会记录；手机记录不是服务端全量通话账本。Mac 配对窗口仍需终端命令启动，
Finder 安装器和正式分发签名仍属于 R13/R14 的交付范围。

R04–R11 尚未整体关闭：R06 旧/新通路并行迁移、R07 AudioHost 新租约恢复仍需实现；
R04–R10 三端升级、真实切网/到期边界/故障注入验收仍欠缺，不能以自动化结果替代。

## 本批 Mac 升级包

- 文件：`dist/release/DJOneHub-macOS-arm64-remote-r11-20261012.zip`
- SHA-256：`f9ba8b3d81b77ce2b70dcf383f87ee1c0f1f64d7e8c8e353f5602ad3c2aeb0f4`
- 包含 R04–R11 当前实现（仍含上述未完成边界），新原生配对管理窗口已加入；恢复/控制开关继续默认关闭。
- `scripts/check-macos-package.py` 通过：隔离 demo 的 Opus 1.6.1 API、随包动态库依赖、四个 Mach-O 的签名、许可证、启动器参数。
- ZIP 的 SHA-256 校验通过；日志 `/tmp/djonehub-r11-package.log`、`/tmp/djonehub-r11-package-check.log`。
- iOS 最后加入“历史读取失败不覆盖原文件”保护后的 `build-for-testing` 再次通过，日志 `/tmp/djonehub-r11-ios-build-final.log`。
- 此包尚未实际安装；它不是 R06/R07 全部完成或真机验收通过的声明。
