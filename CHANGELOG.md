# 变更记录

## 2026-10-09

- iPhone App 增加通话面板：接听、拒接、红色挂断按钮、静音、扬声器和通话计时；结束后清除本次状态并继续等待来电。
- 增加配对模块主动拨号与取消流程，沿用 CallKit 和 Opus 媒体链路；Relay 校验身份、模块范围和当前连接，Hub 校验模块政策并对未知 ATD 结果执行绑定清理。旧 Relay 禁用新拨号入口，来电控制继续可用。
- 正常蜂窝挂断先发送明确的结束状态，减少将普通挂断显示为信令故障的情况；补充已有部署的保留配对升级手册。

- 远程 iPhone 安装手册改为这台 CentOS Linux 8 Relay 服务器：保留现有 TCP 80/443 网站，复用 `https://oops.work` 证书，Relay 监听 `https://oops.work:8443`，TURN 使用同一域名。
- CentOS 8 软件源说明改为只处理 AppStream 403 和重复仓库；不再安装 certbot，也不再为申请证书放行 80 端口。
- README 与 `deploy/remote-call/README.md` 同步上述部署目标。
