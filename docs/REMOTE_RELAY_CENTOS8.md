# CentOS Linux 8：Relay 安装前的软件源与依赖

适用于 2026-10-09 收到的 CentOS Linux 8 AppStream 403、重复仓库配置错误。主安装指南已经按这台机器编写：CentOS Linux 8、TCP 80/443 上的现有网站，以及 `https://oops.work` 的现有证书。本文只在系统 `dnf` 不能用时，提供一套独立 Vault 源来安装同样的编译器和 coturn。证书复制、8443 和 firewalld 以主指南第 4 节为准。没有在这台服务器上执行这些命令。

## 原因与边界

- `Repository ... is listed more than once` 表示启用的配置来源存在重复 repository ID。需要查看实际配置才能确定重复来自哪些文件。
- 本次安装直接失败于旧 AppStream URL 的 HTTP 403。仅凭错误不能确定是路径、HTTP 策略还是网络代理导致，先用固定归档版本的 HTTPS 地址验证。
- `build-essential` 是 Debian/Ubuntu 元包，CentOS 改装 `gcc gcc-c++ make`；`pkg-config` 使用 `pkgconf-pkg-config`。
- coturn 使用 EPEL 8。这台服务器已经有 `oops.work` 证书，本次不安装 certbot。EPEL 当前包可能依赖比 CentOS 8.5 归档更新的 EL8 基础库，不能保证任意时间都能在冻结的 8.5 上解析成功。

CentOS Linux 8 已于 2021-12-31 停止维护，Vault 不再提供安全更新。因此这套步骤用于继续联调；长期公网部署应安排迁移至仍受维护的系统。参考 [CentOS 官方 EOL](https://www.centos.org/centos-linux-eol/) 和 [官方 Vault AppStream 归档](https://vault.centos.org/8.5.2111/AppStream/x86_64/os/)。

## 1. 确认版本和源可达性

```sh
cat /etc/centos-release
cat /etc/os-release
uname -m
test -r /etc/pki/rpm-gpg/RPM-GPG-KEY-centosofficial

curl -fL --connect-timeout 10 --max-time 30 -o /dev/null \
  -w 'HTTP %{http_code}\n' \
  https://vault.centos.org/8.5.2111/AppStream/x86_64/os/repodata/repomd.xml
```

本例架构对应用户日志里的 x86_64。必须确认为 **CentOS Linux 8**；CentOS Stream 8、Rocky、Alma 或 RHEL 不按这个归档路径配置。HTTPS 应返回 200；若仍 403、TLS 错误或超时，先停在这里检查服务器到 Vault 的代理/网络，不关闭 TLS 或 GPG 校验。

## 2. 使用独立仓库目录，绕开重复配置

创建 `/etc/djonehub-bootstrap.repos.d`，仅在显式指定它的 dnf 命令中使用。既有 `/etc/yum.repos.d` 和 yum 全局配置保留；普通 yum 命令仍可能报告旧配置问题，这里不是全局修复。

```sh
sudo bash <<'SH'
set -eu
test -r /etc/pki/rpm-gpg/RPM-GPG-KEY-centosofficial
install -d -m 755 /etc/djonehub-bootstrap.repos.d
for component in BaseOS AppStream PowerTools extras; do
cat <<EOF
[djonehub-${component}]
name=DJOneHub CentOS 8.5.2111 ${component}
baseurl=https://vault.centos.org/8.5.2111/${component}/\$basearch/os/
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-centosofficial

EOF
done > /etc/djonehub-bootstrap.repos.d/centos-vault.repo
chmod 644 /etc/djonehub-bootstrap.repos.d/centos-vault.repo
SH

sudo dnf --releasever=8 \
  --setopt=reposdir=/etc/djonehub-bootstrap.repos.d \
  --refresh makecache
```

这四个归档仓库元数据必须加载成功后再安装，不要使用 `skip_if_unavailable` 跳过失败后继续猜测。输出若仍显示原来的重复仓库警告，保留完整命令和日志，核对 reposdir 是否正确传入及是否有额外插件注入仓库。

## 3. 安装基础依赖与 EPEL

```sh
sudo dnf --releasever=8 \
  --setopt=reposdir=/etc/djonehub-bootstrap.repos.d \
  install -y gcc gcc-c++ make pkgconf-pkg-config \
  ca-certificates openssl python3 curl jq epel-release

# EPEL release 包默认把 repo 放到系统目录；复制主仓库定义到本次专用目录。
sudo install -m 644 /etc/yum.repos.d/epel.repo \
  /etc/djonehub-bootstrap.repos.d/epel.repo

sudo dnf --releasever=8 \
  --setopt=reposdir=/etc/djonehub-bootstrap.repos.d \
  --refresh install -y coturn

rpm -q gcc make pkgconf-pkg-config coturn
command -v turnserver
```

使用 release 包提供的 EPEL GPG key 与仓库配置，不安装 epel-next、epel-testing 或 EL9 包。官方包信息：[coturn / EPEL 8](https://packages.fedoraproject.org/pkgs/coturn/coturn/epel-8.html)。网站证书已经存在，不要用这套 Vault 源安装 certbot，也不要申请新证书。

如果 epel-release 已装但 `/etc/yum.repos.d/epel.repo` 被删除，不能跳过复制步骤，应先通过上面的独立源重新安装 epel-release 恢复该文件。若出现依赖版本冲突，保留完整报错；不要使用 `--allowerasing`、`--nogpgcheck` 或混装其他发行版基础包。

## 4. 接回安装指南

回到 [完整安装指南](REMOTE_IPHONE_INSTALL_AND_TEST.md) 第 3 节的 Go 构建。gcc 和 coturn 已用上面的 `reposdir` 装好时，跳过主指南里的普通 `dnf install`，直接安装官方 Go 1.26.3 并编译。不要假定 CentOS Vault 的 golang 包满足 `go.mod`。Relay 要在此主机以 `CGO_ENABLED=1` 构建，SQLite 驱动需要 GCC。

账户、私有目录、证书、Relay/TURN 和配对仍按主指南执行：

- `SSH_USER` 换成实际登录用户。
- `command -v turnserver` 核对 systemd unit 的路径；主指南使用自定义 unit 和 `/etc/djonehub/turnserver.conf`，不是 EPEL 默认的 `/etc/coturn/turnserver.conf`。
- 端口和证书按主指南第 4 节。TCP 80/443 留给现有网站；追加 TCP 8443/3478/5349 与 UDP 3478、49160–49259。复制网站正在使用的 `oops.work` 证书，不申请新证书。
- 保持 SELinux 的现有保护。若服务报权限拒绝，检查 `journalctl` 与 AVC 记录后按实际上下文修复，不以关闭 SELinux 作为通用安装步骤。
- 软件源恢复之后，仍要按主指南验收 8443、TURN 和电话音频。

## 5. 后续排查重复 repo

本次通过独立目录绕开重复配置，未删除任何原有仓库。可先只读定位：

```sh
grep -nE '^\[[^]]+\]' /etc/yum.conf /etc/dnf/dnf.conf /etc/yum.repos.d/*.repo
```

同一 repository ID 出现在多处时，结合文件来源、业务依赖和内容选择保留哪份；备份后再合并/禁用重复项。不要批量删除全部 `.repo`，否则可能影响其他已部署服务的软件更新。
