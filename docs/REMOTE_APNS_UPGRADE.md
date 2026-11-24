# Relay 升级与失败恢复

现有服务使用 `scripts/upgrade-relay-apns.sh`，不重跑首次安装脚本。当前 Relay 支持数据库 schema 1–5 的读取和迁移；APNs 普通短信通知引入 schema 4，APP 发短信队列引入 schema 5。

## 通过源码 tar 包升级

Mac 工作区执行：

```sh
python3 scripts/package-relay-apns.py
```

上传 `dist/djonehub-relay-apns-source.tar.gz` 和同名 `.sha256` 文件。服务器先校验，再解压到新目录，避免继续使用旧目录里的升级工具：

```sh
sha256sum -c djonehub-relay-apns-source.tar.gz.sha256
DJ_RELAY_WORK=$(mktemp -d)
tar -xzf djonehub-relay-apns-source.tar.gz -C "$DJ_RELAY_WORK"
cd "$DJ_RELAY_WORK/djonehub-relay-apns"
sudo bash scripts/upgrade-relay-apns.sh \
  --apns-config "$DJ_APNS_WORK/apns.json" --go-bin "$DJ_GO"
```

沿用服务器原有 `DJ_APNS_WORK` 和 `DJ_GO`；APNs 输入文件必须是 root 所有的私有临时副本。Go 要求见 `go.mod`。不上传新的私钥，不删除身份数据库。

升级会先编译、检查 APNs 文件，并在停服前以只读方式检查数据库版本和完整性。停服后再次检查，执行 WAL checkpoint，创建一致快照，再替换二进制、配置和启动服务。最后检查 `voip_push`、`sms_push`、`sms_send` 等能力。APNs 配置通过和健康检查通过不等于 Apple 已接受实际推送。

## 第二步备份失败，尚未进入第三步

旧版 `upgrade_apns.py` 只接受 schema 1–3，遇到 schema 4/5 会输出笼统的 `preflight/backup/health check failed`。新版支持 1–5，并打印不含凭据的具体失败原因。SQLite 被其他进程占用、文件权限或磁盘问题也可能导致备份失败，不能仅凭旧通用错误认定原因。

若日志明确停在 `[2/5]`，且之后没有手动替换二进制、配置或让其他新版程序迁移数据库，这次尝试没有执行安装或迁移。备份目录中的 `identities.sqlite` 可能不存在或不完整，不要把它覆盖回运行目录。

核对当前二进制仍与此次备份相同后，可启动原来正在运行的版本恢复服务。把下例目录改为本次失败打印的路径：

```sh
DJ_FAILED_BACKUP=/var/lib/djonehub-apns-backup.XXXXXX
if sudo cmp -s /usr/local/bin/djonehub-relay "$DJ_FAILED_BACKUP/djonehub-relay"; then
  sudo systemctl start djonehub-relay
  sudo systemctl status djonehub-relay --no-pager
else
  echo '当前二进制已变化：停止恢复，先核对后续安装或迁移操作。'
fi
```

恢复后使用新 tar 包中的升级脚本重新升级。若新检查仍失败，记录它打印的阶段、schema 或 errno；不要输出 APNs JSON、私钥、访问令牌或数据库内容。

## 第三步及之后失败

新版程序可能已经迁移数据库。保持 Relay 停止；不要直接把旧二进制放回去启动，也不要只恢复数据库。优先使用同一新版源码排查配置、权限和本机健康检查，修复后继续运行支持当前 schema 的新版程序。

必须整体回退时，先停止所有使用身份库的 Relay/独立管理进程，另行保留失败现场，再核验私有备份中的 SQLite 完整性以及配套旧二进制、APNs 配置、服务 override。恢复需同时匹配二进制、数据库与配置，数据库及相邻 WAL/SHM 文件也必须一致处理。若新服务已消费授权或轮换凭据，旧库回退会丢失这些状态；此时应优先修复新版服务，而不是回退身份状态。
