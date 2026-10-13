# Relay、APNs 与 /relay-admin/ 统一部署

2026-10-13，用户确认入口为 **`https://oops.work/relay-admin/`**，沿用 `oops.work` 现有证书。管理模块与身份、信令、TURN 凭据签发、APNs 发送合并到一个 `djonehub-relay` 进程，监听原来的回环 `18443`。coturn 媒体中继和 Nginx 仍是各自的服务。

当前源码已实现；下文由部署者在服务器执行，不代表已经自动升级公网。本迁移保留现有身份库、APNs/TURN 配置与 `/var/lib/djonehub-relay-admin` 管理账号/审计。旧独立管理进程必须停用，不能同时占用同一个账号目录。

## 1. 传输最新源码并编译

Mac：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
python3 scripts/package-relay-apns.py
scp dist/djonehub-relay-apns-source.tar.gz \
  dist/djonehub-relay-apns-source.tar.gz.sha256 \
  root@47.108.184.34:/root/
```

服务器：

```sh
cd /root
sha256sum -c djonehub-relay-apns-source.tar.gz.sha256
DJ_RELAY_WORK=$(mktemp -d /root/djonehub-relay-integrated.XXXXXX)
tar -xzf /root/djonehub-relay-apns-source.tar.gz -C "$DJ_RELAY_WORK"
cd "$DJ_RELAY_WORK/djonehub-relay-apns"
DJ_GO=/opt/djonehub/go/bin/go
env -u GOROOT GOTOOLCHAIN=local "$DJ_GO" version
mkdir -p build
env -u GOROOT GOTOOLCHAIN=local CGO_ENABLED=1 "$DJ_GO" build \
  -trimpath -o build/djonehub-relay ./cmd/djonehub-relay
```

Go 至少 1.26.3；路径按实际修改，需要 GCC/CGO。只需构建 native Relay，统一模式不要求安装 `djonehub-relay-admin` 二进制。命令成功后再继续，已有服务此时仍在运行。

## 2. 空闲时升级，保留凭据

结束所有电话，先停用此前的独立管理服务：

```sh
sudo systemctl disable --now djonehub-relay-admin
```

若该 unit 从未安装，跳过这一条。**不删除**管理账户目录或身份库。

在同一源码目录、同一终端执行现有安全升级流程；它会再次编译、校验 APNs，停 Relay 并备份一致的身份库后替换二进制。当前标准 systemd/Nginx 布局适用，定制布局不会被强制覆盖：

```sh
DJ_APNS_WORK=$(sudo mktemp -d /root/djonehub-apns-input.XXXXXX)
sudo install -o root -g root -m 600 /etc/djonehub/apns.json "$DJ_APNS_WORK/apns.json"
sudo bash scripts/upgrade-relay-apns.sh \
  --apns-config "$DJ_APNS_WORK/apns.json" --go-bin "$DJ_GO"
```

失败时停止后续步骤，按脚本打印的备份目录排查；不要拿旧二进制打开迁移后的库。成功后保留备份目录，并清理临时私有副本：

```sh
sudo rm -- "$DJ_APNS_WORK/apns.json"
sudo rmdir -- "$DJ_APNS_WORK"
```

后续升级工具已支持保留 `--admin-state-dir`、`--admin-tenant`、`--admin-path`，不会悄悄关闭统一管理模块。

## 3. 在现有 Relay 上启用管理模块

模板适用于本线程的 `djonehub` 用户、`home` tenant 和已有 APNs/TURN 配置。若实际 tenant/目录不同先编辑模板，保留实际值；`--public-origin` 必须是 `https://oops.work`，不能带 `/relay-admin/` 路径。

```sh
sudo install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay-admin
sudo install -d -m 755 /etc/systemd/system/djonehub-relay.service.d
sudo install -m 644 deploy/remote-call/50-integrated-admin.conf.example \
  /etc/systemd/system/djonehub-relay.service.d/50-integrated-admin.conf
sudo systemctl daemon-reload
sudo systemctl restart djonehub-relay
sudo systemctl status djonehub-relay --no-pager -l
```

生效后的 ExecStart 为：

```text
/usr/local/bin/djonehub-relay serve --state-dir /var/lib/djonehub-relay --listen 127.0.0.1:18443 --insecure-loopback --turn-config /etc/djonehub/turn-issuer.json --apns-config /etc/djonehub/apns.json --public-origin https://oops.work --admin-state-dir /var/lib/djonehub-relay-admin --admin-tenant home --admin-path /relay-admin
```

`50-integrated-admin.conf` 覆盖此前的 30/40 ExecStart，完整保留 native 所需参数。不要直接启动旧的 `web-management-v2` Relay 并传入新参数。

管理文件权限/tenant/origin 错误，或旧独立 admin 仍占用管理目录时，统一 Relay 会启动失败并给出错误；不会回退到无鉴权管理页。先修复配置，不清理账号文件。诊断：

```sh
sudo journalctl -u djonehub-relay -n 60 --no-pager
curl -fsS http://127.0.0.1:18443/healthz
```

## 4. 复用 oops.work 的 Nginx 和证书

安装路由模板：

```sh
sudo install -m 644 deploy/remote-call/nginx-relay-integrated-admin.locations.conf \
  /etc/nginx/djonehub/relay-integrated-admin.locations.conf
```

在 **`oops.work` 现有 `listen 443 ssl` 的 HTTPS server 块内**添加一次：

```nginx
include /etc/nginx/djonehub/relay-integrated-admin.locations.conf;
```

保留原证书、原 `relay.locations.conf` include、`/v1/`、`/healthz` 和网站其他路由；先确认原网站没有使用 `/relay-admin/`。新增 include 只含：

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

`proxy_pass` 不加末尾 `/`，保留 `/relay-admin/` 完整路径；不要转发到旧的 18444。管理 API 与页面资源都在 `/relay-admin/` 下，不占用主站 `/admin/`。

```sh
sudo nginx -t
sudo nginx -s reload
curl -I --max-time 10 https://oops.work/relay-admin/
curl -fsS https://oops.work/healthz
```

证书仍须有效并覆盖 `oops.work`，不跳过校验。无需 `relay-admin.oops.work` 的 DNS 或证书；以后访问新 URL。原独立子域名 vhost 若仅用于旧管理页，可备份后移出 Nginx 的 include 范围，不改主站证书或关闭 HSTS。

如域名经外部代理且公网结果与本机不同，先检查源站：

```sh
curl -I --max-time 10 \
  --resolve oops.work:443:127.0.0.1 \
  https://oops.work/relay-admin/
```

## 5. 登录、Mac 管理跳转和运行检查

- 已初始化管理员：原账号密码在新地址继续使用；内存会话在停服时失效，需要重新登录。
- 尚未初始化：在服务器私有终端读取 `/var/lib/djonehub-relay-admin/setup.json`，在新地址设置账号密码。token 不贴到聊天或 URL；未初始化且 token 过期时重启统一 Relay 生成新的初始化凭据。
- Mac 最新 Web 支持保存完整管理地址 **`https://oops.work/relay-admin/`**，点击后直接打开，不再追加 `/admin/`。只保存 origin 的旧值仍按旧 `/admin/` 规则兼容；请明确更新地址。此修改不改变手机配对的原生 Relay 地址，既有 iPhone 无需因管理 URL 改动重新安装或配对。
- 管理“服务与权限”直接读取同进程信令/数据库/APNs/TURN 观测，不依赖状态文件；私有 `runtime-status.json` 仍保留供兼容工具使用。
- `healthz` 证明配置/能力已加载，不证明推送送达或通话媒体已连通。上线仍需一通前台和一通锁屏后台双向通话验证。

日常只重启统一 Relay：

```sh
sudo systemctl restart djonehub-relay
sudo systemctl is-active djonehub-relay djonehub-turn
```

独立 `djonehub-relay-admin` 保持 disabled/inactive 是正常状态。coturn 仅在自身配置、证书或故障需要时重启。管理密码/审计故障在统一服务中会影响启动，因此管理目录也纳入私有备份。

## 6. 回退统一入口

若仅要恢复独立管理进程而保留新版 Relay：先结束通话，移出 `50-integrated-admin.conf`（保留备份，确认原有 30/40 native 参数仍完整），daemon-reload 并重启 native Relay 释放账号目录，再按旧 service 的 public-origin/base-path 和对应 Nginx 路由恢复独立 admin。同一目录不能同时被两个管理模块打开。

不因管理路径改动恢复旧数据库快照。若确需二进制/数据库整体回退，停止所有访问身份库的进程并按一致备份恢复；回退可能使已轮换客户端凭据失效，先核对实际身份状态。
