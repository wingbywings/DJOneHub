# iPhone 拨打、接听与挂断：更新和验收

> 2026-10-12：当前升级、重启与扫码配对统一使用[操作指南](REMOTE_OPERATIONS_GUIDE.md)。下文部署命令和运行进程描述保留为早期批次记录；本批升级需要同时更新 Hub、AudioHost 与 iOS App。

2026-10-10 更新：用户已确认 APNs/后台来电完成；随后出现的 iPhone 单向无声已通过
修复服务端音频时钟累积漂移恢复，真机复测确认“已恢复”。修复版 Hub 已在本机运行，
AudioHost 已重新注册，详见[音频诊断与验收记录](REMOTE_AUDIO_DIAGNOSTICS.md)。下文保留此前批次的部署过程。

当前分支：`codex/remote-iphone-callkit`。2026-10-09 用户已确认更新 UAC 回调周期后的 CallKit 接听及双向音频测试通过。本次补齐 App 通话控制和主动呼出；呼出尚需更新公网 Relay 后进行真机验收。

新版 App 已覆盖安装并启动到 Wings Black，保留 `com.oops.djonehub` 的原配对身份。用户随后确认 App 红色挂断按钮正常、双方结束且恢复“在线，等待来电”。Hub 同一通 `e283c29e-bafe-472f-aebb-bced97026827` 记录为 `ended / remote_hangup`、`cleanup_failed=false`；模块 active 为空，AudioHost running=false。当前 Mac 仍运行 20:13 启动的旧 Go 进程，需按下述步骤重启以加载新呼出和结束状态实现。

## 使用流程

1. 保持 Mac 的 Go 后端与 Swift AudioHost 各运行一个进程。iPhone 打开 DJOneHub，连接后显示“在线，等待来电”。
2. 模块收到来电时出现系统 CallKit 来电界面。可以在系统界面接听或拒接；退回 App 后，“当前通话”面板同样提供接听和拒接。
3. 接听期间显示准备状态，待模块及系统音频就绪后显示“通话中”和计时。App 有明显的红色“挂断”按钮，以及静音、扬声器/听筒切换。静音通过 CallKit 操作，系统与 App 的静音状态保持一致；蓝牙路由变化会刷新扬声器状态。
4. 空闲时在“拨打电话”选择配对授权范围内的模块，输入电话号码并点击“拨打”。支持数字和开头的 `+`；可输入空格、括号、短横线作为显示格式。拨号不能包含 AT 命令、分号、换行、USSD 或特殊拨号序列。
5. 呼出先建立媒体连接，再准备模块并提交一次 ATD。对方接听之前可以“取消拨号”，接通后按钮变为“挂断”。呼出等待对方接听的预算为 60 秒；模块及媒体准备也占用这段预算。
6. 本机挂断、对方挂断、拒接或未接听后，清除本次音频及操作状态，回到等待来电；后续来电使用新 UUID。连接本身故障时显示断开，可点击“连接并等待来电”重新连接。

如果手机通话已结束，但模块的 CLCC/ATH 清理未确认，App 会提示“模块挂断未确认，请检查模块通话状态”；Hub 保留该模块的音频占用，直到实际通话结束，避免把新电话误认为原通话或误报挂断成功。

后台 APNs/PushKit 已接入，启用前需要完成[推送配置与后台验收](remote-calls-thirteenth-batch.md)；未配置时，空闲等待仍需 App 在前台。只有“通话中”才开始计时；系统提前激活呼出音频不会提前打开 RTP 音频门控。

## 更新已有部署，保留配对

仅使用 App 内来电接听、挂断和静音时，新 App 可以连接现有 Relay。主动拨号需要本次新版 Go Hub 和 Relay；App 会读取 Relay 的能力标记，旧 Relay 上拨号按钮保持禁用，接听继续可用。不需要重新配对、撤销身份、重建数据库或更换 TURN secret。

### Mac Hub

本机已构建的新二进制在 `dist/djonehub-macos`。在没有通话时，在原 Go 终端按 Ctrl+C 后重新执行：

```sh
./dist/djonehub-macos --listen 127.0.0.1:7575 \
  --remote-online-profile home-xiaomi \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

已运行 UAC 周期修复版的 AudioHost 不需要因本次改动再次重建或重启。若尚未启动，在另一个终端运行：

```sh
./macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost \
  --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

控制文件被占用代表仍有一个 Go 后端运行，不要删除控制文件来绕过锁，也不要同时再开第二个后端。

### Relay（当前 CentOS 8 / Nginx 443 部署）

源码更新包为本机 `dist/djonehub-relay-call-flow-source.tar.gz`，仅包含 Relay 所需源码与 Go 本地依赖，不包含配对文件或凭据。把它传到服务器，解压到一个新的目录。例如服务器当前目录已有该包时：

可先在 Mac 执行（按实际 SSH 用户及地址调整）：

```sh
scp dist/djonehub-relay-call-flow-source.tar.gz root@47.108.184.34:/root/
```

随后在服务器执行：

```sh
mkdir -p /root/djonehub-call-flow
tar -xzf djonehub-relay-call-flow-source.tar.gz -C /root/djonehub-call-flow
cd /root/djonehub-call-flow

# 原安装器安装的 Go；若不存在，使用 PATH 中的 Go 1.26.3+。
relay_go=/opt/djonehub/go/bin/go
if [ ! -x "$relay_go" ]; then relay_go=$(command -v go); fi
"$relay_go" version
CGO_ENABLED=1 "$relay_go" build -trimpath -o /tmp/djonehub-relay-call-flow ./cmd/djonehub-relay
```

构建成功后，在无通话时仅更新 Relay 可执行文件并重启 Relay。以下路径沿用当前安装器的 `/usr/local/bin/djonehub-relay`；若手工部署使用其他路径，先查看 `systemctl cat djonehub-relay`，按实际 ExecStart 路径替换。

```sh
sudo cp -p /usr/local/bin/djonehub-relay /usr/local/bin/djonehub-relay.before-call-flow
sudo install -m 755 /tmp/djonehub-relay-call-flow /usr/local/bin/djonehub-relay.new
sudo mv /usr/local/bin/djonehub-relay.new /usr/local/bin/djonehub-relay
sudo systemctl restart djonehub-relay
sudo systemctl status djonehub-relay --no-pager -l
curl -fsS http://127.0.0.1:18443/healthz
curl -fsS https://oops.work/healthz
```

两次 healthz 均应显示 `capabilities.outgoing_calls: true`。现有 `/var/lib/djonehub-relay`、`/etc/djonehub`、Nginx、证书与 coturn 配置保持原样；无需运行初次安装器或重新 provision。更新 Relay 会断开旧 WSS，Mac Hub 会自动重连；iPhone 点击重新连接，能力检查通过后启用拨号。

若需要回退，使用新的 inode 原子替换，保留当前数据库及配置：

```sh
sudo install -m 755 /usr/local/bin/djonehub-relay.before-call-flow /usr/local/bin/djonehub-relay.rollback
sudo mv /usr/local/bin/djonehub-relay.rollback /usr/local/bin/djonehub-relay
sudo systemctl restart djonehub-relay
```

### iPhone

沿用原 Team、Bundle ID 和 Keychain 身份覆盖安装更新，不卸载 App。Xcode 项目为 `ios/DJOneHubPhone/DJOneHubPhone.xcodeproj`，Scheme 为 `DJOneHubPhone`。新版打开后仍使用现有配对，点击“连接并等待来电”。

## 验收清单

| 场景 | 应看到的结果 |
| --- | --- |
| 来电接听后返回 App | 红色挂断按钮可见，双向音频正常，计时继续 |
| App 点击挂断 | 主叫结束、音频停止，App 回到等待来电 |
| CallKit 系统界面挂断 | App 同步结束并回到等待来电 |
| 来电在 App 拒接 | 主叫结束，不执行 ATA、不播放通话音频 |
| 对方先挂断 | App 显示对方已挂断，系统 CallKit 通话结束 |
| 静音 / 取消静音 | 对方听不到 / 恢复听到 iPhone 声音，本机接收音频继续 |
| 扬声器 / 听筒 / 蓝牙 | 音频路由可切换，按钮反映当前路由 |
| 呼出正常接听 | 对方接听后通话计时开始，双向音频正常 |
| 呼出准备阶段取消 | 不提交 ATD，不残留模块通话或音频占用 |
| 呼出振铃阶段取消 | 结束该 UUID 的蜂窝呼出，不重新拨号 |
| 对方拒接或不接 | 结束本次呼出，可再次拨号或接听新来电 |
| 模块忙线、离线、AI 启用或配对不匹配 | 拒绝呼出，不改变其他通话 |
| 连续两次来电或呼出 | 前次静音、计时、音频、按钮状态均不带到新通话 |

本次自动验证覆盖 PhoneCore 呼出/挂断状态竞态、实际 TLS Relay → Hub 管理器 → Opus offer → 拨号前取消、未知 ATD 结果的 CLCC/ATH 清理、音频门控和相关包竞态检查。自动测试不实际拨打付费电话，也不代替升级后的公网呼出真机验收。
