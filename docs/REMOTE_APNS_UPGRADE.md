# 现有服务器升级 APNs（保留配对）

本次由用户自行执行服务器升级；Codex 不通过 SSH 登录或上传私钥。目标为现有 `root@47.108.184.34`、`https://oops.work`、Nginx 转发 `127.0.0.1:18443` 的部署。

本机已完成：签名 Debug App 包含 `aps-environment=development`，Bundle ID 为 `com.oops.djonehub`，已覆盖安装到 Wings Black；私有 sandbox 配置经过 Relay 自身校验。公网健康查询目前仍是 `voip_push:false`，**不能据此宣称后台电话验收完成**。

2026-10-10 本机补充验证：从最终源码包解压到独立目录可构建 Relay；3 项升级工具测试通过，涵盖服务参数保留、SQLite WAL 一致快照与能力检查。用该产物加载实际私有 APNs 配置的回环启动测试通过，未登录注册返回 401，数据库 schema 2 和正常退出通过；该测试没有向 Apple 发送请求。App 覆盖安装成功，但上次自动启动因 Wings Black 锁屏被系统拒绝，需解锁后手动打开。

## 1. 传输两个文件

源码包不含密钥、配对凭据或数据库。APNs 配置单独传输。在 Mac 项目目录执行：

```sh
scp dist/djonehub-relay-apns-source.tar.gz root@47.108.184.34:/root/
scp dist/apns.sandbox.json root@47.108.184.34:/root/
```

使用自己已验证主机身份的 SSH 连接，不关闭主机密钥校验。校验文件在 `dist/djonehub-relay-apns-source.tar.gz.sha256`，Mac 可用 `shasum -a 256` 对照，服务器用 `sha256sum` 对照。

## 2. 服务器执行

选择没有电话的时间：升级会重启 Relay，断开当前 WSS。不要运行初次安装器，不要删除现有配对数据库或 TURN secret。

```sh
cd /root
chmod 600 apns.sandbox.json
tar -xzf djonehub-relay-apns-source.tar.gz
cd djonehub-relay-apns
sudo bash scripts/upgrade-relay-apns.sh \
  --apns-config /root/apns.sandbox.json
```

脚本识别安装器已有 Go 路径；如需指定，追加 `--go-bin /absolute/path/to/go`，要求 Go 1.26.3+ 和已有 GCC/CGO 环境。构建可能需要访问 Go 模块代理。

脚本顺序为：

1. 检查服务用户、现有 ExecStart 和私有文件，构建新 Relay，用 `check-apns` 验证 JSON 与 P-256 密钥。此时旧服务仍运行。
2. 停止 Relay，确认主 PID 已退出，通过 SQLite checkpoint 将 WAL 合并后保存一致的 0600 快照，校验数据库完整性。支持 CentOS 8 的 Python 3.6。
3. 在 `/var/lib/djonehub-apns-backup.XXXXXX` 保留旧二进制、旧服务文本、旧 APNs override/config（如果有）与数据库快照。该目录是 root 私有目录。
4. 原子替换 Relay 和 `/etc/djonehub/apns.json`，以 `djonehub` 用户再次校验配置；新增 `30-apns.conf` systemd override，原服务其他属性保留。
5. 启动新 Relay，自动迁移 schema 1→2；验证本机健康响应具备 identity、signaling、outgoing_calls、TURN 和 VoIP Push 能力。

如果当前 User、ExecStart 或端口不符合已有安装器布局，脚本会在停止服务前拒绝执行，不猜测或覆盖定制参数。把**不含密钥**的错误信息提供回来即可调整。

## 3. 验证公网与手机

```sh
curl -fsS http://127.0.0.1:18443/healthz
curl -fsS https://oops.work/healthz
systemctl is-active djonehub-relay
```

两处应有 `capabilities.voip_push:true`，原 `outgoing_calls` 和 `turn` 仍为 true。这只证明配置已加载，未验证 Apple 端权限。

Mac 的 `dist/djonehub-macos` 已更新。原 Go 终端在无通话时按 Ctrl+C，重新执行：

```sh
./dist/djonehub-macos --listen 127.0.0.1:7575 \
  --remote-online-profile home-xiaomi \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

AudioHost 无需因 APNs 改动重建；保持原进程运行即可。不要另开第二个 Go 进程或删除控制文件绕过锁。

iPhone 打开 DJOneHub，确认原配对仍在，点击“启用后台来电”。Relay 升级前点击该按钮会注册失败，升级后重试即可；此时不需要重新配对。配置为 sandbox，必须使用本次 Debug 签名版。

先做一次前台正常接听，再退到后台并锁屏，用另一部电话拨入模块：

- 系统 CallKit 出现来电；接听后双向说话、挂断；连续第二次来电仍可接听。
- 再测试未接听、立即拒接、主叫在唤醒前挂断、在 WSS 恢复前迅速接听/拒接。
- AI 启用的模块来电仍只走 AI，不应产生 Push。

实际来电触发才会调用 Apple APNs；本升级脚本不会制造 VoIP 测试通知或拨打电话。

## 4. 失败恢复

脚本在停止服务后的任何错误都会尽力保持 Relay 停止，并打印备份目录。优先修复新版本配置后重新启动，避免回退已经发生的凭据轮换。

- 构建、配置校验或现有布局检查失败：旧服务未停止、数据库未迁移，修正错误即可重试。
- 停服后、数据库快照完成前失败：脚本未替换二进制；确认原配置仍在后可重新启动原服务。
- 替换或迁移后失败：不要直接拿旧二进制启动 schema 2 数据库。先检查 `journalctl -u djonehub-relay`，不公开包含凭据的文件。
- 必须完整回退时：保持服务停止，归档当前数据库及 WAL/SHM，再由备份恢复旧二进制、数据库快照和原 override/config（原来没有的 override 应移除），校正数据库为 `djonehub:djonehub`、0600，再 daemon-reload/start。不要在服务运行时复制或替换数据库。若升级后已有客户端刷新过身份，恢复旧快照会回退该刷新，可能需要按凭据恢复流程重新配对。

详细协议与仍未完成的网络切换、长通话验证见 [第十三批说明](remote-calls-thirteenth-batch.md)。
