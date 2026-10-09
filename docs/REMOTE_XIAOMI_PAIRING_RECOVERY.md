# Xiaomi 来电不触发 CallKit：模块授权修复

模块范围和路由修复后仍失败时，继续按 [信令/TURN 分阶段排查](REMOTE_TURN_TROUBLESHOOTING.md) 检查。常驻 Hub 在线时不要运行 `remote check` 建立第二条 Hub 连接。

2026-10-09 本机实查结果：

| 项目 | 实际状态 |
| --- | --- |
| Xiaomi 模块 | `dji-75d81d083dca`，ready，具备 USB 音频接口 |
| 旧 home profile 的模块范围 | `dji-xiaomi`，与真实 ID 不同 |
| 已配对 iPhone | `ios_ADCILkRceKTAeZ3MInYYrA`，模块范围同样为 `dji-xiaomi` |
| Hub 信令 | online |
| Xiaomi 接听路由 | local；远程开关 false、目标安装 ID 为空 |
| 音频宿主 | registered=true；无通话时 running=false，不据此判定故障 |

终端 C 使用 `dji-REPLACE` 导致不存在的模块返回 409；不能将该 409 一概解释为通话中的路由锁。`ios_REPLACE` 也不是实际安装身份。即使只替换这两个占位值，旧 Hub/iPhone 仍没有真实模块权限，所以必须先修复注册范围。`Xiaomi` 是显示别名，不能自行拼接为 `dji-xiaomi`。

原终端 C 示例中的 `#` 在未开启 interactivecomments 的交互式 zsh 中会被当作命令；新的配置入口使用 Bash 脚本，遇到 HTTP 错误会保留错误原因并立即停止，避免继续处理空文件。

以下只修复身份/配对和本机路由，公网 IP、域名及证书路径不变，不重装 Relay/Nginx/coturn。

## 1. CentOS 生成正确范围的新注册文件

准备好 Mac 再签发，10 分钟内完成注册。在服务器执行：

```sh
DJ_ENROLL_FILE="enrollment-xiaomi-$(date +%Y%m%d-%H%M%S).json"
sudo -u djonehub /usr/local/bin/djonehub-relay provision \
  --state-dir /var/lib/djonehub-relay \
  --tenant home --modules dji-75d81d083dca \
  --out "/var/lib/djonehub-relay/transfer/$DJ_ENROLL_FILE"
install -d -m 700 "$HOME/djonehub-transfer"
sudo install -o "$(id -un)" -g "$(id -gn)" -m 600 \
  "/var/lib/djonehub-relay/transfer/$DJ_ENROLL_FILE" \
  "$HOME/djonehub-transfer/$DJ_ENROLL_FILE"
printf '注册文件：%s\n' "$HOME/djonehub-transfer/$DJ_ENROLL_FILE"
```

保留终端打印的文件名。使用 SSH 下载私有副本到 Mac，不把文件放到公开网站。

## 2. Mac 注册新的 home-xiaomi profile

先结束测试电话、让 iPhone 断开连接，再在终端 A 用 Ctrl-C 停止旧 home 的常驻后端。终端 B 可保持运行，后端恢复后宿主会重新读取 bootstrap。

在 Mac 执行，替换 SSH 用户和上一步实际文件名：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
DJ_PRIVATE="$HOME/Library/Application Support/DJOneHub/private"
mkdir -p "$DJ_PRIVATE"
chmod 700 "$DJ_PRIVATE"
scp SSH_USER@47.108.184.34:djonehub-transfer/上一步实际文件名.json \
  "$DJ_PRIVATE/enrollment-xiaomi.json"
chmod 600 "$DJ_PRIVATE/enrollment-xiaomi.json"
./dist/djonehub-macos remote enroll --profile home-xiaomi \
  --relay https://oops.work \
  --enrollment-file "$DJ_PRIVATE/enrollment-xiaomi.json"
./dist/djonehub-macos remote status --profile home-xiaomi
./dist/djonehub-macos remote check --profile home-xiaomi
```

新的 status 必须是 phase=ready、modules 包含 `dji-75d81d083dca`。如果这一步已成功，不要重复 enroll。旧 home 保留，不覆盖或删除 Keychain。当前没有直接修改现有 Hub 模块授权的 CLI，因此使用新 profile。

## 3. 撤销旧手机安装，按正确模块重新配对

Mac 执行：

```sh
./dist/djonehub-macos remote revoke --profile home \
  --installation ios_ADCILkRceKTAeZ3MInYYrA
```

iPhone App 断开连接后，选择“清理本机配对凭据”。这一步在撤销旧身份后进行，不需要卸载 App。

接着在 Mac 生成新文件，5 分钟内导入 iPhone：

```sh
./dist/djonehub-macos remote pair --profile home-xiaomi \
  --modules dji-75d81d083dca \
  --out "$HOME/Library/Application Support/DJOneHub/private/iphone-xiaomi-$(date +%Y%m%d-%H%M%S).json"
open "$HOME/Library/Application Support/DJOneHub/private"
```

AirDrop 后在 App 导入新的 JSON。成功后执行：

```sh
./dist/djonehub-macos remote installations --profile home-xiaomi
```

应看到新的 ios_ 安装 ID，模块范围包含 `dji-75d81d083dca`。旧 ios_ ID 不复用。新 ID 无需手填到后面的配置命令，脚本会在唯一匹配时选择它。

## 4. 重新启动 A，然后用一条命令完成 C

终端 A：

```sh
./dist/djonehub-macos --listen 127.0.0.1:7575 \
  --remote-online-profile home-xiaomi \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

终端 B 仍使用原来的 Swift 宿主和相同 bootstrap 路径，无需修改。

终端 C：

```sh
bash scripts/configure-remote-answering.sh \
  --profile home-xiaomi --module dji-75d81d083dca
```

脚本依次核对真实模块、Hub 授权、手机安装授权、无通话、Opus 和在线功能配置，再关闭该模块 AI 并启用远程路由。多台匹配 iPhone 时会停止并要求指定 `--installation 实际ID`；不会猜选。任一步失败立即停止，先修复错误再重跑。两个配置 PUT 不是单个事务；若配置期间新来电导致中途失败，先结束来电再重跑。

可加 `--check-only` 只检查，不修改接听配置。脚本沿用现有 CLI 读取身份；必要时 CLI 会正常刷新访问凭据，不创建新 Hub、配对或通话。

确认输出 `effective_route=remote`、`config.enabled=true`、目标为新 ios_ ID，并等 `online_transport.state=online`。当前 `available:false` / `online_only_not_production` 仍是预期的开发状态。

最后让 iPhone 点击“连接并等待来电”，保持前台；第三部电话**重新拨打一通**。此前在错误路由或离线状态下出现的来电不会自动重放到手机。如果仍不响，提供脚本完整错误/状态及这通电话对应的 Hub 日志，再排查 TURN、CallKit 报告与媒体握手；本次尚未验证真实电话双向音频。
