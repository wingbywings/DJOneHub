# Relay Web 授权管理：当前实现与部署

Mac、Relay 和 iPhone 的完整构建与启动步骤见[三端部署指南](DEPLOYMENT_GUIDE.md)。

**用户最新选择：`https://oops.work/relay-admin/`，与 native Relay/APNs 合并为同进程。请优先使用[统一服务迁移指南](RELAY_INTEGRATED_ADMIN_DEPLOY.md)。** 下文独立 18444 和 `/admin/` 方案属于兼容部署，不是此次首选。无需为首选方案新增证书。

2026-10-13，Web 配对方案 W01/W02 首批。代码已实现并自动化验证；**尚未部署公网**。
Mac Web 注册向导、profile 动态连接、手机 QR/管理迁移已在第二批接入，见[Web 操作指南](REMOTE_WEB_OPERATIONS_GUIDE.md)；临时身份配对浏览器闭环已通过；新增运行展示的临时身份浏览器验收也已通过，实际升级仍未验收。

## 1. 服务边界

- 现有 Relay：`https://oops.work` → 回环 `18443`；原生身份/WSS/APNs/TURN 接口不变。
- 新管理程序：`djonehub-relay-admin`，只绑定回环 `18444`。
- 新管理站点建议 `https://relay-admin.oops.work/admin/`，需要你配置独立 DNS 和该域名证书；该站点目前尚不存在。
- 独立管理账户/session/audit，与手机/Hub Bearer 身份分离，不向网页返回长期 access/refresh token、APNs token 或私钥。账户为私有 `account.json`，session 仅驻留内存、重启失效，审计持久化到私有 `audit.sqlite`。
- 管理服务复用 identity 数据库，schema 从 2 升级到 3，保留旧 Hub/手机/访问凭据。新增授权绑定及管理元数据，旧创建时间显示未知。

**必须先升级 Relay，再启动管理服务。** 旧二进制不支持 schema 3；升级时结束通话、保留一致数据库备份。不要在旧 Relay 仍运行时让新管理程序首先迁移它的数据库。

## 2. 构建与升级

使用当前工作区打包脚本生成源码包，包已加入管理程序源码和本指南：

```sh
cd /Users/yuyi/Documents/Code/DJOneHub
python3 scripts/package-relay-apns.py
```

按现有[操作指南](REMOTE_OPERATIONS_GUIDE.md#82-升级到当前工作区-relay)传输源码并执行 Relay 升级。
新版 APNs 升级工具支持 schema 3，并在后续升级时保留已配置的 `--public-origin`。
目前无需向服务器重新上传 `.p8`。

在服务器解压源码目录编译新管理服务，使用安装器已有 Go/CGO 环境，例如：

```sh
CGO_ENABLED=1 /opt/djonehub/go/bin/go build -trimpath -o /tmp/djonehub-relay-admin ./cmd/djonehub-relay-admin
sudo install -m 755 /tmp/djonehub-relay-admin /usr/local/bin/djonehub-relay-admin
if command -v restorecon >/dev/null 2>&1; then
  sudo restorecon -v /usr/local/bin/djonehub-relay-admin
fi
sudo -u djonehub /usr/local/bin/djonehub-relay-admin --version
sudo install -d -o djonehub -g djonehub -m 700 /var/lib/djonehub-relay-admin
sudo install -m 644 deploy/remote-call/djonehub-relay-admin.service.example /etc/systemd/system/djonehub-relay-admin.service
```

先核对 service 的管理域名、原生 Relay origin、tenant、目录和用户。模板沿用当前 `home` tenant；若现有身份属于其他 tenant，使用实际值，不能随意改来绕过权限。

`203/EXEC` 表示 systemd 未能执行程序，先确认二进制已安装到 ExecStart 指定位置，再检查 Linux 架构、执行权限、动态解释器及 SELinux 文件标签；不要先修改身份库、APNs 或管理密码。构建到 `/tmp` 或 `build/` 后仍需安装，首次 Relay/coturn 安装器不会安装 admin。

## 3. HTTPS 与首次初始化

### 复用主站域名与证书（无需新证书）

如果现有证书只覆盖 `oops.work`，可以将管理入口改为 `https://oops.work/admin/`，无需创建管理子域名或新增证书。当前程序支持管理 origin 与原生 Relay origin 相同；原生 `/v1/`、`/healthz` 仍转发 18443，只有 `/admin/` 转发 18444。确认原站点没有占用 `/admin/`；此方案与主站共享浏览器同源，主站内容也应由同一可信管理员控制。

1. 修改管理 service 的 `--public-origin` 为 `https://oops.work`（不能带 `/admin/`）；`--relay-origin`、用户、tenant、两种状态目录保持不变。已有账户和身份数据不需要删除。
2. 将 `deploy/remote-call/nginx-relay-admin.locations.conf` 安装到 `/etc/nginx/djonehub/relay-admin.locations.conf`，在**已有 `oops.work` HTTPS server 内**添加 `include /etc/nginx/djonehub/relay-admin.locations.conf;`。保留其原证书和其他路由，不能把这个 include 放到 `http` 顶层。
3. `sudo nginx -t` 成功后 reload Nginx；`sudo systemctl daemon-reload` 后重启 `djonehub-relay-admin`。管理会话失效，需要重新登录；不必重启原生 Relay/coturn。
4. 打开 `https://oops.work/admin/`；Mac Web 中保存的“Relay 管理地址”也改为 `https://oops.work`。已初始化账号直接登录；尚未初始化使用当前有效 setup token。

`proxy_pass http://127.0.0.1:18444;` **不要加末尾 `/`**，以保留后端要求的 `/admin/` 路径。原 `relay-admin.oops.work` vhost 不再作为管理入口，确认未供其他用途后可移出 Nginx 的 include 范围。

单域名 `oops.work` 证书不能直接用于 `relay-admin.oops.work`；只有证书 SAN 明确含该子域名，或有匹配的 `*.oops.work`，才能继续使用子域名方案。

### 独立管理域名

配置新域名 DNS 和有效证书，复制并调整 `nginx-relay-admin.conf.example` 到实际 Nginx 配置目录。
不要替换当前主站 vhost。`ssl_certificate` 与 key 的 REPLACE 路径必须修改。
管理后端只监听回环，无需开放 18444 到公网。

```sh
sudo nginx -t
sudo systemctl reload nginx
sudo systemctl daemon-reload
sudo systemctl enable --now djonehub-relay-admin
sudo systemctl status djonehub-relay-admin --no-pager
```

若主机使用非 systemd 的 Nginx 管理方式，使用现有管理方式 reload；不重装 Nginx。

首次启动在 `/var/lib/djonehub-relay-admin/setup.json` 生成 30 分钟有效的一次性初始化凭据（djonehub 所有、0600）。
部署者在服务器私有终端读取该文件的 `token`，打开管理 HTTPS 页，填写初始化凭据、账号和至少 12 字节密码。
此凭据不在日志/URL中，不在公开下载接口中；初始化后删除并失效。处理完成后不要分享它。
密码保存为随机 salt 的 PBKDF2-HMAC-SHA256 校验值；Session 为短时 HttpOnly/Secure/SameSite Cookie，写操作带 CSRF。
过期且未初始化时重启管理服务生成新初始化凭据；配置文件错误不会回退为无密码状态。

## 4. 注册申请与授权文件

管理页可接收 Mac 签名申请 JSON；模块 ID来自申请候选，管理员勾选范围，不能手填任意 ID。
申请的公钥与模块信息来自该 Mac 声明，应核对申请编号和公钥指纹后批准。

1. 导入申请，或查看已由 Mac 提交的待审核申请。
2. 勾选所需模块，批准；只能批准申请中的模块或其子集。
3. 点击“签发 / 重新签发并下载”，在页面内确认后下载授权文件，绑定申请及 Mac 公钥；有效期最多 10 分钟且不超过申请期限。
4. 再次下载将重新签发，使旧未消费文件失效；消费成功后只显示结果，不能重复签发。
5. 可拒绝/撤销未完成申请；已登记身份需在身份列表撤销。

Mac Web 已支持创建/提交公有申请，并在对应申请中预览、导入版本 2 的绑定授权文件，调用 `BeginEnrollment/CompleteEnrollment` 完成 Keychain 登记。日常流程不要求 CLI；旧 CLI grant 继续属于兼容路径，不能导入新绑定向导。

若要启用 Mac 直接提交公开注册申请，给现有 Relay serve 命令增加：

```text
--public-origin https://oops.work
```

通过 systemd drop-in 修改时保留原 ExecStart 中的 state-dir、listen、insecure-loopback、TURN 和 APNs 参数。
默认不配置此参数时，申请提交 endpoint 不开启；管理页仍可导入签名申请文件。
该参数不能指定为管理站点域名，因为申请和手机配对仍连接原生 Relay origin。

## 5. 设备与审计

管理页列出当前 tenant 的 Mac/iPhone、所属 Hub、模块范围、备注、有效/撤销状态、真实登记/认证时间和推送注册状态。
推送注册与实际 WSS 连接分开展示。新 Relay 每秒在 `state-dir/runtime-status.json` 写入原子私有运行快照，管理页每五秒读取；两个进程必须使用同一服务用户、同一身份目录。文件或目录不安全、快照缺失或超过八秒时显示未知。旧记录没有时间时显示未知，不虚构。

“服务与权限”展示 Relay 进程、数据库实际查询、管理范围 WSS 连接数、TURN/APNs 配置加载和最近请求结果。快照不包含密钥、访问 token、APNs device token、电话号码或通话 UUID；不向公开 `/healthz` 暴露身份清单。TURN 签发／APNs 接受不等于端到端成功。状态写入失败只使观测降级，不阻止 Relay 核心服务启动。身份 ID／指纹可复制，所属 Hub 可点击跳转。
撤销 Hub 会影响子手机；独立管理进程修改 SQLite 后，既有 Relay 一秒周期的撤销检测会使 WSS及时观察变化，电话继续有界收尾。

“已注册 Mac 与已配对 iPhone”中的已撤销条目支持“删除记录”。有效身份不能直接删除；删除 Mac 时清理其所属已失效 iPhone，删除单个手机不影响所属 Mac 或其他手机。确认后在同一身份事务中清理设备记录、访问/刷新/挑战凭据和推送地址；相关授权 secret 清理，审批/邀请元数据及管理审计保留，已消费授权不能重新使用。此操作不会删除 Mac Keychain、手机配对或本机模块目标；这些仍需在对应设备处理。独立与集成管理模式均支持，界面和 API 使用同一管理范围与 CSRF/审计保护。详见[删除功能记录](remote-relay-delete-revoked-batch.md)。

写操作先保存审计意图再修改身份数据；审计不可用时不开始操作。
若完成后结果审计失败，返回“结果待核对”，刷新对象查看，不把未知结果报告为成功。
授权 secret 不写审计、URL或永久下载路径，下载返回 no-store/attachment。

申请各状态可“删除申请及授权”，同时清理其全部授权；最新授权可单独删除。未消费下载副本失效，已登记设备继续有效。管理操作记录可“清除已完成记录”：清除当前 tenant 全部成功/失败记录，保留 pending 和本次清除记录；这些清理操作不改变设备状态，本地下载文件需自行删除。说明与验证见[管理清理记录](remote-relay-management-cleanup-batch.md)。

## 6. 兼容与回退

保持现有 Keychain/手机身份。管理进程账号及审计位于独立 admin-state-dir，不覆盖 Relay APNs/TURN 配置。
关闭管理服务不会停止原生 Relay。

数据库一旦升级到 schema 3，不直接启动旧 schema 2 二进制；如需回退，按 Relay 升级工具的一致备份流程恢复二进制、配置和数据库。
若升级后已刷新客户端凭据，回退数据库可能导致凭据不确定，需要专门恢复，不能只复制旧 token。

本阶段未更改运行中的 Mac、手机或公网服务。部署与实际管理员初始化由环境准备后进行；自动化与页面预览仅使用临时数据库和测试账号。
