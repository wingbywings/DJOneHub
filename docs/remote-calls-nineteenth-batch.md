# 第十九批：R09 通话 journal 与启动核对

日期：2026-10-12。分支 `codex/remote-iphone-callkit`，未提交；未更新真实服务。

## 实现

每个模块在 `remote-calls.json` 同目录保存 `remote-call-journal.json`，仅保存一个有界记录：UUID、generation、阶段和时间。阶段为 offered、answer_pending、active、ending、ended；无法归属的启动通话记为 unverified。不保存号码、安装凭据、推送 token、SDP 或音频。

发出邀请前记录 offered；提交 ATA/ATD 前先持久化 answer_pending；观察到蜂窝接通后记录 active；清理前记录 ending。关键前置写入失败则不继续远程接听/拨号。结束清理不因日志失败而跳过通话绑定的硬件收尾。临时文件 0600，写入后 fsync，再原子替换并同步目录；稳定阶段不会每次轮询重复写盘。

重启读取未结束记录、损坏/超大/未知版本文件或读取失败时进入待本机处理。新进程第一次成功 CLCC 若直接观察到 active/held/拨号中的通话，即使没有未结束记录，也不推断它属于某台手机。不会复用旧 UUID、宿主凭据、媒体连接，或自动 ATA/ATH/重发邀请。

待核对的模块不能远程拨号或自动恢复接听；当前通话选本机处理路由，停用自动媒体、AI 接听与录音。Mac 通话页显示待处理提示，保留明确的本机挂断/拒接动作；执行时仍走当前通话绑定和 fresh CLCC 校验。不能仅凭模块 index 将重启前后两通电话视为同一通。

只有完整成功的 CLCC 空闲快照才能持久化 ended 并解除隔离。AT ERROR、截断/格式错误的 CLCC、USB 断开和程序内部的清理通知都不能当成空闲；非确认断开保留 journal 并要求后续核对。文件损坏也只能在模块确认为空闲后修复。状态接口给出 recovery_required、persistence_failed 和阶段。

## 验证

- 各关键阶段写入后重开文件，均识别为未结束；确认空闲后重开不再阻断。
- 文件权限、原子替换临时文件清理、最小字段、旧绑定拒绝。
- 损坏/未知版本/超大文件、写入失败均阻断自动操作；硬件准备前必须看到持久化 answer_pending。
- 重启后的未知 active 不自动 AI/Remote 接听；合成断开不解除隔离，远程外呼被拒；真实空闲核对可持久恢复。
- 没有 journal 的启动 active 也隔离；正常空闲启动后出现的新通话不受旧恢复状态影响。
- 不完整/错误 CLCC 不能冒充空闲。
- Go 全仓库 race 回归通过；最终启动与非确认断开补充改动另跑 Hub race 回归。

日志：`/tmp/djonehub-r09-journal.log`、`/tmp/djonehub-r09-full-regression.log`、`/tmp/djonehub-r09-hub-final.log`。

尚未做真实通话中强制结束 Hub、磁盘故障或重启的故障注入。当前模块协议没有足以证明跨进程通话连续性的标识，因此本批实现保守核对/隔离与本机处理，不承诺崩溃后自动续上音频。
