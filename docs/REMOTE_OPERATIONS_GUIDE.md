# 远程 iPhone 接听：部署、重启与扫码配对操作指南

更新：2026-10-12，适用于第二十一批 / R11 旧发行版本。**升级到 Web 配对版本后，请使用 [Web 操作指南](REMOTE_WEB_OPERATIONS_GUIDE.md)**；本文保留旧窗口/CLI、iOS 安装、协议开关与旧版本维护步骤。早期批次中的 8443 拓扑和当时未完成项只适用于历史场景。

## 1. 先选择你的操作场景

| 场景 | 操作 |
| --- | --- |
| 现有电话正常，只想重启 | 按第 2 节；不需要重新配对、重新生成 APNs 或重装 Relay |
| 更新到新 Mac 管理窗口和手机扫码/记录功能 | 按第 3、4 节升级；保留原 profile 和手机身份。已配对手机的扫码按钮禁用是正常现象 |
| 给新手机配对 | 按第 5、6 节；Mac 已登记的 profile 可以直接使用 |
| 旧手机确实需要重新配对/换机 | 按第 7 节，先撤销旧安装，再清理手机凭据；不要为了升级而执行此步骤 |
| 更新 Relay 或测试恢复/DTMF 新功能 | 按第 8、9 节；三端版本配套，先结束所有通话 |
| 全新 Mac、尚无 Hub profile | 按第 10 节完成 Hub 登记，再扫码 |

本线程已使用并确认过的参数如下。它们不是本次重新探测的实时状态；其他环境须替换为自己的值。

| 项目 | 本线程环境 |
| --- | --- |
| Mac 源码目录 | `/Users/yuyi/Documents/Code/DJOneHub` |
| 最新 Mac 包 | `dist/release/DJOneHub-macOS-arm64-remote-r11-20261012.zip` |
| Mac Hub profile | `home-xiaomi`；管理窗口默认填写 `default`，**需要手工改成 `home-xiaomi`** |
| Xiaomi 模块 ID | `dji-75d81d083dca`；用网页或 `/api/devices` 核对，别名 Xiaomi 不是 ID |
| Relay 公网地址 | `https://oops.work`，不是 `https://oops.work:8443`，也不是带 `/relay` 的地址 |
| Relay 内部监听 | Linux `127.0.0.1:18443`，由现有 Nginx 443 反代 |
| Mac 网页 | `http://127.0.0.1:7575` |
| iPhone 身份 | Team `8AHAFH9VTZ`，Bundle ID `com.oops.djonehub`，Debug / APNs sandbox |
| 手机安装 ID | 使用 App 或 Mac 列表当前显示的 `ios_...`，不要复制旧文档的示例 ID |

“Mac 配对”包含两个不同步骤：**Hub 登记**把这台 Mac 的身份保存到 Keychain；**手机配对**给 iPhone 授予指定模块权限。新窗口负责后者及安装身份管理，首次 Hub 登记仍用 CLI。

## 2. 日常重启：使用安装包启动器

先结束通话，让模块空闲。在终端执行：

```sh
djonehub stop
djonehub start --remote-profile home-xiaomi
```

没有 `djonehub restart` 命令。`start --remote-profile` 会同时启动 **Hub 和 AudioHost**，不用再单独启动宿主。启动终端保持打开；Ctrl+C 停止，或在另一个终端执行 `djonehub stop`。`djonehub start` 不带 profile 只启动本机模式，不能替代远程启动命令。Mac 运行命令不要加 sudo。

另一个终端可执行：

```sh
djonehub status
djonehub logs
```

`status` 只说明启动器管理的 Hub 进程是否存在；`logs` 持续显示日志，Ctrl+C 只退出日志查看。默认日志为 `~/Library/Logs/DJOneHub/djonehub.log`。

**如果目前仍是源码手工启动的两个进程**（例如 `dist/djonehub-macos` 与 `DJOneHubAudioHost`），`djonehub stop/status` 不负责它们。先到原先两个终端分别 Ctrl+C，再改用新启动器。找不到原终端时，用活动监视器核对这两个进程后退出；如果由其他进程管理器托管，先通过原管理器停止。不要重复启动、使用宽泛的 killall，或删除 bootstrap/锁文件绕过占用。

更新二进制后 macOS 可能弹出 Keychain 访问提示，允许你刚启动的 DJOneHub 读取原有配置。启动器约一分钟未就绪会停止本次启动并留日志；完成授权后重新启动。配对窗口的单次操作等待上限为 30 秒，遇到授权延迟可处理系统提示后重新“读取 / 刷新”。

## 3. 升级 Mac：保留现有配对

### 3.1 停止旧进程、保留旧程序

先按第 2 节停止旧 Hub 和 AudioHost。保留原 Release ZIP；如果使用 `/usr/local/libexec/djonehub` 安装目录，也可在覆盖前复制一份**程序目录**作为回退备份。

升级不要求删除 `~/Library/Application Support/DJOneHub` 或 Keychain。不要恢复旧 refresh token、清理 profile、删除 journal 或拷贝旧 bootstrap 来“修复”升级。

### 3.2 校验、解压并安装本批 ZIP

在 Mac 同一个终端依次执行：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub/dist/release
shasum -a 256 -c DJOneHub-macOS-arm64-remote-r11-20261012.zip.sha256
```

必须显示 `OK`，再执行下面这组命令。从 ZIP 解压到新的临时目录，避免误用此前修改过的展开目录：

```sh
DJ_INSTALL_WORK=$(mktemp -d /tmp/djonehub-install.XXXXXX)
ditto -x -k DJOneHub-macOS-arm64-remote-r11-20261012.zip "$DJ_INSTALL_WORK"
cd "$DJ_INSTALL_WORK/DJOneHub-macOS-arm64-remote-r11-20261012"
./install
```

安装器可能请求管理员密码。默认安装到 `/usr/local/libexec/djonehub`，命令为 `/usr/local/bin/djonehub`。**仅安装器需要提权；后续启动、配对均使用当前登录用户。** 安装器尝试停止启动器管理的旧服务，但不能替代第 3.1 节对手工进程的停机。

当前 ZIP 的 SHA-256 为：

```text
f9ba8b3d81b77ce2b70dcf383f87ee1c0f1f64d7e8c8e353f5602ad3c2aeb0f4
```

不安装也可以在完整解压目录执行 `./djonehub start --remote-profile home-xiaomi`；后面的 `remote-ui` 等命令同样改成 `./djonehub ...`。不要混用多个目录的二进制。

### 3.3 验证 profile 并启动

```sh
djonehub remote status --profile home-xiaomi
djonehub start --remote-profile home-xiaomi
```

`status` 应显示 `phase: ready`、`relay_url: https://oops.work`、授权模块包含 `dji-75d81d083dca`。该命令不打印访问 token。如果不是 ready，先处理身份状态，不重复 enroll 或消费旧注册文件。

不要在常驻 Hub 已在线时运行 `remote check`，它会尝试建立另一条 Hub WSS。`remote status` 和 `remote installations` 可以用于日常查询。

## 4. 升级 iPhone：先保留配对，再使用新功能

Mac ZIP 不包含可直接安装的 iPhone App。扫码和通话记录需要更新本次源码的 iOS App；目前没有已交付的 TestFlight 或发行 IPA。

```sh
open /Users/yuyi/Documents/Code/DJOneHub/ios/DJOneHubPhone/DJOneHubPhone.xcodeproj
```

1. 连接并解锁原 iPhone，在 Xcode 选择 **DJOneHubPhone** scheme 和真实 iPhone。
2. 保持原来的 Team `8AHAFH9VTZ`、Bundle ID `com.oops.djonehub`、自动签名。Run 使用 **Debug**，与现有 sandbox APNs 对应；不要为升级切换 Bundle ID 或 Release/production。
3. 点 Run 覆盖安装，**不要先卸载旧 App**。项目已有相机使用说明、麦克风说明、`audio/voip` 后台模式和推送 entitlement。
4. 打开 App，确认原安装 ID/配对仍在，新界面出现“通话记录”和“扫描 Hub 配对二维码”。已有配对时扫码按钮不可点是预期行为；无需为了使用新版而清理凭据。
5. 连接时允许麦克风；实际扫码时才需要相机。后台接听查看“后台来电”状态；未启用则点“启用后台来电”。已经启用时打开 App 会尝试同步推送注册，不必为了升级先关闭再打开。

如果签名失败，先处理 Xcode 账号、设备信任或 provisioning 错误。`CODE_SIGNING_ALLOWED=NO` 仅用于构建检查，不能用于真机部署。相机不支持或权限被拒绝时可以按第 5.3 节导入文件。

## 5. Mac 管理窗口与 iPhone 扫码配对

### 5.1 Mac 生成配对码

确保 Relay 可访问，Mac profile 已 ready。在**另一个终端**执行：

```sh
djonehub remote-ui
```

1. 把顶部 profile 从 `default` 改为 **`home-xiaomi`**，点击“读取 / 刷新”。
2. 核对窗口中的 Relay 为 `https://oops.work`。
3. 勾选授权模块 `dji-75d81d083dca`。列表只包含该 Hub 已授权的模块，不能用配对界面给 Hub 扩大权限。
4. 点“生成一次性配对二维码”。保持窗口在前台，准备手机扫码。

二维码通常有效 **5 分钟**，只能消费一次。Mac 窗口失去焦点时会隐藏二维码；若切换其他 Mac 窗口后看不到，回到配对窗口重新生成即可。隐藏不会撤销已经签发的 grant，它仍按原到期时间失效。模块过多导致二维码生成失败时，减少所选模块或改用文件方式。

窗口只负责管理配对，不代替 Hub/AudioHost 常驻服务。关闭窗口不会停止电话服务。它通过同目录的 Hub 程序访问 Keychain，不需要把 token 填入网页或手机。

### 5.2 手机确认配对

适用于尚未配对的手机；已有身份确需替换时先做第 7 节。

1. 手机打开 DJOneHub → “配对与接听” → **“扫描 Hub 配对二维码”**，允许相机。
2. 扫描 Mac 屏幕上的二维码。
3. 在手机确认页核对服务器、模块列表、有效期，再点 **“确认配对”**。
4. 配对成功后记录手机连接状态中的安装 ID `ios_...`。App 自动使用二维码中的 Relay 地址，无需另填服务器。
5. Mac 点“读取 / 刷新”，在“已配对 iPhone”中确认同一个安装 ID及模块范围。

二维码无效/过期且尚未提交确认时重新生成；如果确认后报“配对失败”且 App 提示凭据更新未完成，可能存在服务端已消费、本机未确认的状态，按第 7 节处理，不反复消费同一二维码。

### 5.3 文件导入备用路径

在 Mac 执行；输出使用新的文件名，不覆盖旧文件：

```sh
DJ_PAIR_DIR="$HOME/Library/Application Support/DJOneHub/private"
mkdir -p "$DJ_PAIR_DIR"
chmod 700 "$DJ_PAIR_DIR"
DJ_PAIR_FILE="$DJ_PAIR_DIR/iphone-$(date +%Y%m%d-%H%M%S).json"
djonehub remote pair --profile home-xiaomi --modules dji-75d81d083dca --out "$DJ_PAIR_FILE"
open "$DJ_PAIR_DIR"
```

通过 AirDrop 等私有传输送到手机“文件”App，在 DJOneHub 点击“导入 Hub 配对文件”。它同样 5 分钟有效、一次性消费；完成后移除两端交付文件。配对 JSON、二维码、APNs JSON、`.p8` 和 native bootstrap 各有用途，**手机只接受配对 JSON/二维码**。

## 6. 配对后给模块指定目标手机

配对只授予权限，不会自动打开远程接听或覆盖模块原来的目标。

1. 打开 `http://127.0.0.1:7575`（或 Mac 配对窗口的“打开模块远程接听设置”）。
2. 在网页顶部选择正确模块，进入“来电”，展开 **“iPhone 远程接听”**。
3. 勾选“启用此模块的远程接听”；粘贴第 5 节确认的实际安装 ID；响铃超时默认 45 秒，可选 10–90 秒；来电显示选择电话号码或仅模块。
4. 在没有振铃/通话时保存。每个模块独立配置；给手机授权多个模块不等于所有模块自动启用。
5. 如果希望此模块走手机接听，到 AI 配置中关闭该模块 AI 开关并保存。**AI 启用时只走 AI，远程不会收到通知；AI 出错也不会自动转远程。** 新远程配置页面不会偷偷关闭 AI。
6. 手机点“连接并等待来电”，等待在线；启用后台来电后检查注册状态。

可选的只读核对命令：

```sh
curl -fsS http://127.0.0.1:7575/api/devices
curl -fsS http://127.0.0.1:7575/api/devices/dji-75d81d083dca/remote-calls/status
curl -fsS http://127.0.0.1:7575/api/devices/dji-75d81d083dca/calls/status
djonehub remote installations --profile home-xiaomi
```

确认 `effective_route=remote`、`online_transport.state=online`、`codec.available=true`。无通话时 AudioHost `running=false` 正常，主要检查是否已注册及有无错误。整体 `available:false` 和 `remote_validation_pending` 仍是开发状态标记，不能单独判定故障；`online` 也不能替代实际双向音频验证。

旧脚本 `scripts/configure-remote-answering.sh` 会关闭 AI 后配置远程。日常使用本节网页流程，不必同时运行旧脚本。

## 7. 撤销、换机与异常配对恢复

正常更新 App/Hub 不做这一步。确定要替换身份时，在无通话时操作：

1. 手机先断开连接。Mac 管理窗口读取正确 profile，核对安装 ID后点“撤销”，确认。不要撤销整个 Hub 身份来替代撤销手机。
2. 手机“恢复配对” → “清理本机配对凭据” → 确认已撤销旧身份。清理本机不会撤销服务器身份，顺序不能反过来。
3. 按第 5 节重新生成、扫码，获得**新的**安装 ID。
4. 按第 6 节更新所有指向旧 ID 的模块配置，再启用手机后台来电。

如果不再使用该手机，撤销后关闭相应模块的远程接听或指定其他已授权手机。CLI 等价操作为 `djonehub remote revoke --profile home-xiaomi --installation 实际安装ID`。

若 App 处于 pending、没有安装 ID但有公钥指纹，按 [身份恢复指南](REMOTE_XIAOMI_PAIRING_RECOVERY.md) 的 Relay lookup/revoke 流程定位身份。Hub profile 非 ready 时需要专门恢复，不删除整个 Keychain/Relay 数据库解决。

## 8. 公网服务：只重启，或升级源码

### 8.1 日常重启

目前 APNs/后台接听已由你确认通过，普通 Mac/iOS 更新不要求重新生成 `.p8`、重新登记设备或改 TURN secret。在 Linux 服务器执行；重启会断开现有信令，应先结束通话：

```sh
sudo systemctl restart djonehub-relay
sudo systemctl is-active djonehub-relay djonehub-turn
curl -fsS http://127.0.0.1:18443/healthz
curl -fsS https://oops.work/healthz
```

只有修改 coturn 配置、证书或其本身故障时才另外执行 `sudo systemctl restart djonehub-turn`；这会影响媒体分配。不要为了重启 Relay 重装 Nginx 或改成 8443。

健康响应应具备 identity、signaling、outgoing_calls、turn、voip_push。健康接口没有精确构建版本或 R04–R08 的完整能力清单，**不能靠 healthz 推断恢复协议已经升级**。失败时查看 `sudo journalctl -u djonehub-relay -n 80 --no-pager`，不输出私有配置内容。

### 8.2 升级到当前工作区 Relay

仅使用新配对 UI/历史记录且不开启实验开关时，不需要为了 R11 本身重做 Relay 配置。要使用第 9 节新协议，先升级 Relay，再升级 Mac/iPhone。

旧的源码包可能早于当前实现。由你在 Mac 重新打包当前工作区（包含未提交实现，脚本使用文件白名单，不打包 dist 私钥）：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
python3 scripts/package-relay-apns.py
scp dist/djonehub-relay-apns-source.tar.gz dist/djonehub-relay-apns-source.tar.gz.sha256 root@47.108.184.34:/root/
```

使用已核对身份的 SSH 连接。此处仅传源码和校验文件；已有 APNs 可在服务器复用，**无需再次上传 `.p8`**。

下面按 `root` 登录服务器编写，在服务器同一个终端执行：

```sh
cd /root
sha256sum -c djonehub-relay-apns-source.tar.gz.sha256
```

必须通过后解压到新目录，避免旧源码残留：

```sh
DJ_RELAY_WORK=$(mktemp -d /root/djonehub-relay-upgrade.XXXXXX)
tar -xzf /root/djonehub-relay-apns-source.tar.gz -C "$DJ_RELAY_WORK"
cd "$DJ_RELAY_WORK/djonehub-relay-apns"
DJ_APNS_WORK=$(sudo mktemp -d /root/djonehub-apns-input.XXXXXX)
sudo install -o root -g root -m 600 /etc/djonehub/apns.json "$DJ_APNS_WORK/apns.json"
sudo bash scripts/upgrade-relay-apns.sh --apns-config "$DJ_APNS_WORK/apns.json"
```

这里的 APNs 临时副本有必要：升级脚本以 root 预校验，要求输入文件归 root 所有；在用 `/etc/djonehub/apns.json` 归 djonehub 所有，不能直接把它当 root 校验输入。脚本会把最终配置重新安装为 djonehub/0600。以上命令适用于原有 systemd + loopback 18443 安装布局；脚本会拒绝未知定制布局，不强行覆盖。

脚本先构建/校验，再停服务、备份二进制/配置和一致的 SQLite 快照、替换、重启，打印私有备份目录。保存该路径。确认成功并通过第 8.1 节健康检查后，删除此次临时 APNs 输入副本：

```sh
sudo rm -- "$DJ_APNS_WORK/apns.json"
sudo rmdir -- "$DJ_APNS_WORK"
```

Go 要求、失败后停服和数据库回退规则见 [Relay 升级说明](REMOTE_APNS_UPGRADE.md)。此流程由你自行在服务器执行；本文更新不会远程重启服务。

## 9. 实验功能开关：与日常启动分开

本批 ZIP 包含相关实现，但普通 `djonehub start --remote-profile home-xiaomi` **不会启用**下列开关。R11 扫码/记录和 R09 journal、R10 事件订阅不需要额外开关。

| Hub 原始参数 | 含义与边界 |
| --- | --- |
| `--remote-ice-restart` | 有界 ICE 恢复；需升级 Relay/iOS，真实切网待验收 |
| `--remote-signal-resume` | WSS 续通及受限备用控制，隐含 ICE 恢复；不是进程重启后续接原通话 |
| `--remote-turn-renew` | 到期前刷新 TURN 并 ICE restart，隐含信令恢复；新旧通路并行迁移未完成，不能保证无缝 |
| `--remote-audio-recovery` | 已接通电话的短暂 CallKit 音频打断恢复，隐含信令恢复；AudioHost 进程断开恢复尚未实现 |
| `--remote-call-controls` | 协商式 DTMF 键盘与 Hub 上行静音同步；正常 CallKit 本机静音不依赖它 |

启动器不透传任意参数，**不能**写 `djonehub start --remote-profile home-xiaomi --remote-call-controls`。只在三端已升级、准备专项测试时，先停止启动器，再用安装包原始程序手动启动。

例如只验证 DTMF，终端 A：

```sh
/usr/local/libexec/djonehub/bin/djonehub-macos \
  --listen 127.0.0.1:7575 \
  --remote-online-profile home-xiaomi \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json" \
  --remote-call-controls
```

终端 B：

```sh
/usr/local/libexec/djonehub/bin/djonehub-audio-host \
  --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

需要测试其他开关时在 A 的 Hub 命令末尾加入对应参数。两端必须同一 Mac 用户、同一 bootstrap 绝对路径。结束测试后两个终端分别 Ctrl+C，再恢复第 2 节普通启动。手动进程不由 `djonehub stop/status` 管理。

## 10. 全新 Mac 首次登记（已有 home-xiaomi 则跳过）

1. Mac 先用普通 `djonehub start` 发现模块，在网页或 `/api/devices` 记录真实模块 ID，完成模块语音准备；然后停止。不要把显示别名作为授权 ID。
2. Relay 服务器执行 provision，为真实模块签发 10 分钟有效的 enrollment 文件，通过私有 SSH 传输到 Mac。服务器命令和文件属主处理见 [首次登记/模块授权修复](REMOTE_XIAOMI_PAIRING_RECOVERY.md#1-centos-生成正确范围的新注册文件)。该文模块为本线程 Xiaomi，新模块须替换。
3. 在 Mac 用**尚未存在的新 profile 名**登记；下面示例仅适用于 home-xiaomi 不存在时，文件必须实际存在、属于当前用户且权限 0600：

```sh
djonehub remote enroll --profile home-xiaomi \
  --relay https://oops.work \
  --enrollment-file "$HOME/Library/Application Support/DJOneHub/private/enrollment-xiaomi.json"
djonehub remote status --profile home-xiaomi
```

4. ready 后保留 Keychain 身份，移除已消费 enrollment 副本，按第 2 节启动，再做第 5、6 节手机配对和路由设置。不要将另一台 Mac 的 bootstrap 或 Keychain token 当 enrollment 使用。

## 11. 更新后验收与回退

先验证一次前台来电，双方轮流说话各 30 秒，再分别测试手机挂断和对方挂断，确认第二通也能接听。之后手机退到后台、锁屏，再测接听、拒接和双向声音；不要强退 App 代替正常后台测试。

新手机通话记录在 App 的“通话记录（仅保存在此 iPhone）”中显示，最多 100 条，不含录音。它只记录手机实际收到的有效推送/信令和手机发起的通话，不会同步完整服务端账本。“App 退出，结束状态未确认”只表示本机记录未完成，不意味着允许恢复那通电话。

| 现象 | 处理 |
| --- | --- |
| `remote-ui` 不存在 | 核对 `command -v djonehub`，是否还在使用旧安装或旧解压目录 |
| 配对窗口读不到配置 | 默认 profile 改为 home-xiaomi；处理 Keychain 提示；用 remote status 核对 |
| 扫码按钮灰色 | 已有配对、正在操作/在线或有通话；升级不需要重新配对 |
| 手机没有扫码/通话记录 | iOS App 仍是旧版；Mac ZIP 不会更新手机 |
| 手机配对成功但不响 | 核对目标安装 ID、模块授权、AI 开关、Hub online 与后台注册状态 |
| 保存路由返回 409 | 先结束通话；或模块离线/ID 不匹配，按具体错误处理 |
| Hub 启动卡住/超时 | 查看 macOS Keychain 提示及日志，不删除锁文件或重复开进程 |
| App 报模块挂断未确认/网页恢复隔离 | 在本机核对真实模块通话并处理，等待成功 CLCC 空闲快照；不要删除 journal 解锁 |
| 不出现 DTMF 数字键 | 普通启动未协商 call_controls；按第 9 节专项部署，不能只升级 UI |

回退前结束通话并停止 Hub/AudioHost。优先关闭实验开关回到普通启动；需要退旧 Mac 程序时，从保留的旧完整包重新安装并处理 Keychain 授权。保留当前身份与设置，不把旧 Keychain/refresh token 或 bootstrap 恢复回来。Relay 数据库回退另按升级脚本的私有备份恢复流程执行；不要单独回退数据库，避免令牌轮换状态倒退。

本次只更新源码仓库中的指南与入口说明，未重打或改写已交付 ZIP，包的校验和不变；随包旧 README 与本文不一致时，以本文为准。

本文是操作说明，不是本次部署报告：未替换运行进程、安装手机或修改公网服务。R06 并行迁移、R07 宿主恢复和完整真机/弱网验收仍未完成，最新状态见 [总体计划](REMOTE_PLAN_STATUS_2026-10-10.md)。
