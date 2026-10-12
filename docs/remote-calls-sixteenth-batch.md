# 第十六批：信令自动恢复、备用控制通道与 TURN 续租

日期：2026-10-11。分支：`codex/remote-iphone-callkit`。本记录对应工作区改动；运行中的 Hub、AudioHost、iPhone 和公网 Relay 尚未更新。本批开关默认关闭，不应将代码验证视为已上线验收。

## R05：产品链路接入

Hub 和 iOS 已接入第十五批的鉴权续通协议。仅已接通、双方支持恢复的同一 UUID/generation 可以恢复。Hub 连接管理器把活动通话的连接生命周期交给会话；iOS 保留 CallKit、PhoneCall 和 PhoneMedia。Wi-Fi/蜂窝路径变化、WSS 读写或心跳失败进入固定 10 秒恢复预算。重复事件、重试及 DataChannel 心跳均不能延长该预算。

重连重新取得访问凭据，校验恢复 welcome 和原 epoch。iOS 用有 request_id 的 sync 核对不倒退的 revision/终态，再退出恢复状态。未确认的挂断保留原 request_id；重复 ICE offer 返回同一已缓存或正在生成的 answer，不重复协商或操作模块。恢复成功的新 WSS 生命周期归属于通话，不再继承已耗尽的重试超时。

`call-control` 是原 PeerConnection 上有序、可靠的 SCTP DataChannel，协议名为 `djonehub.calls.v1`。它仅允许本通话的 sync/hangup，以及 Hub 的 state/result；不承载接听、拨号、SDP 或鉴权更新。两条通道使用同一个绑定校验和 Session.Command 幂等处理。帧大小限制 8 KiB，收发队列和速率有界。

挂断先停止双向音频并释放 AudioHost，再等待模块收尾。完成前的状态查询不返回 ended；失败返回 cleanup_failed。收尾后通过可用 WSS/DC 投递终态，再最多等待 300 ms 关闭媒体传输。这是有界的尽力投递，尚无应用层终态收讫 ACK，不能保证断网时手机收到最后一个状态。父上下文取消时，DC 保留另有 15 秒硬上限，不保留音频或旧宿主租约。

启用方式：三端更新后，Hub 使用 `--remote-signal-resume`，它同时启用 `--remote-ice-restart`。不支持恢复的旧 Relay 不会协商该能力。此开关仍需更新后的 iOS 和 Relay 配套使用。

### 已通过的 R05 验证

- 真实 TLS Relay：Hub、手机、双方断开，暂时失败后重试，取消和固定期限超时；ATA/媒体激活不重复。
- 成功续通后继续超过原 10 秒期限，再查询状态与挂断，连接没有误继承旧 deadline。
- DC 挂断、重复请求和并发 sync：音频先停，硬件清理阻塞期间不提前报告 ended；清理失败正确报告；最终传输关闭。
- 协议拒绝 DC 上的 answer/reject/dial、反向消息、额外字段。
- 原生 iOS/WebRTC 154 与 Pion：两次 ICE restart 后继续接收实际 Opus；双向 DC 通信成功；音频关闭后仍收到终态；七项原生测试通过，未跳过互通测试。
- Swift PhoneCore 26 项通过；全仓库 Go 竞态测试通过。终态 sync 修正后，会话包再次通过。

日志：`/tmp/djonehub-r05-control-native-{ios,go}.log`、`/tmp/djonehub-r05-full-regression.log`、`/tmp/djonehub-r05-final-session.log`。

## R06：已接入的续租机制及尚存差距

新增默认关闭的 `--remote-turn-renew`，同时启用信令和 ICE 恢复。在凭据到期前两分钟由 Hub 发起更新：双方分别向已配对 Relay 的 `/v1/ice` 取得自身授权的 TURN 凭据，更新现有 PeerConnection 配置，再通过有序 ICE restart 创建新 allocation。凭据不经信令或 DC 转发。

Hub 获取凭据最多三秒，HTTP 暂时失败且旧媒体仍正常时，间隔至少五秒重试。旧凭据到期前 30 秒仍未确认新连接就结束通话。ICE 已开始修改后，沿用固定十秒恢复预算，不重复发起重叠协商。拒绝禁用 TURN、改变 relay-only 策略、倒退有效期、复用旧 TURN 用户名；到期续租还要求延长有效期。同秒签发的不同用户名可用于尚未临期的网络恢复。

Hub 在 answer 被接受且新传输连接成功后提交新有效期；iOS 则通过统计确认已选 candidate pair 与更新前不同且已经 succeeded/connected 后才提交；仅凭旧连接的 connected 状态不足以确认续租。重复 offer 复用同一任务/answer，不再次获取凭据。iOS 最多等待五秒候选收集；当前 SDP 出现符合原 relay-only 策略的候选后，再收集 300 ms 即发送有界快照，避免因等待收集结束增加数秒静音。迟到的 trickle 候选仍不进入新的协商；没有可用候选仍失败。WSS 正在恢复时不启动新的续租协商，但旧截止时间仍有效。

更新 ICE 配置后需要重新收集候选，采用 ICE restart 的依据见 [W3C WebRTC 配置流程](https://www.w3.org/TR/webrtc/#set-the-configuration)。同时核对了当前 Pion v4.2.22 的 SetConfiguration 和 ICE restart 实现。

**还不能关闭完整 R06**：当前 Pion ICE agent 的 Restart 会删除旧候选和已选 candidate pair，因此这条实现是有界重新连接，可能产生短暂音频间断；没有实现主计划要求的“预建新通路、成功后再释放旧通路”。不能宣称无缝切换。该目标需要另行引入通路并行迁移及 PCM/序号去重，不能仅通过更新凭据实现。真实公网 TURN 到期边界、长通话及切换音频间断仍需验收。

### R06 验证记录

- Go 时钟可控测试覆盖到期前两分钟启动、HTTP 失败重试、不提前延长有效期、到期前 30 秒终止（包括尚未接听的准备阶段）、拒绝降级/复用凭据。
- 本地真实 UDP TURN + 原生 iOS/WebRTC 154 + Pion 强制 relay-only：两次独立轮换，TURN 记录初始及双方四组新凭据通过鉴权；最终两端候选均为 relay，Opus RTP 在每次轮换后继续增长，DC 双向和停止音频后的终态仍通过。
- 本机模拟器与 Pion fixture 验证不等于公网 coturn、TURN/TLS 或真实电话到期边界验收。测试使用合成音频与临时测试凭据，没有使用生产 TURN/APNs 密钥。
- 初次测试暴露模拟器启动挤占测试凭据窗口，以及 iOS 等待候选收集结束造成恢复失败；已分别通过独立时钟测试/扩大 fixture 启动窗口和有界候选快照修正，原生回归重新通过。

日志：`/tmp/djonehub-r06-native-{ios,go}.log`、`/tmp/djonehub-r06-full-regression.log`、`/tmp/djonehub-r06-final-go.log`。最终原生七项全部通过，互通测试没有跳过；缩短候选收集等待后的两次续租及音频/DC 核对亦通过。

## 后续顺序

R04/R05 三端升级与真实切网验收；R06 通路并行迁移/到期边界验证；然后 R07 系统音频/宿主暂停恢复、R08 DTMF/Hub 静音、R09 journal、R10 AudioHost 事件订阅、R11 配对管理/扫码/手机历史。未实现的项目保持未完成，不因本批测试而关闭。
