# Relay 与 TURN 在线联调

当前为前台在线接听版本，尚无 APNs 后台唤醒。以下是配置模板，不会自动部署服务。

CentOS 8 已有 Nginx/SSL 时，使用 [Nginx 自动安装手册](../../docs/RELAY_CENTOS8_NGINX_QUICKSTART.md) 与 `scripts/install-relay-centos8.sh`，复用 HTTPS 443。

首次安装请按 [完整安装、配对与双向音频联调指南](../../docs/REMOTE_IPHONE_INSTALL_AND_TEST.md) 执行，覆盖 Mac Hub、CentOS 8 上的 Relay/coturn、iPhone 真机签名、配对和分方向验收。当前目标机已有 TCP 80/443 网站，Relay 复用 `https://oops.work` 证书并监听 `https://oops.work:8443`。

1. 使用 `oops.work` 上已经受信任的 TLS 证书。Mac/iPhone 必须信任证书链；客户端不跳过校验。
2. 安装 coturn，复制 `turnserver.conf.example`，填写 realm、证书路径和公网地址。如 TURN 在 NAT 后面，配置正确的公网/内网映射。
3. 生成至少 32 字符的随机共享 secret，例如在服务器本机使用 `openssl rand -hex 32`，写入权限 0600 的 Relay issuer JSON 和 coturn 私有配置。两处使用完全相同的字符串；不要放到终端启动参数、App 配置、配对文件或版本库中。
4. 复制 `turn-issuer.example.json` 并填写真实 URLs 和 secret。空 secret 会被 Relay 拒绝。URLs 必须包含明确的端口及 `?transport=udp` 或 `?transport=tcp`；`turns:` 仅接受 TCP。IPv6 地址使用方括号。配置文件必须为运行 Relay 的用户拥有的私有普通文件，不允许符号链接/多硬链接。
5. 启动 coturn，并在 Relay 的原有 `serve` 参数后加入 `--turn-config /absolute/private/turn-issuer.json`。不传该参数时，已鉴权的 ICE API 明确返回 direct 模式。修改主密钥/配置后需重启进程。
6. 放行 TURN 3478/UDP、3478/TCP、5349/TCP 与配置的 UDP relay 端口范围（模板为 49160–49259）。若要用 443/TCP 对抗仅允许 HTTPS 端口的网络，需要独立 IP/端口规划、TURN TLS 监听和相应 URL；不能把 TURN TLS 当成普通 HTTP 反向代理路径。
7. 在 iPhone 和 Mac 使用更新后的客户端和 Relay。把 issuer 的 `relay_only` 设为 true 并重启 Relay，测试两端使用 relay candidate，再测试 UDP 阻断、TURN/TLS、蜂窝网络、IPv6/NAT64。成功后按部署需求选择是否恢复直连优先。

`POST /v1/ice` 需要 Bearer access token 和 `{ "module_id": "dji-..." }`。它检查当前身份撤销状态和模块范围，返回 no-store 的临时 TURN username/password；每次调用生成独立凭据，客户端仅在本通媒体对象中使用。用户配对身份不能修改 TURN 地址或获得主密钥。

临时凭据有效期 4 小时。当前不在通话中轮换 ICE 身份；两端在其到期前 30 秒结束本通，避免凭据失效后留下无音频电话。新的来电重新获取凭据。该期限不代表已完成 4 小时稳定性验收。

撤销安装身份会禁止新的签发，并关闭其信令/通话；已经签发的 coturn 凭据仍可能被持有者使用到自身到期。coturn 的 user-quota 按临时 username 计算，不是按 DJOneHub 安装身份计算，所以必须同时设置总分配数和带宽上限。Relay 现有全局/来源限流继续适用。需要立即废止全部已签发凭据时，应按运维流程轮换共享 secret 并处理现存分配；这会影响活动通话。

模板拒绝 TURN 对内网等地址的转发，避免将中继当成访问内部网络的代理。需结合实际公网地址规划检查规则。服务日志中的临时用户名也应按凭据相关数据管理，不记录 password 或主密钥。

已通过本机 Pion TURN/UDP、TURN/TCP 的强制中继与 Opus 双向测试；尚未部署 coturn，尚未完成真实 TLS 证书、公网 NAT、NAT64、受限网络和 iPhone 硬件验收。协议参考 [coturn 官方配置](https://github.com/coturn/coturn/blob/master/examples/etc/turnserver.conf)及 [TURN REST 鉴权说明](https://github.com/coturn/coturn/blob/master/README.turnserver)。
