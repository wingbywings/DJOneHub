# DJOneHub 电话拨打与接听实施计划

## 实施状态（2026-08-12）

| 项目 | 状态 | 证据或下一步 |
| --- | --- | --- |
| 拨号 | 已实现、已真机验证 | 用户已在大疆模块上验证可以拨出电话 |
| 挂断 | 已实现、已真机验证 | 用户已在大疆模块上验证可以挂断电话 |
| 来电识别、接听与拒接 | 已实现、待完整真机回归 | 使用 `AT+CLCC`、`ATA` 和 `ATH`；需按第 8 节覆盖真实来电场景 |
| UAC 只读探测 | 已实现、已真机验证 | QDC507 固件 `01.001.02.004` 的 USB UAC 位为 1，但 `QPCMV?` 返回 `ERROR` |
| UAC 显式启用 | 已实现、当前真机固件不兼容 | `QPCMV=?` 声明 mode 0–2，但读写命令均返回 `ERROR`；应用自动停止重试 |
| 本机双向音频桥 | 已实现实验版、当前真机不可用 | 代码已提供本机桥；需要能够成功进入 QPCMV mode 2 的模块/固件 |

当前发布口径：电话信令可以使用，其中拨号和挂断已有真机结果；双向通话音频仍是实验功能，在 UAC 验收通过前不得标记为稳定可用。

## 1. 目标与边界

在不引入 Asterisk、Docker 或完整 IMS/VoWiFi 引擎的前提下，利用大疆第一代 4G 模块的蜂窝语音能力，并在模块固件支持时使用 USB Audio Class（UAC），为 DJOneHub 增加：

- 电话号码拨号；
- 来电接听、拒接和通话挂断；
- 来电、拨号、振铃、接通、保持和结束状态；
- 最近通话记录和未接来电 Bark 通知；
- 真机验证通过后的本机双向语音。

第一阶段已交付电话信令，第二阶段的 UAC 探测、显式启用和本机音频桥已进入真机验证。应用不会自动重启模块、刷写固件或假定 UAC 一定可用。完整语音必须先通过第 4 节的硬件验收门槛。

## 2. 现状

DJOneHub 已具备以下基础：

- `AT+CLIP=1` 和 `AT+CLCC` 来电、去电状态轮询；
- `incoming`、`waiting`、`dialing`、`alerting`、`active`、`held` 状态解析；
- 最近 100 条运行期通话记录和未接来电 Bark 通知；
- 共享 Modem Manager 中的 `ATA`、`ATD<number>;`、`ATH`；
- UAC USB 组合检测以及 `AT+QPCMV=1,2` 支持代码；
- macOS USB AT 与串口 AT 两种控制路径。

电话控制 API、Web 拨号/接听/挂断控件和浏览器本地音频桥已经落地。实测硬件为 Baiwang QDC507，并非直接自报 EG25-G；当前固件禁用了 QPCMV 运行控制，因此该硬件只能发布电话信令能力。剩余工作是寻找明确支持 UAC 的固件/硬件，以及真实来电接听回归。

## 3. 推荐架构

```text
Web 电话页面
├── REST：拨号、接听、挂断、状态查询
└── 后续音频：Mac 麦克风/扬声器 <-> EG25 UAC

Go CallService
├── 号码和状态校验
├── 每模块电话操作串行化
├── ATA / ATD / ATH
├── CLCC 状态收敛
└── 通话记录与错误映射

DJI / Baiwang QDC507（或兼容模块）
├── 运营商蜂窝语音信令
└── USB Audio Class 双向 PCM（待真机确认）
```

电话业务继续放在 `cmd/djonehub-macos/calls.go`，HTTP handler 只负责解析请求和映射响应。底层统一通过 `app.runATCommand`，保证 USB AT 和串口 AT 都可使用。

## 4. 里程碑 0：硬件能力验收

### 4.1 只读探测

连接真实模块后记录：

```text
ATI
AT+CLCC
AT+QPCMV=?
AT+QPCMV?
AT+QCFG="USBCFG"
```

使用测试号码验证 `ATD<number>;`、`ATA` 和 `ATH`，并确认运营商侧 VoLTE、主叫、被叫均正常。

### 4.2 UAC 验收

只有用户明确确认后才允许修改 USB 组合。修改前保存完整 `USBCFG` 原值；若配置变化需要重启模块，页面必须提示 USB 会重新枚举。

重启后验收：

- macOS“音频 MIDI 设置”或 `system_profiler SPAudioDataType` 能看到模块输入和输出；
- 模块下行能够被 Mac 持续采集；
- Mac 上行能够送入通话对端；
- 连续通话 30 分钟无静音、爆音或严重漂移；
- 模块拔插、重启后音频设备能够重新匹配。

通过条件：信令和双向 PCM 都稳定。若只通过信令，产品保留“无音频电话控制”模式，不把 UAC 标记为可用。

### 4.3 固件兼容性判定

Quectel《LTE Standard UAC Application Note V1.0》规定，`USBCFG` 在命令名、VID、PID 后必须包含 7 个 USB 功能参数，第 7 个才是 UAC 开关。如果模块只返回 6 个功能参数，表示当前固件不支持通过 AT 启用 UAC 声卡；程序必须停止写入，不能把最后一个已有功能参数误判为 UAC。

若 7 个参数齐全但写入仍返回 `ERROR`，记录为 `uac_usb_config_rejected`，通常需要向 DJI/Quectel 获取与该硬件基线匹配的 UAC 固件。若 USB 组合已经启用但 `AT+QPCMV=1,2` 返回错误，记录为 `uac_runtime_enable_rejected`。DJOneHub 不自动刷写固件，也不绕过模块的固件能力检查。

本次 QDC507 真机结果：

```text
ATI       -> Baiwang / QDC507 / QDC507GLEFM21
AT+QGMR   -> QDC507GLEFM21_01.001.02.004
QPCMV=?   -> +QPCMV: (0,1),(0-2) / OK
QPCMV?    -> ERROR
QPCMV=1,2 -> ERROR
QPCMV=1,0 -> ERROR（2026-08-13 单次真机探测，模块随后仍正常响应 AT）
USBCFG    -> 0x2CA3,0x4006,...,1（7 个功能参数，UAC 位为 1）
```

这表明该固件仅在命令表中保留了 QPCMV 声明，并未开放运行状态读写。UAC mode 2 和 USB NMEA 串口 PCM mode 0 都不可用。DJOneHub 将其标记为 `uac_runtime_control_unavailable`，保留拨号/接听/挂断信令，不再显示 UAC 启用操作。更换 VID/PID 不会改变固件内部命令实现，不能作为音频修复方案。

Quectel 技术支持已公开说明 `QDC507GLEFM21` 并非 Quectel 开发的固件，应联系设备供应商获取匹配固件。禁止把标准 EG25 固件直接刷入 QDC507；已有公开案例因此进入 9008/循环重启状态。

## 5. 里程碑 1：电话信令 MVP

状态：已实现；拨号和挂断已完成真机验证，接听与异常场景继续回归。

### 5.1 API

| 方法 | 路径 | 用途 | 成功状态 |
| --- | --- | --- | --- |
| `GET` | `/api/calls/status` | 当前通话、历史和能力 | `200` |
| `POST` | `/api/calls/dial` | 创建外呼，body 为 `{ "number": "10086" }` | `202` |
| `POST` | `/api/calls/answer` | 接听 `incoming/waiting` 通话 | `202` |
| `POST` | `/api/calls/hangup` | 拒接或挂断；空闲时幂等成功 | `200` |

错误响应保持现有 `{ "error": "...", "code": "..." }` 约定：

- `422 invalid_number`：号码格式非法；
- `409 call_state_conflict`：当前状态不允许该操作；
- `503 call_unavailable`：模块或 AT 通道不可用；
- `502 modem_rejected`：模块返回 `ERROR`、`CME ERROR` 或 `CMS ERROR`。

### 5.2 业务规则

- 号码只允许一个可选前导 `+` 和 1～32 位数字；
- 只有空闲状态可以拨号；
- 只有 `incoming` 或 `waiting` 可以接听；
- 空闲状态挂断返回成功且不发送 `ATH`；
- AT 命令执行和电话状态变更按设备串行化；
- 拨号成功后先建立乐观 `dialing` 状态，随后由 `AT+CLCC` 收敛；
- 挂断成功后立即结束当前记录；
- 通话存在时加快 `CLCC` 轮询，空闲时保持低频轮询。

### 5.3 Web 交互

- 电话页增加号码输入框和拨号按钮；
- 来电时显示“接听”和“拒接”；
- 拨号、振铃、接通和保持时显示“挂断”；
- 操作期间禁用相关按钮，避免重复提交；
- 明确标记“当前版本仅控制信令，UAC 验证完成前没有通话音频”；
- API 错误映射为可读提示，不展示内部堆栈或原始设备信息。

## 6. 里程碑 2：本机音频

状态：实验版已实现，等待连接真实模块完成本节与第 4.2 节验收。

首选本地浏览器音频桥：

```text
Mac 麦克风 -> EG25 UAC 输出
EG25 UAC 输入 -> Mac 耳机/扬声器
```

第一版优先 Chrome/Edge，提供设备选择、静音、输出音量和音频诊断。使用耳机作为默认建议，降低回声和啸叫风险。USB 重枚举后不能只依赖浏览器临时 `deviceId`，需要结合设备名称、groupId 和 macOS USB location 重新匹配。

本次实现提供以下接口和保护：

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| `GET` | `/api/calls/audio` | 只读查询 USB UAC 配置、QPCMV 支持和运行模式 |
| `POST` | `/api/calls/audio/enable` | 当 USB 组合已包含 UAC 时开启本次运行模式 |
| `POST` | `/api/calls/audio/enable`，body 含 `{ "allow_usb_reconfigure": true }` | 经页面二次确认后写入 UAC USB 组合；只返回需要重启，不自动重启 |

浏览器音频桥仅在存在活动通话、已枚举输入/输出设备且用户主动点击后启动；通话结束、启动失败或页面关闭时释放媒体轨道。第一版不把音频上传到 DJOneHub 服务端。

如果后续需要手机或局域网远程通话，再增加独立的 WebRTC/CoreAudio 媒体网关；该阶段需要 Opus/PCM 转换、重采样、抖动缓冲、WSS、认证和回声控制，不纳入信令 MVP。

## 7. 多模块兼容

电话状态、操作锁和音频设备映射必须属于单个设备 Runtime。`feat-multi-hub` 合并后，现有 `/api/calls/...` handler 由设备作用域路由复用，并对应到：

```text
/api/devices/{deviceID}/calls/...
```

多台同型号模块可能暴露相同声卡名称，音频阶段必须建立 USB physical location 到 CoreAudio 设备的映射，不能按名称选择第一台设备。

## 8. 测试与验收

### 自动化测试

- 号码合法、非法和边界长度；
- 拨号成功、模块拒绝、AT 通道失败；
- 非空闲重复拨号冲突；
- 来电接听成功和错误状态冲突；
- 挂断成功和空闲幂等；
- API 状态码、错误码和 JSON 结构；
- 来电结束后未接记录与 Bark 行为不回归；
- 前端按钮随状态正确显示和禁用。

### 真机回归

- 主叫、被叫、对端拒接、无人接听、主动挂断；
- 通话中拔出模块、模块重启、SIM 失去注册；
- 电话轮询与短信读取、eSIM 操作不串 AT 响应；
- 多设备同时来电和单设备掉线隔离；
- 不同运营商、SIM/eSIM Profile 和固件版本。

## 9. 发布门槛

- `go test ./...` 通过；
- Apple Silicon macOS 构建通过；
- Demo 模式可演示拨号和挂断状态；
- 真机信令用例全部通过后才标记“拨打/接听已实现”；
- UAC 双向验收通过后才移除“无音频”提示；
- 默认仍只监听 `127.0.0.1`，若未来开放局域网访问，必须先增加认证、CSRF 和 HTTPS/WSS。

## 10. 许可证边界

MDD Sim Gateway 的状态机、错误处理和软电话交互可作为设计参考，但不直接复制其 GPL-3.0-only 源码。DJOneHub 的实现保持独立编写，并继续遵守仓库现有 PolyForm Noncommercial 和第三方依赖声明。
