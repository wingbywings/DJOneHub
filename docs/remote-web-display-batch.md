# Web 管理展示补齐记录

2026-10-13，分支 `codex/remote-iphone-callkit`，对应用户要求补齐管理展示。仅修改工作区与待安装包，未操作真实 Keychain、APNs 私钥、Mac 服务或公网 Relay。

## 实现

- 原生 Relay 提供真实进程内 WSS 连接观测，在鉴权管理安装列表中附加采样状态／时间；已撤销身份不报告在线，Hub 列表范围仍由后端鉴权。
- Relay 每秒发布 `runtime-status.json` 原子私有快照：启动／构建、数据库查询、WSS、TURN 签发与 APNs 请求结果。0600、同所有者、无 symlink／hardlink、限定大小，使用独占发布锁；退出写停止状态，八秒过期降级未知。写入故障不阻断核心服务。
- 独立管理服务从同用户私有目录读取快照；身份清单与连接数按 tenant 过滤。管理 Web 显示状态、采样与最近请求结果，公开健康接口不增加身份信息；不采集电话、UUID、APNs 地址、TURN／身份凭据。
- Mac 明确读取手机列表后，以内存短期 access 凭据每五秒更新观测，单次查询有三秒预算；轮询和 GET 缓存不访问 Vault，不后台续凭据。临时网络失败显示未知并继续重试，到期提示重新读取；更换观察配置和关服务取消旧任务。
- UI 新鲜度以本地接收时间计算，防止浏览器／Relay 时钟差异误判；采样绝对时间独立展示。查询失败及过期不保留在线结论。手机推送登记、WSS 连接、授权三者分别表示。
- Mac 显示 Hub ID，提供 Hub／手机 ID、公钥指纹、申请编号复制，浏览器无法自动复制时提供可选择文本。可保存经 HTTPS origin 校验的独立 Relay 管理地址，并打开 `/admin/`。
- 各模块提供到原来电设置的精确 ID 跳转；指定设备缺失或设备清单失败时不清空为默认模块。Relay 身份行提供所属 Hub 跳转及 ID／指纹复制。

## 验证

全仓库 `go test -race -tags remoteopus ./...` 通过；新增 APNs/TURN 实际请求观测回归通过。真实临时 TLS/WSS 覆盖上线、断开和撤销、跨 Hub；管理 API 覆盖跨 tenant、过期、退出、无鉴权拒绝及不返回凭据；私有快照覆盖权限、symlink、发布互斥和停止。Observer 验证读取后封禁 Vault 仍可轮询，且到期不续凭据；Mac GET 缓存跨配置与过期隔离通过。

`test-remote-display.mjs` 覆盖过期／未知、时钟偏差、推送与 WSS 分离、健康结果语义，以及指定模块缺失／清单失败不切换目标。原模块异步保存与管理提示脚本回归通过。

用户解锁后已完成新增展示的浏览器点击验收，全部使用临时身份、SQLite 和真实本机 TLS/WSS：

- Relay 显示两条 WSS、数据库正常、测试环境 APNs/TURN 未加载；与真实测试连接一致。
- 核对手机 ID、Hub ID、公钥指纹复制后的剪贴板内容；手机“所属 Hub”链接准确定位并高亮对应 Mac 行。
- Mac 显示手机 WSS 在线／未登记推送，五秒观测更新；module_a 手机下拉显示在线，module_b 不显示未经该模块授权的手机。
- 管理地址保存后生成准确的 HTTPS `/admin/` 外链；这里只验证地址持久化及链接目标，示例站点未部署，未访问或绕过证书提示。
- 从模块卡片实际跳转到 `/?device=module_b#calls`，页面打开来电页，读取选择框真实值确认是 module_b，即使两个模块同名也不串选。
- 在 Mac 断开临时 Hub 后，Mac 显示 disabled 并保留重启选择，Relay 显示 Hub 未连接、手机仍在线、WSS 数由 2 降为 1。

验收发现并修复手机下拉缺少本地接收时间而误显示未知的问题，新增直接回归；扩大换行 Hub 链接的整块点击区域，修复后语义点击与目标高亮均通过。同步更新读取列表提示，明确区分 WSS 与推送。

截图位于 `dist/screenshots/web-management-20261013/`。之前的配对浏览器闭环见[验收记录](remote-web-browser-acceptance.md)。真实 iPhone 前台／后台通话及公网管理域名部署仍待实际环境验收。

## 交付及范围

Mac 包：`dist/release/DJOneHub-macOS-arm64-web-management-20261013.zip`；Relay 源码包：`dist/djonehub-relay-apns-source.tar.gz`。操作方式见[Web 指南](REMOTE_WEB_OPERATIONS_GUIDE.md)与[Relay 部署指南](REMOTE_RELAY_WEB_ADMIN_DEPLOY.md)。先升级原生 Relay 与管理服务，再升级 Mac；没有自动部署现有服务。

此次补齐管理展示，不关闭 R06 并行媒体迁移、R07 AudioHost 故障恢复、R12 完整质量观测和正式分发工作；TURN/APNs 未进行端到端实测的项仍保持未知。
