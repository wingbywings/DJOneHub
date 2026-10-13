# DJOneHub 三端编译、部署与配对指南

更新：2026-10-13。适用代码：本机 `codex/remote-iphone-callkit` 分支的**当前工作区**，包括尚未提交的 Web 管理实现。仅克隆远端分支可能拿不到这些改动；Relay 应使用本文从当前工作区生成的源码包。

本文是当前版本的统一部署入口，覆盖 Mac Hub + AudioHost、Relay + coturn + Web 管理和 iPhone App。临时身份的 Web 注册、配对管理、健康与在线展示已验收；本文编写不代表已替换真实服务。R06 并行媒体迁移、R07 AudioHost 故障恢复、R12 完整质量观测及正式分发仍未完成。

**本文全部步骤采用统一服务方案：管理入口 `https://oops.work/relay-admin/`，管理模块与 Relay/APNs 同进程、同 18443 回环监听，复用 `oops.work` 原证书。** Mac 包标签为 `module-names-20261013`。从旧独立管理服务迁移的专门说明见[统一 Relay 迁移指南](RELAY_INTEGRATED_ADMIN_DEPLOY.md)。

## 0. 部署参数与顺序

| 项目 | 本线程使用的值 / 要求 |
| --- | --- |
| Mac 源码目录 | `/Users/yuyi/Documents/Code/DJOneHub` |
| Mac 平台 | Apple Silicon，macOS 13+；当前发行脚本不支持 Intel |
| Go | `go.mod` 要求至少 1.26.3；本机当前为 1.26.5 |
| Xcode | 安装完整 Xcode，并选择其开发工具目录；本机当前为 Xcode 27.0 |
| iPhone | iOS 17+ 真机 |
| Mac Web | `http://127.0.0.1:7575/remote/`，模块管理在 `/` |
| 原生 Relay | `https://oops.work` → Nginx → `127.0.0.1:18443` |
| Relay 管理 Web | 首选 `https://oops.work/relay-admin/` → Nginx → `127.0.0.1:18443`；与 native 同进程，复用证书 |
| Relay 用户 / 身份目录 | `djonehub` / `/var/lib/djonehub-relay` |
| 管理账户目录 / tenant | `/var/lib/djonehub-relay-admin` / `home`；已有身份必须沿用实际 tenant |
| Apple Team / Bundle ID | `8AHAFH9VTZ` / `com.oops.djonehub` |
| APNs Key ID | `BA73J355G9`；本机密钥路径 `dist/AuthKey_BA73J355G9.p8` |
| 当前推送环境 | Debug App + sandbox APNs |

其他环境统一替换域名、IP、目录和 Apple 标识。本文沿用现有 `oops.work`，管理页面仅增加 `/relay-admin/` 路由；无需新增管理子域名、DNS 记录或证书。原生 Relay 地址保持 `https://oops.work`，不带 `:8443` 或路径。

**线路备注显示版须同时升级 Relay、Mac Hub 和 iPhone App，保留既有配对。** Mac 模块备注会同步到手机线路菜单、通话页和记录；在线通常约 30 秒内更新，没有备注时显示 ID，同名线路附带 ID。CallKit 待接听页同时显示“对方号码 · 模块备注”（布局由 iOS 决定）。详见[线路备注更新说明](remote-module-names-batch.md)与[CallKit 显示说明](remote-callkit-module-name-display.md)。

执行顺序：**先编译各端 → 空闲时停用旧独立 admin 并升级 Relay → 启用同进程管理模块 → 配置现有 Nginx 路由 → 安装并启动 Mac → 按需覆盖安装 iPhone → 配对或导入旧配置 → 来电验收。** 重启统一 Relay 会同时中断信令和管理会话；coturn 仍独立运行。仅修改管理入口不要求已配对 iPhone 重新安装。

## 1. 编译、安装与运行 Mac

### 1.1 检查构建环境

在 Mac 终端执行：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
git branch --show-current
go version
xcode-select -p
xcodebuild -version
swift --version
pkg-config --version
```

如果 `xcode-select -p` 指向 CommandLineTools，安装完整 Xcode 后执行（应用路径按实际修改）：

```sh
sudo xcode-select --switch /Applications/Xcode.app/Contents/Developer
```

先打开 Xcode 完成首次组件安装和许可确认。Go 未安装或版本不足时安装满足 `go.mod` 的版本；已有 Homebrew 可用 `brew install go pkg-config` 安装工具，再核对版本。

### 1.2 推荐：构建完整发行包

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
sh scripts/package-macos-arm64.sh module-names-20261013
```

脚本自动完成：固定版本 libusb 下载与 SHA-256 校验、静态 Opus 1.6.1、带 `remoteopus` 的 Go Hub、Swift release AudioHost、启动器、依赖库、许可证、ad-hoc 签名和 ZIP 校验文件。不需要单独复制 AudioHost，也不需要目标 Mac 安装 Homebrew Opus。

产物：

```text
dist/release/DJOneHub-macOS-arm64-module-names-20261013/
dist/release/DJOneHub-macOS-arm64-module-names-20261013.zip
dist/release/DJOneHub-macOS-arm64-module-names-20261013.zip.sha256
```

构建会重建同名发行目录和 ZIP；若需保留旧包，先复制或使用新的版本标签。脚本需要联网下载源码及 Go/Swift 依赖。

```sh
cd /Users/yuyi/Documents/Code/DJOneHub/dist/release
shasum -a 256 -c DJOneHub-macOS-arm64-module-names-20261013.zip.sha256
```

### 1.3 安装与启动

结束通话，停止旧进程。已安装启动器时运行 `djonehub stop`；旧版手动运行的 Hub/AudioHost 在各自终端 Ctrl+C，不要同时运行两套。

```sh
cd /Users/yuyi/Documents/Code/DJOneHub/dist/release/DJOneHub-macOS-arm64-module-names-20261013
./install
djonehub start
```

安装目录为 `/usr/local/libexec/djonehub`，命令入口为 `/usr/local/bin/djonehub`。安装可能要求 sudo；**日常启动使用当前登录用户，不加 sudo**，否则 Keychain、权限和数据目录会变成另一套。

`djonehub start` 启动 Hub 和 AudioHost，并自动打开本机远程管理 Web。 本机 `/remote/` 页面无需账号密码，直接访问即自动建立会话；Relay 管理仍需登录。启动终端要保持打开；当前启动器没有安装 launchd 常驻服务，也不会随 Mac 开机自动启动。页面中的“下次启动自动连接”只保存 Relay 配置选择。

```sh
djonehub status
djonehub open
djonehub logs
```

重启：在另一个终端执行 `djonehub stop`，待原终端退出后再执行 `djonehub start`。没有 `djonehub restart` 命令。

首次允许 macOS 麦克风和 Keychain 访问。升级后二进制可能重新触发 Keychain 提示；不要通过删除凭据解决。日志：`~/Library/Logs/DJOneHub/djonehub.log`；本机配置及控制文件：`~/Library/Application Support/DJOneHub`。原生 bootstrap 包含凭据，不要上传到管理网页或分享。

无需系统安装时，在完整发行目录使用 `./djonehub start`。当前是 ad-hoc 签名，尚非 Developer ID 公证发行；从可信来源取得包并核对校验值后，按系统“隐私与安全性”提示允许打开。

### 1.4 可选：源码开发运行

开发环境需自行安装 libusb、pkg-config 和受支持的 Opus（1.5.2 或 1.6.1）；若包管理器提供其他 Opus 版本，使用上面的固定版本发行脚本。

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
DJONEHUB_REMOTE_OPUS=1 sh scripts/build-macos.sh
swift build --package-path macos/DJOneHubAudioHost -c release
```

终端 A：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./dist/djonehub-macos \
  --listen 127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

终端 B，在 Hub 已启动后：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost \
  --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

终端 C，打开本机管理页面：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
./dist/djonehub-macos web-open \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

两进程必须使用同一 Mac 用户和同一控制文件；关闭时分别 Ctrl+C。手动进程不由 `djonehub stop/status` 管理。不要用未开启 `remoteopus` 的构建或 `--demo` 代替真实接听服务。

## 2. 编译、部署与运行 Relay

### 2.1 生成并传输当前源码

Mac 执行：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
python3 scripts/package-relay-apns.py
scp dist/djonehub-relay-apns-source.tar.gz \
  dist/djonehub-relay-apns-source.tar.gz.sha256 \
  root@47.108.184.34:/root/
```

使用已核对主机身份的 SSH 连接；其他登录用户改成可写的家目录。源码包不含 `.p8`、APNs 私有配置、运行数据库或配对凭据。Mac 二进制不能直接上传给 Linux 运行。

服务器 root 终端执行：

```sh
cd /root
sha256sum -c djonehub-relay-apns-source.tar.gz.sha256
DJ_RELAY_WORK=$(mktemp -d /root/djonehub-relay-build.XXXXXX)
tar -xzf /root/djonehub-relay-apns-source.tar.gz -C "$DJ_RELAY_WORK"
cd "$DJ_RELAY_WORK/djonehub-relay-apns"
```

后续服务器命令都在此源码目录执行。每次用新目录解压，避免残留旧源码。

### 2.2 检查 Go/CGO 并编译统一 Relay

已有服务器执行本节；全新 CentOS/RHEL 8 服务器直接按第 2.4 节安装依赖与 Go，安装器会从当前源码编译 Relay。统一模式只需要 `djonehub-relay` 二进制，包含管理页面、身份/信令、TURN 凭据签发及 APNs 发送，不需要单独编译安装 admin。

Relay 使用 SQLite CGO，需要 Go 1.26.3+ 和 GCC；不需要 libopus、libusb 或 Swift。现有安装器的 Go 一般位于 `/opt/djonehub/go/bin/go`；若使用其他路径，修改变量。构建需要能访问 Go 模块源。

```sh
DJ_GO=/opt/djonehub/go/bin/go
env -u GOROOT GOTOOLCHAIN=local "$DJ_GO" version
gcc --version
mkdir -p build
env -u GOROOT GOTOOLCHAIN=local CGO_ENABLED=1 "$DJ_GO" build \
  -trimpath -o build/djonehub-relay ./cmd/djonehub-relay
```

构建成功再继续。旧 `web-management-v2` 二进制没有同进程管理参数，必须先安装新版；本次管理构建标记为 `web-management-v3`，可在部署后的管理概览核对。

### 2.3 已有 Relay：保留配对升级并停用旧 admin

适用当前 `djonehub` 用户、回环 18443、systemd + Nginx 布局。沿用 `/etc/djonehub/apns.json` 和 TURN 配置，不需要再次上传 `.p8`，不运行首次安装器。

先结束通话。旧独立管理进程会占用账号目录并访问身份库，迁移前将其停用；没有这个 unit 时自动跳过：

```sh
if sudo systemctl cat djonehub-relay-admin >/dev/null 2>&1; then
  sudo systemctl disable --now djonehub-relay-admin
fi
```

保留 `/var/lib/djonehub-relay-admin` 中的账号、审计和初始化状态，不删除或重新创建身份库。

```sh
DJ_APNS_WORK=$(sudo mktemp -d /root/djonehub-apns-input.XXXXXX)
sudo install -o root -g root -m 600 /etc/djonehub/apns.json "$DJ_APNS_WORK/apns.json"
sudo bash scripts/upgrade-relay-apns.sh \
  --apns-config "$DJ_APNS_WORK/apns.json" --go-bin "$DJ_GO"
```

升级脚本再次编译并校验 Relay，停服务后备份二进制、APNs 配置、override 和一致 SQLite 快照，再替换/迁移/启动并检查能力。保存打印的 `/var/lib/djonehub-apns-backup.XXXXXX` 路径。临时 APNs 副本必须归 root；最终运行配置归 `djonehub`，不能直接用运行配置绕过属主校验。

脚本已支持保留 `--admin-state-dir`、`--admin-tenant` 和 `--admin-path`，后续升级无需重新启用管理模块。**脚本失败时停止后续步骤。** 停服后的失败会留下服务停止和私有恢复目录；成功后删除临时副本：

```sh
sudo rm -- "$DJ_APNS_WORK/apns.json"
sudo rmdir -- "$DJ_APNS_WORK"
```

已有服务器继续第 2.5 节，不执行第 2.4 节首次安装。新版 Relay 支持 schema 3；同进程管理共用已经打开的身份 Store。旧独立 admin 保持 disabled/inactive 是正常状态。

### 2.4 全新服务器：安装 Relay/coturn 并接入 APNs

此节用于没有现有 DJOneHub 服务的 CentOS/RHEL 8 布局；已有服务器跳过。要求可用的 dnf 源、现有 Nginx、有效 `oops.work` 证书。以实际公网 IP 和证书路径替换示例：

```sh
sudo env -u GOROOT GOTOOLCHAIN=local bash scripts/install-relay-centos8.sh \
  --domain oops.work \
  --public-ip 47.108.184.34 \
  --cert /etc/nginx/conf.d/oops.work.pem \
  --key /etc/nginx/conf.d/oops.work.key
```

脚本安装构建依赖、选择或安装 Go、编译 Relay、创建 `djonehub` 用户和私有目录、生成 TURN secret、复制证书、启动 `djonehub-relay` 和 `djonehub-turn`。不会改现有 Nginx、软件源、防火墙或 SELinux。其他 Linux 发行版不能直接套用该 dnf 脚本，应按自身发行版配置依赖和 coturn，再采用同样的服务参数。

在 `oops.work` **已有 HTTPS server 块内**加入一次：

```nginx
include /etc/nginx/djonehub/relay.locations.conf;
```

该 include 已支持 `/v1/signaling` 的 WebSocket Upgrade；不要替换原站点，也不要重复定义 `/v1/` 和 `/healthz`。检查并重载 Nginx：

```sh
sudo nginx -t
sudo nginx -s reload
```

防火墙和云安全组放行：TCP 443、TCP/UDP 3478、TCP 5349、UDP 49160–49259（需与实际 coturn 范围一致）；不要开放回环 18443/18444。TURN 不通过 HTTP location 反代；若为网站使用 CDN，TURN 主机名仍需直达服务器。Nginx 502 且 SELinux 日志明确阻止连接时，按现有策略处理 `httpd_can_network_connect`，不要关闭 SELinux。

初装脚本没有配置 APNs。首次需通过私有传输将有效的 APNs JSON 放到服务器 root 私有目录，并执行：

```sh
sudo chmod 600 /root/apns.sandbox.json
sudo bash scripts/upgrade-relay-apns.sh \
  --apns-config /root/apns.sandbox.json --go-bin /opt/djonehub/go/bin/go
```

APNs JSON 格式：

```json
{
  "team_id": "8AHAFH9VTZ",
  "key_id": "BA73J355G9",
  "bundle_id": "com.oops.djonehub",
  "environment": "sandbox",
  "private_key_pem": "完整 .p8 PEM 内容，以 JSON 字符串保存"
}
```

`private_key_pem` 是完整私钥内容，**不是文件路径**。原生程序不接受 `p8_path` 字段。已有配置不用重新生成；首次可在 Mac 运行以下脚本，生成新文件且不把密钥打印出来：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
python3 - <<'PY'
import json, os
from pathlib import Path
key = Path('dist/AuthKey_BA73J355G9.p8').read_text()
config = dict(team_id='8AHAFH9VTZ', key_id='BA73J355G9',
              bundle_id='com.oops.djonehub', environment='sandbox',
              private_key_pem=key)
fd = os.open('dist/apns.sandbox.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    json.dump(config, f, indent=2)
    f.write('\n')
print('已生成 dist/apns.sandbox.json（0600）')
PY
```

输出文件存在时脚本拒绝覆盖。私有传输示例：`scp dist/apns.sandbox.json root@47.108.184.34:/root/`。配置含私钥，不提交 Git、不放 Web 下载目录、不交给手机；成功安装后移除服务器输入副本。运行配置最终为 `/etc/djonehub/apns.json`，归 `djonehub`、0600、普通文件。

### 2.5 在统一 Relay 中启用管理模块

已有服务完成第 2.3 节，全新服务完成第 2.4 节后，都执行本节。标准模板同时启用 Mac 申请提交和 `/relay-admin/` 管理页面，使用现有 `home` tenant；定制部署先调整模板中的目录、用户和原生参数。

```sh
sudo install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay-admin
sudo install -d -m 755 /etc/systemd/system/djonehub-relay.service.d
sudo install -m 644 deploy/remote-call/50-integrated-admin.conf.example \
  /etc/systemd/system/djonehub-relay.service.d/50-integrated-admin.conf
sudo systemctl daemon-reload
sudo systemctl restart djonehub-relay
sudo systemctl status djonehub-relay --no-pager -l
```

模板中的完整 ExecStart：

```text
/usr/local/bin/djonehub-relay serve --state-dir /var/lib/djonehub-relay --listen 127.0.0.1:18443 --insecure-loopback --turn-config /etc/djonehub/turn-issuer.json --apns-config /etc/djonehub/apns.json --public-origin https://oops.work --admin-state-dir /var/lib/djonehub-relay-admin --admin-tenant home --admin-path /relay-admin
```

- `--public-origin` 是 HTTPS origin，只填 `https://oops.work`，不能带管理路径；它也启用 Mac 直接提交签名申请。
- `--admin-state-dir` 启用管理模块，沿用原账号/审计目录；必须与身份目录分开。
- `--admin-tenant` 是实际管理范围，必须与既有账号和身份一致。
- `--admin-path /relay-admin` 控制页面、静态资源和 API 前缀；浏览器入口带末尾 `/`。

`50-integrated-admin.conf` 覆盖此前 30/40 的 ExecStart，并保留 APNs/TURN 参数。无需新建或启动独立 `djonehub-relay-admin.service`，也无需开放 18444。

旧 admin 未停止、管理目录权限/账号配置错误，可能使统一 Relay 启动失败。先查看 `sudo journalctl -u djonehub-relay -n 60 --no-pager` 并修复配置；不删除账号，不回退到无密码管理页。

### 2.6 复用现有 Nginx 和证书

安装统一路由模板：

```sh
sudo install -m 644 deploy/remote-call/nginx-relay-integrated-admin.locations.conf \
  /etc/nginx/djonehub/relay-integrated-admin.locations.conf
```

在 **`oops.work` 已有 `listen 443 ssl` 的 HTTPS server 块内**加入一次：

```nginx
include /etc/nginx/djonehub/relay-integrated-admin.locations.conf;
```

保留主站证书、原 `relay.locations.conf` include、`/v1/`、`/healthz` 和其他网站路由。不要创建第二个同名 server；确认主站没有占用 `/relay-admin/`。管理与主站共享浏览器同源，主站内容应由同一可信管理员控制。

模板将 `/relay-admin/` 的完整 URI 转发到 **18443**，核心设置如下：

```nginx
location = /relay-admin {
    return 302 /relay-admin/;
}
location ^~ /relay-admin/ {
    proxy_pass http://127.0.0.1:18443;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    client_max_body_size 32k;
    proxy_connect_timeout 5s;
    proxy_read_timeout 20s;
    proxy_buffering off;
    proxy_cache off;
    proxy_next_upstream off;
    proxy_intercept_errors off;
}
```

`proxy_pass` 不加末尾 `/`，否则路径会被剥离；不要转发到旧 18444。页面资源和管理 API 都在 `/relay-admin/` 下，原生鉴权/信令仍保持 `/v1/`。上述完整 location 已包含在模板里，使用 include 后不要重复粘贴。

```sh
sudo nginx -t
sudo nginx -s reload
curl -I --max-time 10 https://oops.work/relay-admin/
curl -fsS https://oops.work/healthz
```

证书必须有效并覆盖 `oops.work`；无需申请管理子域名证书，也不关闭 HSTS 或跳过校验。自定义 Nginx 安装使用其实际配置目录和 reload 方式。旧 `relay-admin.oops.work` vhost 若仅用于之前的管理页，可备份后移出 Nginx 的 include 范围，以后访问新 URL。

若公网结果不正常，可在服务器绕过 DNS 检查源站，仍校验证书：

```sh
curl -I --max-time 10 \
  --resolve oops.work:443:127.0.0.1 \
  https://oops.work/relay-admin/
```

### 2.7 管理员初始化与登录

已初始化管理员直接在 `https://oops.work/relay-admin/` 使用原账号密码登录；原内存会话在停服时失效，不需要重建账号。

尚未初始化时，统一 Relay 在 `/var/lib/djonehub-relay-admin/setup.json` 生成约 30 分钟有效的 token（djonehub 所有，0600）。仅在服务器私有终端读取：

```sh
sudo -u djonehub cat /var/lib/djonehub-relay-admin/setup.json
```

在新页面填写 token、自定账号和至少 12 字节密码。不要分享文件或输出；初始化后 token 失效并删除。未初始化且 token 过期时，在没有通话时重启 **`djonehub-relay`** 生成新凭据。后续使用账号密码，不使用手机身份 token。

### 2.8 运行检查、重启、备份与回退

```sh
sudo systemctl is-active djonehub-relay djonehub-turn
curl -fsS http://127.0.0.1:18443/healthz
curl -fsS https://oops.work/healthz
curl -I --max-time 10 https://oops.work/relay-admin/
sudo journalctl -u djonehub-relay -n 80 --no-pager
```

healthz 应有 `status=ok`，`identity/signaling/outgoing_calls/turn/voip_push=true`。这证明能力/配置已加载，不证明推送送达或媒体已连通。登录管理页查看构建 `web-management-v3`、数据库、WSS、APNs/TURN 最近结果。统一管理直接读取同进程观测，不依赖状态文件；`runtime-status.json` 仍保留供兼容工具使用。计数在 Relay 重启后归零。

日常重启（先结束通话）：

```sh
sudo systemctl restart djonehub-relay
```

管理代码也由此重启；旧独立 admin 保持停用。只有 TURN 配置、证书或其本身故障需要时，才重启 `djonehub-turn`。证书续期使用现有证书更新流程（安装器提供 `/usr/local/sbin/djonehub-refresh-turn-cert`），并重启 TURN 使新证书生效。

备份包括 Relay 二进制、systemd drop-in、APNs/TURN 配置、身份数据库一致快照、管理账户/审计目录和 Nginx/证书配置。停止统一 Relay，并确认没有旧独立 admin 或其他写库进程，再备份 SQLite；不能在运行时只复制主文件而遗漏 WAL。

只回退管理拓扑时保留新版二进制和数据库，按[统一服务回退步骤](RELAY_INTEGRATED_ADMIN_DEPLOY.md#6-回退统一入口)恢复独立管理方案。schema 3 库不能直接用旧 schema 2 二进制启动；整体回退必须恢复配套二进制、配置和一致快照。若已消费授权或轮换客户端凭据，回退旧库可能需要恢复身份。

独立管理服务的兼容部署另见[旧模式说明](REMOTE_RELAY_WEB_ADMIN_DEPLOY.md)，不与本文统一模式同时启用同一管理目录。

## 3. 编译与安装 iPhone App

### 3.1 打开工程并配置签名

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
open ios/DJOneHubPhone/DJOneHubPhone.xcodeproj
```

仓库已包含生成好的工程，不必每次运行 XcodeGen。若修改 `project.yml`，再运行 `xcodegen generate --spec ios/DJOneHubPhone/project.yml`；Team/Bundle 的持久修改也要同步到 YAML，避免再次生成时丢失。

Xcode 中：

1. 登录有权限的 Apple Developer 账号；选择 `DJOneHubPhone` target 和 scheme。
2. Signing & Capabilities 启用自动签名，Team `8AHAFH9VTZ`、Bundle `com.oops.djonehub`。Apple App ID/provisioning profile 必须允许 Push Notifications。
3. 确认 Background Modes 包含 Audio 和 Voice over IP；工程已有 `aps-environment` entitlement，不要添加虚构的 CallKit entitlement。
4. USB 连接 iPhone，解锁并信任 Mac；在 iPhone“设置 → 隐私与安全性 → 开发者模式”启用并按提示重启确认。参见 [Apple 开发者模式说明](https://developer.apple.com/documentation/xcode/enabling-developer-mode-on-a-device)。
5. 选择真实 iPhone 作为运行目标。模拟器或无签名构建不能作为本方案后台来电验收。

已有手机升级保持 Team、Bundle ID，不卸载，不先清理配对身份，直接覆盖安装。

### 3.2 当前环境：Debug + sandbox

Run 的 Build Configuration 保持 **Debug**，按 ⌘R 编译并安装。Swift Package Manager 会读取本地 `shared/DJOneHubPhoneCore` 和固定版本 WebRTC `154.0.0`，需要首次解析依赖的网络。App 使用 WebRTC 自带 Opus，不需给 iOS 工程额外链接 Mac 的 libopus。

也可命令行编译安装（需要已配置好的签名/设备注册）：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
xcrun devicectl list devices
xcodebuild -showdestinations \
  -project ios/DJOneHubPhone/DJOneHubPhone.xcodeproj -scheme DJOneHubPhone
DJ_IPHONE_UDID='替换为真实 iPhone 的 UDID'
xcodebuild \
  -project ios/DJOneHubPhone/DJOneHubPhone.xcodeproj \
  -scheme DJOneHubPhone -configuration Debug \
  -destination "id=$DJ_IPHONE_UDID" \
  -derivedDataPath dist/ios-debug \
  -allowProvisioningUpdates build
xcrun devicectl device install app --device "$DJ_IPHONE_UDID" \
  dist/ios-debug/Build/Products/Debug-iphoneos/DJOneHubPhone.app
```

UDID 可从 Xcode“Devices and Simulators”取得，构建目标也可使用 `-showdestinations` 返回的真实 iPhone ID；不要误选模拟器或把 DeviceCtl 列表中的另一种 Identifier 当成 UDID。安装后在 iPhone 手动打开 App，允许麦克风/扫码相机权限。

### 3.3 Release / TestFlight / 正式分发

| 配置 | App 标记 | 签名 entitlement | Relay APNs 环境 |
| --- | --- | --- | --- |
| 当前 Debug | `sandbox` | `development` | `sandbox` |
| Release | `production` | `production` | `production` |

不要只将 App 改为 Release 而让 Relay 继续发送 sandbox 推送。当前 Relay 每个进程只配置一个 APNs environment，不同时服务两种推送环境；并行保留 Debug 和生产测试时应规划独立测试 Relay。生产切换可复用同一有效 `.p8`，但需生成 production JSON、重新校验/部署并让 App 登记对应推送 token。

归档：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
xcodebuild \
  -project ios/DJOneHubPhone/DJOneHubPhone.xcodeproj \
  -scheme DJOneHubPhone -configuration Release \
  -destination 'generic/platform=iOS' \
  -archivePath dist/ios-release/DJOneHubPhone.xcarchive \
  -allowProvisioningUpdates archive
```

成功后从 Xcode Organizer 打开归档，选择 Distribute App，再按账号和用途选择 TestFlight/App Store 或注册设备分发。Archive 不等于已经得到可任意安装的 IPA；导出依赖有效分发证书、profile 和设备/分发方式。上传新构建前增加 build number。参见 [Apple 分发说明](https://developer.apple.com/documentation/xcode/distributing-your-app-for-beta-testing-and-releases)。本项目目前尚未完成 production APNs 与正式分发验收。

## 4. 编译安装后的 Web 配对与目标选择

### 已有 `home-xiaomi` 和已配对手机

1. `djonehub start` → `/remote/` → “导入已经保存在钥匙串中的旧配置”，输入 `home-xiaomi`，读取并加入列表。
2. 选择该配置，保存“下次启动自动连接”，连接到 `online`。在配置卡片将 Relay 管理地址保存为完整的 `https://oops.work/relay-admin/`；仅填 origin 的旧值会沿用 `/admin/` 兼容规则。
3. 读取手机列表，确认原 iPhone/模块范围；普通升级无需重新扫码。
4. iPhone 打开 App，确认旧配对存在；麦克风已授权时自动连接，不再每次点击连接按钮。首次在设置允许麦克风，或启用“后台来电”时授予权限；核对后台来电的推送已注册状态。

### 全新 Mac / iPhone

1. Mac 根页面选择实际 USB 模块，完成模块初始化、语音运行时准备和音频就绪；这些硬件操作须在页面明确执行。
2. Mac `/remote/` 创建签名申请，原生 Relay 填 `https://oops.work`，管理地址填完整的 `https://oops.work/relay-admin/`，勾选页面已有模块并提交。
3. Relay 管理页核对申请编号/公钥指纹，批准模块或子集，签发并下载绑定授权 JSON。
4. Mac 对应申请导入该 JSON，核对预览并确认登记；授权文件最长十分钟，登记成功后连接并保存重启选择。
5. Mac 生成 iPhone 五分钟配对 QR；iPhone 扫码、核对并确认。无法扫码可下载配对 JSON，私有传输后从 App 导入。手机只接收配对 QR/JSON，不接收 Mac 授权文件或 APNs 配置。
6. 在 Mac“各模块的接听目标”或原模块“来电”页，从已授权手机下拉中选择目标并保存。扫码不自动启用路由。
7. 对要使用 iPhone 的模块明确关闭 AI 接听并保存；**AI 启用时仅走 AI，不通知远程手机，AI 失败也不自动转手机**。
8. iPhone 在设置允许麦克风后自动连接；启用“后台来电”也会请求该权限。已配对、权限就绪后，打开或返回 App 前台即自动连接；后台接听仍需启用并完成推送注册。完成后删除授权/配对交付副本。

详细 UI 说明见[Web 操作指南](REMOTE_WEB_OPERATIONS_GUIDE.md)及[iPhone 自动连接说明](remote-iphone-auto-connect.md)。手机 WSS 未连接仍可能由 APNs 唤醒；“推送已登记”不等于手机在线，也不等于该次通知已经送达。

## 5. 上线检查与常见问题

部署通过至少核对：Mac 模块/音频宿主就绪、Hub online、手机授权与目标正确、推送已登记；前台来电双向说话各 30 秒；手机与主叫分别挂断；第二通接听；App 正常退后台并锁屏后的来电、接听和双向音频；AI 启用时远程不响。

| 现象 | 核对 |
| --- | --- |
| Mac 7575 端口占用 | 旧手动进程/另一份启动器；先停止旧 Hub 与宿主，不同时运行 |
| Mac 更新后启动卡住 | Keychain 访问弹窗、麦克风权限和日志；允许系统访问，不删除旧身份 |
| 手机列表在线状态未知 | 确认已运行新版统一 Relay、Hub/手机实际连接和查询是否成功；Mac 查询凭据过期时重新读取列表。统一管理不需要检查跨进程快照文件 |
| 手机线路仍显示模块 ID | 确认三端均为备注同步版、Hub online 和手机已连接；Mac 设置备注后等待约 30 秒；用 `/healthz` 的 `capabilities.module_names=true` 核对 Relay |
| 手机已配对但不响 | AI 开关、模块手机目标、Hub online、后台来电登记、Debug/sandbox 匹配 |
| 有信令但没有声音 | AudioHost 实际注册、模块 UAC/语音就绪、Opus 构建、TURN 端口/候选、手机麦克风权限 |
| 管理页面 502 | 检查 Relay 的 18443 监听、`djonehub-relay` 日志、Nginx/SELinux；确认没有仍转发到旧 18444 |
| Relay `203/EXEC` | systemd 未能执行 `/usr/local/bin/djonehub-relay`；检查安装路径、755 权限、Linux 架构、ELF 动态解释器和 SELinux 标签 |
| Relay 报不识别 admin 参数 | 仍是旧二进制；先从最新源码包构建并升级，再安装 50 drop-in |
| Relay 报管理目录不可用 | 旧独立 admin 是否仍运行、目录是否 djonehub/0700、私有文件是否同用户/0600；保留原账号并修复权限或配置 |
| 证书域名不匹配 | 访问 `https://oops.work/relay-admin/`，核对主站加载的证书；单域名 `oops.work` 证书不覆盖旧管理子域名 |
| 旧 Relay 在迁移后启动失败 | schema 不兼容；恢复配套版本，不直接拿旧二进制打开新库 |

恢复、TURN 续租、DTMF 等专项开关默认保持关闭；发行启动器不透传任意参数。要验证时按[专项开关说明](REMOTE_OPERATIONS_GUIDE.md#9-实验功能开关与日常启动分开)手动运行同版本 Hub/AudioHost，并同步升级 Relay/iPhone。普通部署不把尚未验收的恢复能力默认开启。
