# CentOS 8 + 现有 Nginx/SSL：Relay 自动安装手册

适用日期：2026-10-09。前提：CentOS 8 的 dnf 源已修复；服务器现有 Nginx 使用 TCP 80/443，已有正常工作的域名证书。本文使用现有 HTTPS 站点，不重新申请证书，不安装 Certbot。下面以仓库配置中的 `oops.work` 为例；若实际域名不同，统一替换。

**最短流程：上传源码 → 运行一次安装命令 → 给现有 HTTPS server 加一条 include → 放行 TURN 端口 → 生成 Hub 注册文件。**

安装脚本自动完成依赖安装、Go 选择/下载校验、Relay 编译、服务账户、TURN 密钥、证书副本、systemd 开机启动和本机健康检查。需要人工填写的只有域名、公网 IP、现有证书链路径与私钥路径；网卡地址默认自动识别，复杂网络可显式指定。

本文是可执行的安装流程，不代表已在你的服务器上完成部署。脚本已通过 Shell 语法和配置生成测试，尚未在该 CentOS 主机实际运行。原生 iPhone 目前仍要求 App 前台在线，APNs/PushKit 未接入。

## 一、部署后是什么样

```text
Mac / iPhone ── HTTPS/WSS 443 ── 现有 Nginx
                                  │ /v1/*、/healthz
                                  └─ HTTP 127.0.0.1:18443 ── DJOneHub Relay

Mac / iPhone ── TURN 3478 / TURN TLS 5349 ── coturn ── UDP 中继端口
```

Mac/iPhone 的 Relay 地址是 **`https://oops.work`**，不带 `:8443`，也不带 `/relay` 等前缀。18443 只监听回环，不向公网开放。

同一个 Nginx HTTPS `server` 内，把 **`/v1/` 和 `/healthz` 留给 DJOneHub**，其他网站路径继续使用原配置。如果原网站已经使用这两个路径，不要覆盖；应选一个证书已覆盖、DNS 已配置的独立主机名，在它的 HTTPS server 中使用 include。当前客户端只接受 HTTPS origin，不能改成 `https://oops.work/djonehub`。

TURN 不经过 Nginx HTTP 反向代理。证书可共用，但仍需要下列端口；只有 80/443 可达不足以完成本方案的公网音频测试：

| 入站端口 | 用途 |
| --- | --- |
| TCP 80/443 | 现有网站、HTTPS/WSS，维持原配置 |
| TCP 3478、UDP 3478 | TURN/TCP、TURN/UDP |
| TCP 5349 | TURN/TLS，复用现有域名证书 |
| UDP 49160–49259 | TURN 的媒体分配端口，TCP/TLS 模式也需要 |

域名必须直达这台服务器；若网站域名经过 CDN/HTTP 代理，不能直接把同一个代理域名用于 TURN。首次建议只配置可靠的 IPv4 A 记录；不要保留不可达的 AAAA。

## 二、上传当前程序

在 Mac 执行，修改 SSH 用户和服务器地址：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub

scp /tmp/djonehub-relay-source.tar.gz SSH_USER@SERVER_IP:/tmp/
```

这样包含当前尚未提交的实现和安装脚本。不要仅克隆远端旧分支，也不要把 Mac 二进制上传到 Linux。

登录 CentOS，以普通 SSH 用户解压到新目录：

```sh
mkdir -p "$HOME/djonehub-install"
cd "$HOME/djonehub-install"
tar -xzf /tmp/djonehub-relay-source.tar.gz
```

每次升级用新的源码目录解压，避免旧源码文件残留。以下安装命令在这个目录执行。

## 三、确认四个参数，运行一次安装命令

找出现有站点使用的路径；这里只展示配置路径，不读取证书私钥内容：

```sh
sudo nginx -T 2>&1 | grep -E 'configuration file|server_name|ssl_certificate(_key)?[[:space:]]'
```

如果 Nginx 不在 PATH，用实际二进制路径（例如 `/usr/local/nginx/sbin/nginx`）替换后续 nginx 命令。选择该 HTTPS server 的 `ssl_certificate` 完整证书链与 `ssl_certificate_key` 私钥路径。公网 IPv4 从云控制台获取；它可能不同于网卡上的私网 IP。

下面是按本次服务器实际参数填写的完整命令。在解压后的源码目录执行，首次安装和中断后续跑都使用它：

```sh
sudo env -u GOROOT GOTOOLCHAIN=local \
  bash scripts/install-relay-centos8.sh \
  --domain oops.work \
  --public-ip 47.108.184.34 \
  --cert /etc/nginx/conf.d/oops.work.pem \
  --key /etc/nginx/conf.d/oops.work.key
```

域名、IP 或证书路径发生变化时再修改上述参数。多网卡或自动识别地址不正确时，在同一条命令末尾加 `--bind-ip 实际网卡IPv4`。云主机的这个值通常是私网 IP；脚本会生成 `external-ip=公网IP/网卡IP`，公网映射必须保持 TURN 端口的一对一对应。

安装脚本会从 PATH 和常见目录查找 `turnserver`，解析符号链接，并把实际绝对路径写入 systemd。自定义安装可加 `--turnserver-bin /实际路径/turnserver`。旧版脚本若报告 `Expected the EPEL coturn binary at /usr/bin/turnserver`，更新安装脚本和 `deploy/remote-call/render_install.py` 后重跑；不需要卸载 coturn 或创建 `/usr/bin` 软链接。

自动执行内容：

1. 用已经修复的系统 dnf 安装 GCC、make、pkg-config、Python、curl 等；没有 coturn 时安装 EPEL release 和 coturn。不修改软件源。
2. 优先使用已安装的 Go 1.26.3+；没有合适版本时，从 Go 官方元数据选择 1.26 系列最新稳定补丁、验证官方 SHA-256，安装到 `/opt/djonehub/go`。不删除 `/usr/local/go`。
3. 保留 CGO，构建 SQLite 所需的 Linux Relay；不需要安装 libopus 或 libusb。
4. 生成私有 TURN 密钥、复制并校验证书链/域名/私钥匹配，默认 `relay_only:true`，方便首次验收中继。
5. 启动 `djonehub-relay.service` 和 `djonehub-turn.service`，自动设置开机启动，检查回环 healthz。

脚本显示 `[6/6] Local services ready` 表示本机安装完成；**还要继续下一步接入 Nginx**。公网尚未验证时不会宣称部署完成。

若此前出现以下错误，直接使用本节上面的完整命令续跑，无需为此重新上传文件：

```text
go: go.mod requires go >= 1.26.3 (running go 1.25.3; GOTOOLCHAIN=local)
```

旧脚本检测版本时允许自动切换工具链，构建时却使用本机版本，可能把旧 Go 误判为可用。上述启动命令让检测和构建都使用 `GOTOOLCHAIN=local`，并清除外部 `GOROOT`；新版脚本内部也做同样处理。脚本会跳过 Go 1.25.3，寻找或下载符合要求的版本，不需要改低 go.mod、删除系统 Go 或手工覆盖工具链。

如需单独确认当前 PATH 中 Go 自带的版本，执行 `env -u GOROOT GOTOOLCHAIN=local go version`；普通 `go version` 在项目目录中可能显示自动切换后的版本。此设置只作用于该次命令，不修改全局 Go 配置。原因见 [Go 官方工具链切换说明](https://go.dev/doc/toolchain)。

安装脚本也会读取实际 `turnserver --help`，仅为仍支持的旧版 coturn 添加 `no-cli`、`no-dtls`、`no-tlsv1`、`no-tlsv1_1`。coturn 4.18 默认关闭 CLI/DTLS、默认最低 TLS 1.2，不再加入这些已删除或替换的旧项。已部署 4.18 服务器出现相关配置提示时，按[排查指南](REMOTE_TURN_TROUBLESHOOTING.md)备份并清理旧项；公网端口仍需单独放行。

脚本可用相同参数重跑：保留 SQLite 和兼容的 TURN 密钥，旧二进制/配置备份到 `/etc/djonehub/backup.*`，重启两个 DJOneHub 服务。升级会中断通话，应在空闲时执行。改变域名或手改 TURN URL 后，脚本可能要求先显式迁移；不会静默替换原身份配置。如果已经按旧手册安装了同名服务，它会提示 `Existing unmanaged file` 并停止，不覆盖旧配置。

## 四、现有 Nginx 只增加一行

在对应域名、监听 **443** 的已有 `server { ... }` 内，加入：

```nginx
include /etc/nginx/djonehub/relay.locations.conf;
```

位置示意（不是让你用这个示意覆盖完整站点）：

```nginx
server {
    listen 443 ssl;
    server_name oops.work;
    # 原有 ssl_certificate / ssl_certificate_key 等设置保持原样。

    include /etc/nginx/djonehub/relay.locations.conf;

    # 原有网站 location / 等继续保留。
}
```

不要放到 `http {}` 顶层、80 的 server 或已有 `location {}` 内，不要把同一行加两次。先确认此 server 没有冲突的 `/v1/`、`/healthz` location，以及影响 API 的 server 级重写/登录验证。如站点有登录网关、`auth_request` 或 WAF，要让这几个原生 API 使用 Relay 自身的身份认证，避免返回网站登录页。

检查后平滑重载：

```sh
sudo nginx -t && sudo nginx -s reload
curl -fsS https://oops.work/healthz | jq .
```

应看到：

```json
{
  "status": "ok",
  "version": 1,
  "capabilities": {
    "identity": true,
    "signaling": true,
    "voip_push": false,
    "turn": true
  }
}
```

在 Mac 上也执行这个 curl，证明外网客户端可达。healthz 成功只是 HTTP 检查；第六节的 `remote check` 才验证已鉴权的 WSS。

提供的 include 已设置 WebSocket Upgrade、HTTP/1.1 和超时，转发时保留 `/v1/...` 原路径与 Authorization，关闭缓存/错误页拦截/自动重试，避免刷新或配对请求被自动重放。配置方式参考 [Nginx WebSocket 官方说明](https://nginx.org/en/docs/http/websocket.html)。

CentOS 若返回 502：

```sh
curl -fsS http://127.0.0.1:18443/healthz
sudo journalctl -u djonehub-relay -n 60 --no-pager
getenforce
getsebool httpd_can_network_connect
```

本机 healthz 正常、Nginx error log 明确显示 `Permission denied`/AVC 且 SELinux Enforcing 时，按 [RHEL 8 官方反代说明](https://docs.redhat.com/en/documentation/red_hat_enterprise_linux/8/html-single/deploying_different_types_of_servers/index)允许 Web 服务连接后端：

```sh
sudo setsebool -P httpd_can_network_connect 1
```

这是允许 Web 服务发起网络连接的 SELinux 布尔开关，作用范围不止该站点；不要关闭 SELinux。已有严格隔离策略的机器应按自身策略配置。脚本不会替你更改此全局设置。

## 五、放行音频端口

云安全组添加第一节的 TURN 端口，**不需要开放 18443 或原手册中的 8443**。

若 firewalld 已运行，先查看网卡所在 zone：

```sh
sudo firewall-cmd --get-active-zones
```

把下面 `public` 换成承载公网流量的实际 zone，再执行；既加当前规则，也持久化，不整体 reload 防火墙：

```sh
DJ_FIREWALL_ZONE=public
for port in 3478/tcp 3478/udp 5349/tcp 49160-49259/udp; do
  sudo firewall-cmd --zone="$DJ_FIREWALL_ZONE" --add-port="$port"
  sudo firewall-cmd --permanent --zone="$DJ_FIREWALL_ZONE" --add-port="$port"
done
```

firewalld 未运行时不为这次安装强行启动，按现有防火墙方案加规则。云安全组与本机防火墙都必须放行；本机成功不等于公网成功。保留现有 SSH/网站规则及服务器出站媒体流量。

## 六、生成 Hub 注册文件，交付 Mac

先在 Mac 查询 `/api/devices`，取得真实 `dji-...` 模块 ID，准备好 Mac 再签发，注册文件仅有 10 分钟有效。

CentOS 执行：

```sh
DJ_MODULE_ID='dji-实际模块ID'
DJ_ENROLL_FILE="enrollment-$(date +%Y%m%d-%H%M%S).json"
sudo -u djonehub /usr/local/bin/djonehub-relay provision \
  --state-dir /var/lib/djonehub-relay --tenant home \
  --modules "$DJ_MODULE_ID" \
  --out "/var/lib/djonehub-relay/transfer/$DJ_ENROLL_FILE"

# 为当前 SSH 用户准备私有下载副本。
install -d -m 700 "$HOME/djonehub-transfer"
sudo install -o "$(id -un)" -g "$(id -gn)" -m 600 \
  "/var/lib/djonehub-relay/transfer/$DJ_ENROLL_FILE" \
  "$HOME/djonehub-transfer/$DJ_ENROLL_FILE"
printf '下载文件：%s\n' "$HOME/djonehub-transfer/$DJ_ENROLL_FILE"
```

Mac 下载该文件到私有目录，然后注册。替换实际 SSH 路径和输出文件名：

```sh
mkdir -p "$HOME/Library/Application Support/DJOneHub/private"
chmod 700 "$HOME/Library/Application Support/DJOneHub/private"
scp SSH_USER@SERVER_IP:/实际家目录/djonehub-transfer/实际注册文件.json \
  "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"
chmod 600 "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"

cd /Users/yuyi/Documents/Code/DJOneHub
./dist/djonehub-macos remote enroll --profile home \
  --relay https://oops.work \
  --enrollment-file "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"
./dist/djonehub-macos remote status --profile home
./dist/djonehub-macos remote check --profile home
```

确认 phase=ready、WSS 握手成功。随后按 [完整指南第 8–12 节](REMOTE_IPHONE_INSTALL_AND_TEST.md#8-xcode安装-iphone-app)安装 App、生成 iPhone 配对文件、设置模块远程目标并测试音频；**凡旧指南中出现 `https://oops.work:8443`，本部署统一使用 `https://oops.work`**。Mac 常驻后不要再用 remote check 创建第二条 Hub 连接。

已经注册到 `:8443` 的 profile 不能直接编辑文件换地址；它的地址保存在 Keychain 中。先结束通话、确认并撤销旧 Hub 身份，再用新的注册文件与新 profile 名重新注册、重新配对。不要盲目重复 enroll 覆盖旧 profile。

注册/配对成功后移除交付用 JSON 副本。Relay 不需要 AI Key、Apple APNs Key 或 Opus 库；这些都不是本轮服务器安装输入。

## 七、后续维护只需这些命令

```sh
# 服务与日志
sudo systemctl status djonehub-relay djonehub-turn --no-pager
sudo journalctl -u djonehub-relay -u djonehub-turn -n 100 --no-pager

# 原证书完成续期后，同步给 TURN；证书没变时不会重启。
sudo /usr/local/sbin/djonehub-refresh-turn-cert
```

Nginx 继续使用你原来的证书路径和续期方式。把 `djonehub-refresh-turn-cert` 加到现有证书续期成功后的部署操作中：它校验证书链/域名、密钥配对和剩余有效期后更新私有副本，只在证书变化时重启 coturn，不动 Nginx。**重启 coturn 会中断活动中继通话**，应结合自己的维护窗口执行；脚本没有自行安装 cron/timer 或改变现有续期任务。

重要文件位置：

| 文件/目录 | 用途 |
| --- | --- |
| `/usr/local/bin/djonehub-relay` | Linux 程序 |
| `/var/lib/djonehub-relay` | 配对/身份 SQLite，需保留 |
| `/etc/djonehub/turn-issuer.json` | 私有 TURN 密钥、URLs、relay_only |
| `/etc/djonehub/turnserver.conf` | coturn 私有配置 |
| `/etc/djonehub/deployment.env` | 原证书路径、域名；root 私有文件 |
| `/etc/nginx/djonehub/relay.locations.conf` | 已生成的 Nginx include |
| `/etc/djonehub/backup.*` | 重跑安装时保存的旧二进制/配置，不含数据库备份 |

升级：上传新源码到新目录，以相同参数重跑安装命令。失败时先看日志；不要清空数据库、重新生成在用密钥或加 `--allowerasing` 解决依赖。需要备份数据库时先停止 Relay，再复制整个状态目录（包含可能存在的 WAL/SHM），完成后启动；不可只拷贝运行中的单个 SQLite 文件假定备份一致。

Nginx 反代下，当前 Relay 的来源限流看到的是 127.0.0.1，多个设备共享同一来源额度。本文面向一个 Hub/少量设备联调，不代表已完成大规模反代部署优化。CentOS Linux 8 的维护终止状态也没有因安装成功改变。
