# iPhone 远程接听：安装、配对与双向音频联调

适用日期：2026-10-09；代码分支：`codex/remote-iphone-callkit`，截至第十二批实现。

已完成初次安装与配对后，App 内挂断、主动拨号和已有服务器升级请参见 [拨打、接听与挂断：更新和验收](REMOTE_IPHONE_CALL_FLOW.md)。2026-10-09 用户已确认 UAC 周期修复后的真机接听与双向音频测试通过；本次新增主动呼出仍需按该文档验收。

已有 Nginx 80/443 和可信证书、希望简化服务器安装时，优先使用 [CentOS 8 + Nginx 自动安装手册](RELAY_CENTOS8_NGINX_QUICKSTART.md)。它使用现有 HTTPS 443 反代到回环 Relay，并提供安装脚本；与本文后面的独立公网 8443 方案二选一，不能混用两种 Relay 地址。

本文按这台 **CentOS Linux 8** Relay 服务器编写：TCP 80/443 上已有网站，并已有 `https://oops.work` 的域名证书。Relay 与 coturn 加在这台机器上，现有网站继续占用 80/443。Mac 与 iPhone 的 Relay 地址是 `https://oops.work:8443`。若 `dnf` 出现 AppStream 403 或仓库 ID 重复，先按 [CentOS 8 软件源与依赖适配](REMOTE_RELAY_CENTOS8.md) 修好本次安装用的源，再回到第 3 节；那个文档只处理软件源，不替代本文的证书和端口步骤。

本文是按当前代码核对的部署与验收步骤，不是已经完成的公网/真机验收报告。当前支持 **App 前台在线等待来电**；APNs/PushKit 尚未实现，不能把锁屏、后台或被杀掉后的来电作为本轮成功条件。已验证 Go/Pion 的双向 Opus 与 TURN/UDP、TURN/TCP，以及 iOS 原生 WebRTC 模拟器协商；真实 iPhone ↔ Pion ↔ UAC ↔ 电话模块整条链路需要按本文实测。

## 1. 设备与部署拓扑

```text
第三部电话拨打模块 SIM 号码
           ↕ 蜂窝语音
USB 电话模块 ↔ Mac 的 Swift 音频宿主 ↔ Mac DJOneHub（Opus/WebRTC）
                                             ↕ 加密音频
                                      公网 coturn 中继
                                             ↕ 加密音频
                                      iPhone CallKit App

Mac 与 iPhone 均通过 HTTPS/WSS 连接公网 DJOneHub Relay，完成身份、邀请和信令。
Relay 不解码通话音频；coturn 转发 WebRTC 加密媒体。
```

准备以下资源：

| 项目 | 要求 |
| --- | --- |
| Hub 服务端 | macOS 13+，连接已能进行普通语音通话的 USB 电话模块、有效 SIM、稳定供电 |
| Mac 开发环境 | Go 1.26.3+、Swift 6 / 完整 Xcode、pkg-config、libusb、libopus **1.6.1**（也支持 1.5.2） |
| 中间服务器 | 这台 CentOS Linux 8 公网服务器，SSH/sudo 权限；与现有网站同机运行 Relay + coturn |
| DNS | 继续使用已有的 `oops.work`。A 记录已经指向本机公网 IPv4；Relay 与 TURN 共用这个名字，不再申请第二套域名 |
| iPhone | iOS 17+ 真机、能签名安装 App 的 Xcode Team、唯一 Bundle ID、网络和麦克风权限 |
| 主叫设备 | 另外一部手机/电话，用来拨打 USB 模块里的 SIM 号码 |

第一次测试让 Mac 用有线/Wi-Fi，iPhone 用另一个 Wi-Fi 或蜂窝网络。不要用接听 App 所在的同一部 iPhone 拨打测试电话，否则会引入本机蜂窝通话与 CallKit 的争用。

下文的 `SSH_USER`、`SERVER_IP`、`dji-REPLACE`、`ios_REPLACE` 以及网站证书的实际文件路径都必须换成这台服务器上的值。`oops.work` 是已确定的域名，保持不变。默认只先测一个模块、一台 iPhone、一通电话。服务器和设备保持自动校时。

## 2. Mac：构建 Hub 和原生音频宿主，获取模块 ID

在当前源码目录执行；不要使用旧 Release ZIP，它未必包含本分支的远程功能。先停止旧 DJOneHub 后端/宿主，避免两个进程争用模块。

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
git branch --show-current

# 未安装时执行；随后必须核对 Opus 版本。
brew install pkg-config libusb opus jq
go version
swift --version
pkg-config --modversion opus

DJONEHUB_REMOTE_OPUS=1 ./scripts/build-macos.sh
swift build --package-path macos/DJOneHubAudioHost -c release

mkdir -p "$HOME/Library/Application Support/DJOneHub/private"
mkdir -p "$HOME/Library/Application Support/DJOneHub/native-control"
chmod 700 "$HOME/Library/Application Support/DJOneHub/private"
chmod 700 "$HOME/Library/Application Support/DJOneHub/native-control"
```

构建脚本支持 Opus 1.5.2 和 1.6.1，当前可直接使用 Homebrew 的 1.6.1；脚本会打印实际版本。若装了多个版本，用 `PKG_CONFIG_PATH` 选择对应安装的 pkgconfig 目录，并核对 `pkg-config --modversion opus`。其他版本仍需验证后加入支持范围，不能去掉检查后假定已验证。若没有 Go，按 [Go 官方安装步骤](https://go.dev/doc/install)安装符合 `go.mod` 的版本。

产物为 `dist/djonehub-macos` 与 `macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost`。注册 Keychain 后保持使用同一路径、同一构建的 Mac 二进制；改签名/重新构建后的凭据访问需另行验证。不要用 sudo 启动 Mac 端程序。

先在 Mac 终端 A 启动普通后端以发现模块：

```sh
./dist/djonehub-macos --listen 127.0.0.1:7575
```

Mac 终端 B 查询：

```sh
curl -fsS http://127.0.0.1:7575/api/devices | jq .
```

记录目标模块的 `id`，例如实际的 `dji-...`，同时检查 `state`、`error`、`voice`。模块必须通过自动 USB 多模块入口发现；远程模式不支持 `--demo`、`--port` 和旧串口回退。后续不要用显示别名代替 ID。

如果还没有验证过模块的本地语音，用独立终端启动宿主：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost \
  --base-url http://127.0.0.1:7575
```

访问 `http://127.0.0.1:7575`，选择该模块，先完成一次普通人工接听及双方说话。若本地 UAC/蜂窝语音没有工作，先解决这一层。基线测试后结束通话，用 Ctrl-C 停止这两个前台进程，后面换成远程启动参数。

## 3. CentOS 8：部署代码与依赖

当前分支含未提交实现，直接在服务器 clone 远端可能缺文件。下面复制当前工作区的服务端源码快照，不需要先提交或推送。

Mac 执行，把 `SSH_USER` 和 `SERVER_IP` 换成实际登录用户和地址：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
COPYFILE_DISABLE=1 tar -czf /tmp/djonehub-relay-source.tar.gz \
  go.mod go.sum cmd internal pkg third_party deploy
scp /tmp/djonehub-relay-source.tar.gz SSH_USER@SERVER_IP:/tmp/
```

在 CentOS 8 上安装编译器和 coturn。这台机器已经有网站证书，不安装 certbot，也不申请新证书。

```sh
cat /etc/centos-release
uname -m

# 若这里失败并出现 AppStream 403 或 repository listed more than once，
# 先完成 docs/REMOTE_RELAY_CENTOS8.md，再用该文的 reposdir 安装同样这些包。
sudo dnf install -y gcc gcc-c++ make pkgconf-pkg-config \
  ca-certificates openssl python3 curl jq epel-release
sudo dnf install -y coturn
rpm -q gcc make pkgconf-pkg-config coturn
command -v turnserver
```

`go.mod` 要求 Go 1.26.3。CentOS 8 Vault 里的 golang 包版本不够，安装官方 Linux 包。下面按 x86_64 编写；`uname -m` 不是 x86_64 时先停，换成对应架构的官方包。

```sh
curl -fsSL -o /tmp/go1.26.3.linux-amd64.tar.gz https://go.dev/dl/go1.26.3.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go1.26.3.linux-amd64.tar.gz
export PATH=/usr/local/go/bin:$PATH
grep -q '/usr/local/go/bin' "$HOME/.bashrc" || echo 'export PATH=/usr/local/go/bin:$PATH' >> "$HOME/.bashrc"
env -u GOROOT GOTOOLCHAIN=local /usr/local/go/bin/go version
```

上述版本检测应显示 `go1.26.3` 或更新的兼容版本。显式设置 `GOTOOLCHAIN=local`，避免普通 `go version` 自动切换工具链后掩盖本机版本过旧。若报 `GLIBC_x.xx not found`，停在这一步，保留完整报错；不要改用 Vault 里的旧 Go，也不要关掉 CGO。使用自动安装脚本遇到 Go 1.25.3 版本错误时，直接按 [自动安装手册第三节](RELAY_CENTOS8_NGINX_QUICKSTART.md#三确认四个参数运行一次安装命令)的完整命令续跑，不要执行本节的手动重装流程。

```sh
mkdir -p "$HOME/djonehub-relay-src"
cd "$HOME/djonehub-relay-src"
tar -xzf /tmp/djonehub-relay-source.tar.gz
env -u GOROOT CGO_ENABLED=1 GOTOOLCHAIN=local /usr/local/go/bin/go \
  build -o /tmp/djonehub-relay ./cmd/djonehub-relay
sudo install -m 755 /tmp/djonehub-relay /usr/local/bin/djonehub-relay
```

Relay 使用 SQLite 的 CGO 驱动，因此在这台 Linux 上编译，保留 `CGO_ENABLED=1`。不要把 Mac 可执行文件复制过去，也不要用 `CGO_ENABLED=0` 制作看似能编译、实际上缺 SQLite 驱动的产物。Relay 本身不需要 libopus、libusb 或 `remoteopus` 构建标签。不要运行 `go mod tidy` 改写本仓库的依赖替换。

这台服务器已有网站和其他账户。只新增 `djonehub` 系统账户和下面两个目录；账户已存在时核对属主，不要重复 `useradd`，也不要改动网站的用户、目录和配置。

```sh
if ! id djonehub >/dev/null 2>&1; then
  sudo useradd --system --user-group --home-dir /var/lib/djonehub-relay \
    --shell /usr/sbin/nologin djonehub
fi
sudo install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay
sudo install -d -o root -g djonehub -m 750 /etc/djonehub /etc/djonehub/tls
sudo install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay/transfer
```

本文两个服务同用 `djonehub` 账户。Relay issuer 文件必须由实际运行 Relay 的 UID 拥有且为 0600 普通文件。

## 4. CentOS 8：沿用 oops.work 证书，Relay 使用 8443

现有网站继续监听 TCP 80 和 TCP 443。本步骤不停止网站、不把 Relay 或 coturn 绑到这两个端口，也不运行 `certbot --standalone`。

Mac/iOS 的 Relay 地址必须是 `https://主机[:端口]/` 这种源站根地址。当前代码拒绝带路径的地址，例如 `https://oops.work/relay`。因此不能把 Relay 挂到现有网站的某个 URL 路径下。443 已经被网站占用，Relay 改在 **8443** 上用同一张 `oops.work` 证书自行终止 TLS。证书校验的是域名，不包含端口，所以 `https://oops.work:8443` 与现在的 `https://oops.work` 使用同一张证书。只放行 TCP 443 的网络到达不了 8443 上的信令，也到达不了 5349 上的 TURN/TLS；那种网络要另备 IP，本机这次不改现有网站。

TURN/TLS 也不是 HTTP，不能挂到网站的 `/turn` 路径。coturn 继续听 5349，SNI 同样是 `oops.work`。

先确认域名已经指到本机，并且 443 上的证书就是 `oops.work`：

```sh
getent hosts oops.work
echo | openssl s_client -connect oops.work:443 -servername oops.work 2>/dev/null \
  | openssl x509 -noout -subject -ext subjectAltName -dates
```

`subjectAltName` 须包含 `DNS:oops.work`。没有可用 IPv6 时不要保留指向错误地址的 AAAA。DNS 使用直接解析，TURN 不能走普通 HTTP CDN 代理。

找出网站正在使用的 PEM，只复制，不移动、不改网站配置。常见位置在 nginx 的 `ssl_certificate`、httpd 的 `SSLCertificateFile`，或 `/etc/letsencrypt/live/oops.work/`。

```sh
sudo grep -RInE 'ssl_certificate|SSLCertificateFile|SSLCertificateKeyFile' \
  /etc/nginx /etc/httpd /usr/local/nginx/conf /etc/letsencrypt 2>/dev/null
```

把下面两个变量换成上一步看到的**证书链**和**私钥**。证书链要包含服务器证书和中间证书。若网站把证书和私钥写在同一个文件里，拆成两个 PEM 再复制，原文件保持不动。

```sh
WEB_CERT=/path/to/fullchain.pem
WEB_KEY=/path/to/privkey.pem

sudo install -o djonehub -g djonehub -m 600 \
  "$WEB_CERT" /etc/djonehub/tls/fullchain.pem
sudo install -o djonehub -g djonehub -m 600 \
  "$WEB_KEY" /etc/djonehub/tls/privkey.pem

sudo openssl x509 -in /etc/djonehub/tls/fullchain.pem -noout -pubkey | openssl md5
sudo openssl pkey -in /etc/djonehub/tls/privkey.pem -pubout | openssl md5
sudo openssl x509 -in /etc/djonehub/tls/fullchain.pem -noout -ext subjectAltName -dates
```

两条 `md5` 必须相同，SAN 必须包含 `DNS:oops.work`。`/etc/letsencrypt/live` 里常见的是符号链接，而 `archive` 目录往往只有 root 能进入；`install` 会复制成 `djonehub` 拥有的普通文件，Relay 和 coturn 读这份副本。客户端用系统信任校验这张已经在 `https://oops.work` 上使用的证书，不使用 `curl -k`、自签名绕过或 App ATS 例外。

云安全组在现有 80/443 和 SSH 规则之外，再放行下面的入站端口。服务器本机若启用了 firewalld，用同样的端口追加规则；firewalld 没在运行时不要为了这次安装把它打开。

| 端口 | 用途 |
| --- | --- |
| TCP 80、TCP 443 | 现有网站，保持原样 |
| TCP 8443 | Relay HTTPS/WSS |
| UDP 3478、TCP 3478 | TURN/UDP、TURN/TCP |
| TCP 5349 | TURN/TLS，证书名称为 `oops.work` |
| UDP 49160–49259 | TURN 媒体分配端口；即使客户端走 TCP/TLS 也需要 |

```sh
if systemctl is-active --quiet firewalld; then
  sudo firewall-cmd --permanent --add-port=8443/tcp
  sudo firewall-cmd --permanent --add-port=3478/tcp
  sudo firewall-cmd --permanent --add-port=3478/udp
  sudo firewall-cmd --permanent --add-port=5349/tcp
  sudo firewall-cmd --permanent --add-port=49160-49259/udp
  sudo firewall-cmd --reload
  sudo firewall-cmd --list-ports
fi
```

服务器出站也要允许 TURN 媒体及必要的 DNS 流量。Mac 管理端口 7575 和 native bootstrap listener 不向公网开放。保持 SELinux 的现有保护。若服务报权限拒绝，用 `journalctl` 和 `ausearch -m avc -ts recent` 看 AVC 后再按实际上下文修复，不以关闭 SELinux 作为安装步骤。

## 5. CentOS 8：配置 TURN 与临时凭证

先复制仓库模板：

```sh
cd "$HOME/djonehub-relay-src"
sudo install -o djonehub -g djonehub -m 600 \
  deploy/remote-call/turnserver.conf.example /etc/djonehub/turnserver.conf
sudoedit /etc/djonehub/turnserver.conf
```

把 `realm=turn.example.com` 改成 `realm=oops.work`，证书路径保持 `/etc/djonehub/tls/fullchain.pem` 和 `privkey.pem`。网络地址按实际环境设置：

- 公网 IP 直接在服务器网卡上：可以明确设置 `listening-ip=该地址`、`relay-ip=该地址`。
- 云主机网卡上是私网 IP、外部有一对一公网 NAT：设置 `listening-ip=私网IP`、`relay-ip=私网IP`、`external-ip=公网IP/私网IP`，并确保 UDP relay 端口范围按端口一对一映射。
- 保留模板的 relay 端口范围、配额和禁止内网 peer 规则，不启用 `no-auth`。

这些地址必须来自云平台/网卡信息，不能照填示例 IP。coturn 参数语义见 [官方配置样例](https://github.com/coturn/coturn/blob/master/examples/etc/turnserver.conf)。

然后运行以下一次性脚本。域名已经写成 `oops.work`。它只把随机主密钥写入两个私有文件，不打印密钥；issuer 文件存在则拒绝覆盖。

```sh
sudo python3 - <<'PY'
import json, os, pwd, secrets
from pathlib import Path

domain = "oops.work"
issuer = Path("/etc/djonehub/turn-issuer.json")
turn = Path("/etc/djonehub/turnserver.conf")
config = turn.read_text()
if issuer.exists() or any(x.strip().startswith("static-auth-secret=") for x in config.splitlines()):
    raise SystemExit("已有凭据配置，停止；不要覆盖在用主密钥")
secret = secrets.token_hex(32)
account = pwd.getpwnam("djonehub")
os.umask(0o077)
with issuer.open("x") as f:
    json.dump({"urls": [
        f"turn:{domain}:3478?transport=udp",
        f"turn:{domain}:3478?transport=tcp",
        f"turns:{domain}:5349?transport=tcp"
    ], "secret": secret, "relay_only": True}, f, indent=2)
with turn.open("a") as f:
    f.write("\nstatic-auth-secret=" + secret + "\n")
for path in (issuer, turn):
    os.chown(path, account.pw_uid, account.pw_gid)
    os.chmod(path, 0o600)
PY
```

首次验收设 `relay_only:true`，确保成功通话实际依赖中继，避免同网段直连掩盖 TURN 配置错误。主密钥只存在服务器；iPhone 与 Mac 每通使用临时凭据。不要把 issuer JSON 当作 iPhone 配对文件。

## 6. CentOS 8：启动两个服务与检查

停掉 EPEL 可能自动启用的默认 coturn，避免它抢 3478。没有这个 unit 就跳过。不要停止现有网站。

```sh
if systemctl cat coturn.service >/dev/null 2>&1; then
  sudo systemctl disable --now coturn
fi
```

用 `sudoedit /etc/systemd/system/djonehub-relay.service` 写入：

```ini
[Unit]
Description=DJOneHub Relay
Wants=network-online.target
After=network-online.target

[Service]
User=djonehub
Group=djonehub
UMask=0077
ExecStart=/usr/local/bin/djonehub-relay serve --state-dir /var/lib/djonehub-relay --listen 0.0.0.0:8443 --tls-cert /etc/djonehub/tls/fullchain.pem --tls-key /etc/djonehub/tls/privkey.pem --turn-config /etc/djonehub/turn-issuer.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

用 `sudoedit /etc/systemd/system/djonehub-turn.service` 写入：

```ini
[Unit]
Description=DJOneHub TURN
Wants=network-online.target
After=network-online.target

[Service]
User=djonehub
Group=djonehub
UMask=0077
RuntimeDirectory=djonehub-turn
ExecStart=/usr/bin/turnserver -c /etc/djonehub/turnserver.conf --pidfile=/run/djonehub-turn/turnserver.pid --log-file=stdout
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

先用 `command -v turnserver` 核对二进制位置；不是 `/usr/bin/turnserver` 时修改 ExecStart。配置中不添加 `daemon`，由 systemd 管理前台进程。

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now djonehub-turn djonehub-relay
sudo systemctl status djonehub-turn djonehub-relay --no-pager
sudo journalctl -u djonehub-relay -u djonehub-turn -n 80 --no-pager
```

从 Mac 检查外网访问。第一条确认原网站仍在 443 上响应；第二条才是新的 Relay。

```sh
curl -sS -o /dev/null -w 'existing-site %{http_code}\n' --max-time 20 https://oops.work/
curl -fsS https://oops.work:8443/healthz | jq .
openssl s_client -connect oops.work:5349 \
  -servername oops.work -verify_hostname oops.work \
  -verify_return_error </dev/null
```

healthz 应为 `status:ok`、`identity:true`、`signaling:true`、`turn:true`、`voip_push:false`。TURN TLS 检查应无证书链/域名错误；这个检查仅证明 TLS listener 可达，不能替代 TURN 分配和音频测试。原网站只要 TLS 握手成功即可，HTTP 状态码沿用网站自己的响应。

`/etc/djonehub/tls` 里是证书副本。网站自己的续期只会更新它原来的 PEM，不会改这两个副本。网站证书更新后，重新执行第 4 步的两条 `install`，再在无通话时重启 `djonehub-relay` 和 `djonehub-turn`。长期运行可以把这两条复制和重启放进网站现有的续期钩子；本轮手动联调不把自动续期视为已完成。重启会中断活动通话。

## 7. 注册 Mac Hub 身份

准备好 Mac 后再签发，注册文件只有 10 分钟有效。在 CentOS 服务器上执行，模块 ID 必须与第 2 步一致：

```sh
sudo -u djonehub /usr/local/bin/djonehub-relay provision \
  --state-dir /var/lib/djonehub-relay \
  --tenant home \
  --modules dji-REPLACE \
  --out /var/lib/djonehub-relay/transfer/enrollment.json

# 为当前 SSH 登录用户准备可下载的私有副本。
install -d -m 700 "$HOME/djonehub-transfer"
sudo install -o "$(id -un)" -g "$(id -gn)" -m 600 \
  /var/lib/djonehub-relay/transfer/enrollment.json \
  "$HOME/djonehub-transfer/enrollment.json"
```

Mac 执行，把 `SSH_USER` 和 `SERVER_IP` 换成实际值。远程路径相对该用户的家目录：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
scp SSH_USER@SERVER_IP:djonehub-transfer/enrollment.json \
  "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"
chmod 600 "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"

./dist/djonehub-macos remote enroll --profile home \
  --relay https://oops.work:8443 \
  --enrollment-file "$HOME/Library/Application Support/DJOneHub/private/enrollment.json"

./dist/djonehub-macos remote status --profile home
./dist/djonehub-macos remote check --profile home
```

验收：`status` 的 phase 为 `ready`，模块范围正确；`check` 报 WSS handshake verified。此时还没启动常驻远程 Hub，所以可以运行 check；常驻后不要重复运行，否则会建立第二条 Hub 连接并被拒绝。

凭据进入当前用户 macOS Keychain，无需手工复制 access/refresh token。若 Keychain 返回权限或 OSStatus 错误，停在此步处理当前程序签名/当前登录账户的 Keychain 访问，不要切换 sudo、明文文件或无限重试。若 phase 为 `enrolling` / `refresh_pending`，按文末恢复流程处理。

成功后删除 Mac 与 Linux 两处已消费的 enrollment 副本。要再次 provision 时使用新的输出文件名；服务拒绝覆盖已有输出。

## 8. Xcode：安装 iPhone App

在 Mac 打开已提交到工作区的工程：

```sh
open /Users/yuyi/Documents/Code/DJOneHub/ios/DJOneHubPhone/DJOneHubPhone.xcodeproj
```

1. 在 Xcode Settings 的 Accounts 中登录自己的开发账号，连接并信任 iPhone；根据设备提示启用 Developer Mode。
2. 选择 **DJOneHubPhone** target → Signing & Capabilities，选择自己的 Team，保留 Automatically manage signing，把默认 `org.djonehub.phone` 改成自己可签名的唯一 Bundle ID。
3. 确认 Background Modes 中已有 Audio 和 Voice over IP（源码 `UIBackgroundModes` 为 `audio`、`voip`）。CallKit 来电报告需要 VoIP 模式；当前仍需 App 前台在线接收邀请，尚未配置 Push Notifications 或 APNs 密钥。
4. 选择 **DJOneHubPhone** scheme，运行目标选择真实 iPhone，等待本地 Swift 包和固定版本 WebRTC XCFramework 解析完成，点击 Run 安装。
5. 启动 App。后续连接时允许麦克风权限；如系统请求本地网络权限，联调时允许。

若使用 XcodeGen 重新生成工程，应同时把自己的 Team / Bundle ID 设置写入本地 `project.yml`，避免生成操作覆盖 Xcode 修改。保持同一 App 身份测试；不同 Bundle ID 是不同安装/Keychain 范围。

真实设备安装需要有效签名；`CODE_SIGNING_ALLOWED=NO` 只能用于先前的构建检查，不能用它把 App 安装到真机。设备配对、签名和运行步骤参考 [Apple 真机运行说明](https://developer.apple.com/documentation/xcode/running-your-app-on-simulated-or-physical-devices)。当前没有已发行的 TestFlight 或可直接点击安装的 IPA。

## 9. Mac 与 iPhone 配对

先完成 App 安装再生成配对文件，它只有 5 分钟有效。Mac 执行：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./dist/djonehub-macos remote pair --profile home --modules dji-REPLACE \
  --out "$HOME/Library/Application Support/DJOneHub/private/iphone-pairing-01.json"
```

通过 AirDrop 或自己选择的安全文件传输把 JSON 保存到目标 iPhone 的“文件”App。打开 DJOneHub，点击 **导入 Hub 配对文件**，选择该 JSON。成功后界面显示安装 ID（`ios_...`）。不需要在 App 单独填写服务器地址，地址已经在配对文件中。

回到 Mac 获取服务端确认：

```sh
./dist/djonehub-macos remote installations --profile home
```

确认安装 ID 与手机显示一致、模块范围正确且未撤销，记录这个实际安装 ID。过期/已消费文件不能重复使用；重新生成需新的文件名。成功后移除 Mac/iPhone 上的配对交付文件。不要把文件放到公开链接或聊天群。

本批采用文件导入，没有扫码配对；不要导入 Linux 的 enrollment.json、turn-issuer.json 或自己拼装 token。

## 10. Mac：启动远程模式并指定接听手机

Mac 终端 A（保持运行，首次测试期间可另开终端运行 `caffeinate -i` 避免系统休眠）：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./dist/djonehub-macos --listen 127.0.0.1:7575 \
  --remote-online-profile home \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

Mac 终端 B：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost \
  --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

两个进程必须由同一 Mac 用户运行，bootstrap 使用完全相同的绝对路径。bootstrap 由 Go 程序创建，不要自行写 token、复制到手机、cat 到日志或用其他宿主替代。当前发行启动器还不会自动传递这些远程参数，所以本轮保持两个终端启动。

Mac 终端 C 使用校验脚本设置模块路由。`--profile` 必须与终端 A 相同；脚本仅在真实设备、Hub 授权、iPhone 授权均匹配时继续。只有一个符合条件的模块/手机时自动选择，有多个时要求显式选择，不再填写 `dji-REPLACE` / `ios_REPLACE`：

```sh
bash scripts/configure-remote-answering.sh --profile home
```

可附加 `--module` 和 `--installation` 指定真实 API ID；可加 `--check-only` 只检查接听条件。脚本遇错即停，会保留 HTTP 状态和错误原因，保留原 AI Profile 的其他字段，只关闭 AI 开关并配置远程目标。不要用 sudo 运行 Mac 命令。

2026-10-09 实查：Xiaomi 的真实 ID 是 `dji-75d81d083dca`，已注册的 `dji-xiaomi` 不是该 ID。若 profile/安装仍授权 `dji-xiaomi`，先按 [Xiaomi 授权修复步骤](REMOTE_XIAOMI_PAIRING_RECOVERY.md)重新注册、配对；恢复流程使用新 profile `home-xiaomi`，终端 A 和 C 都改用该 profile。不能只改本机目标配置绕过权限。

必须在没有振铃/通话时修改。409 若为 `device is offline or unavailable`，应核对模块 ID/在线状态；若为 `route_locked_by_call`，则先结束通话。确认以下状态：

```json
{
  "effective_route": "remote",
  "online_transport": {"configured": true, "state": "online"},
  "codec": {"available": true, "version": "libopus 1.6.1"},
  "available": false,
  "reason": "online_only_not_production",
  "capabilities": {"signaling": true, "voip_push": false, "remote_media": true}
}
```

这是字段节选，codec.version 是实际加载的库版本，使用旧的受支持安装时可为 `libopus 1.5.2`。`available:false` 是当前开发版本保留的整体产品状态，不表示在线调度必然失败。反之 `online` 只证明 Hub 信令在线，不能证明 iPhone 或 UAC 已就绪。若 `effective_route` 是 `agent`，必须先关闭此模块 AI；AI Provider 出错也不会自动转远程。

## 11. 完成第一次双向音频测试

1. iPhone 打开 DJOneHub，点击 **连接并等待来电**，授予麦克风权限，等待显示在线。保持 App 前台、屏幕解锁；测试期间暂时避免自动锁屏。
2. 确认 Mac 上的后端和 Swift 宿主都在运行，模块在网，`effective_route=remote`，Hub `online_transport.state=online`。
3. 用第三部电话拨打模块 SIM 的号码。不要在 Mac 网页同时点击接听。iPhone 应出现 CallKit 来电界面，显示 DJOneHub；当前邀请显示名是固定值，不以显示真实号码作为验收条件。
4. 在 iPhone 系统通话界面接听。等待建立通话；接听过程中先有信令/媒体就绪、再有蜂窝接听、再由系统激活音频，单独看到来电或计时不能证明声音已通。
5. **主叫 → iPhone**：主叫读“12345，主叫到手机”，iPhone 使用者复述。连续说话至少 30 秒。
6. **iPhone → 主叫**：iPhone 使用者读“67890，手机到主叫”，主叫复述。连续说话至少 30 秒。
7. 双方交替并短暂同时说话，再持续 2–3 分钟。初测使用听筒，设备分处两个房间，避免扬声器声学串音掩盖媒体问题。测试静音/取消静音：应只影响 iPhone 向主叫发送的语音，主叫声音仍可听见。
8. 从 iPhone 挂断，确认主叫断开、模块恢复空闲。等 Hub 从 busy/retrying 回到 online 后，再拨一通，并由主叫挂断。App 若显示已断开，先重新连接再发起新来电。

通话过程中，可在 Mac 终端 C 检查：

```sh
DJ_API="http://127.0.0.1:7575/api/devices/dji-75d81d083dca"
curl -fsS "$DJ_API/calls/status" | jq '{active, audio_host, last_poll_error}'
curl -fsS "$DJ_API/calls/audio/host/config" | jq .
curl -fsS "$DJ_API/remote-calls/status" | jq .
```

这里使用已查到的 Xiaomi 模块 ID；测试其他模块时用 `/api/devices` 返回的对应 `id`。

观察 active 中的 UUID、route、control_state，以及 audio_host 的 running/error；公开配置的 `media_mode` 应随会话进入 remote。它不返回远程媒体 token。结束后 `active` 应清空；历史记录中仍有已结束通话是正常的。保留错误和 UUID 即可，分享排障信息时去掉电话号码等个人数据。

**双向验收条件：两边均能独立复述对方内容、静音可恢复、两端挂断均能收尾、第二通可重新接听。** 仅 /healthz 成功、WSS online、CallKit 响铃、SDP/DTLS 成功或单向有声都不算通过。

## 12. 公网中继分项验收

先完成上一节，然后在 Linux 的 `/etc/djonehub/turn-issuer.json` 中保留 `secret` 和 `relay_only:true`，每轮只保留一个 URL：

| 轮次 | urls 中唯一元素 | 目的 |
| --- | --- | --- |
| A | `turn:oops.work:3478?transport=udp` | 验证 TURN/UDP |
| B | `turn:oops.work:3478?transport=tcp` | 验证 TURN/TCP |
| C | `turns:oops.work:5349?transport=tcp` | 验证 `oops.work` 证书的 TURN/TLS |

用 `sudoedit` 修改，完成后确保文件仍为 djonehub 所有、0600、普通文件；只改 issuer 时重启 Relay 即可。每次在无通话时执行：

```sh
sudo chown djonehub:djonehub /etc/djonehub/turn-issuer.json
sudo chmod 600 /etc/djonehub/turn-issuer.json
sudo systemctl restart djonehub-relay
```

重启会断开 iPhone WSS，需重新点“连接并等待来电”；Hub 自动重连，等其 online 后发起**新来电**。每轮都重复双向说话和挂断测试。仅把 UDP URL 与 TLS URL 一起配置后成功，无法证明 TLS 分支可用；所以单 URL 测试是必要区分。

随后让 iPhone 关闭 Wi-Fi、使用蜂窝数据再测。若要测“UDP 被阻断但 TLS 仍可通”，只在客户端所在网络限制相应 UDP，保留服务器 UDP relay 端口；不要把 coturn 的 UDP 媒体分配端口也关掉。这台服务器的 443 属于现有网站，5349 上的 TURN/TLS 和 8443 上的信令都不覆盖“只允许 TCP 443”的网络。

所有轮次通过后，可恢复三条 URLs，并按需要设 `relay_only:false` 允许直连优先；再重复一次正常呼叫。测试结果记录日期、网络、URL 组合、方向、通话时长与错误。NAT64、网络切换和长通话另行验收；当前不支持通话中 ICE 重启，TURN 临时凭据为 4 小时，两端会在到期前 30 秒结束该通。

## 13. 常见问题与恢复

| 现象 | 优先检查 |
| --- | --- |
| healthz 访问失败 | `https://oops.work:8443/healthz`、8443 安全组与 firewalld、复制过来的证书链、Relay unit 日志；443 上的原网站仍应能打开。不能用跳过 TLS 校验掩盖问题 |
| Relay 启动报告私有配置错误 | state-dir 0700，issuer 为运行 UID 所有/0600/普通文件，JSON 无未知字段，TURN URL 显式端口和 transport |
| `turn:false` | serve 是否传了正确 `--turn-config`，不要误以为已开中继 |
| 注册/刷新停在 pending | 不重复消费旧 grant/token；执行下方身份恢复流程 |
| Hub 不进入 online | profile ready、origin 为 `https://oops.work:8443`、仅一个 Hub 进程；常驻时不运行 remote check |
| 手机在线但不响 | AI 是否仍开；模块 ID 和安装 ID/授权范围是否一致；是否先在线后拨打；旧来电不重邀 |
| 接听前失败 | TURN 分配、证书/外网端口、DTLS、iPhone 权限；用单 URL 分离网络路径 |
| 接听后很快挂断 | Swift 宿主是否运行、bootstrap 路径/用户是否一致、精确 UAC 绑定及 host running/error；不要调大超时掩盖宿主未启动 |
| 单向声音 | 分别核对 iPhone 静音/麦克风、UAC 输入输出及媒体模式；用两个房间的主叫/接听者复述测试定位方向 |
| Mac 本地接听显示音频已连接但双方无声 | 检查宿主终端和实际默认输入/输出。2026-10-09 修复了蓝牙 VoiceProcessing 重配置后 AVAudioEngine 自动停止、UAC 仍运行的情况；更新源码、重新构建并在原终端重启 Swift 宿主，故障后会重建普通音频引擎 |
| 放到后台后不再来电 | 当前在线版的已知限制，空闲进入后台会断开；APNs/PushKit 尚未接入 |
| 第二通不响 | Hub 要等上一通清理并重连；手机若因错误断开则手动连接；必须重新拨打新的电话 |

手机已知安装 ID 的恢复：先在 Mac 执行 `./dist/djonehub-macos remote revoke --profile home --installation ios_实际ID`，再在 App 断开并选择“清理本机配对凭据”，重新生成配对文件、导入，并更新模块 target_installation_id。

不知道安装/Hub ID，但有 App 或 `remote status` 显示的公钥指纹时，在 Linux 执行：

```sh
sudo -u djonehub /usr/local/bin/djonehub-relay lookup \
  --state-dir /var/lib/djonehub-relay --key-fingerprint 实际64位SHA256指纹

# 确认 lookup 对应正确身份后，撤销它。
sudo -u djonehub /usr/local/bin/djonehub-relay revoke \
  --state-dir /var/lib/djonehub-relay --id 实际身份ID
```

撤销 Hub 会使其子安装失效。Hub 恢复需要新 enrollment、新 profile 名重新注册，再重新配对；不要删除整个 SQLite 数据目录。详见 [第十批恢复机制](remote-calls-tenth-batch.md)。TURN 已签发凭据的撤销边界见 [中继部署说明](../deploy/remote-call/README.md)。

## 本指南核对范围

已逐项核对当前 CLI flags、API 路径/JSON、源码构建产物、私有文件约束、App 按钮和测试状态。Relay 源站地址不能带路径，因此在 443 已被网站占用时使用 `https://oops.work:8443`，并复用该域名现有证书。CentOS 8 的 dnf、firewalld、证书复制和 systemd 步骤仍是待执行的运维步骤：本次没有登录这台服务器，没有改防火墙、没有读取网站证书文件、没有读取真实 Keychain、没有安装真实手机或拨打电话。执行到具体错误时，以该层日志与实测结果推进，不能把本文视作已经通过全链路验收。
