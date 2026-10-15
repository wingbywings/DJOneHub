# 模块语音初始化的重启与回滚核验

5Ber1 的初始化曾在回滚后的 `AT+CFUN=1,1` 阶段返回
`LIBUSB_ERROR_IO`，随后 USB 重新枚举并恢复连接。旧实现立即报告重启失败，
且该错误覆盖了触发回滚的首次失败原因。

当前流程：

- 按设备保存执行阶段、首次失败阶段/原因、重启通信错误和回滚备份路径。
  阶段变化写入服务日志；QADBKEY passcode 仍只经私有通道提交，不进入日志或状态。
- 配置写入和回读必须收到完整 OK。重启前检查保存的 IMS 配置；重启后再要求
  IMS 运行时能力就绪，避免能力尚未生效时提前回滚。
- 仅在重启命令周围，将 IO、PIPE、NO_DEVICE 和超时视为待核验的通信中断。
  不据此认定配置写入或重启成功，也不自动重发重启命令。
- USB 重启必须观察到原物理端口的新枚举，再回读配置。
  验证窗口为 60 秒，截止时间持久化；服务重新启动不会无限延长等待。
- 回滚也经过重启与再次回读。只有原始 USB/IMS/VoLTE 值得到确认，才报告恢复完成。
  回滚失败、通信异常或超时均保留首次失败原因。
- 同一模块只允许一个初始化事务；晚到的旧事务更新不能覆盖新事务。

验证命令：

```sh
GOCACHE=/private/tmp/djonehub-go-cache go test ./...
GOCACHE=/private/tmp/djonehub-go-cache go test -race ./cmd/djonehub-macos \
  -run 'Test.*(ModuleSetup|USBSetup|ADBUnlock|TargetVoiceUSB|ParseIMS|QADB|GenerateQADB)' -count=1
node scripts/test-module-setup.mjs
```

自动测试使用模拟 AT 通道，覆盖 USB 断线、原枚举未改变、错误物理端口、
回滚失败、重启超时、瞬时读失败、部分回复、IMS 能力延迟生效、诊断持久化和并发初始化。
真实模块能否完成初始化及通话仍需硬件验收。旧版已覆盖的错误无法追溯恢复，
升级后再次初始化会记录新的首次失败原因。
