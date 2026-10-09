module github.com/iniwex5/vohive

go 1.26.3

replace github.com/damonto/euicc-go => ./third_party/euicc-go

replace github.com/damonto/uicc-go => ./third_party/uicc-go

replace github.com/iniwex5/quectel-qmi-go => ./third_party/quectel-qmi-go

replace github.com/lestrrat-go/strftime => ./third_party/strftime

replace github.com/pkg/errors => ./third_party/pkg-errors

replace golang.org/x/sys => ./third_party/x-sys

replace golang.org/x/text => ./third_party/x-text

replace go.uber.org/multierr => ./third_party/multierr

require (
	github.com/damonto/euicc-go v1.1.3-0.20260628013808-8d873a2dfc98
	github.com/damonto/uicc-go v0.0.0-20260629073618-7ddada6bb13e
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/iniwex5/quectel-qmi-go v0.6.0
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/pion/interceptor v0.1.49
	github.com/pion/rtp v1.10.5
	github.com/pion/webrtc/v4 v4.2.22
	github.com/spf13/viper v1.21.0
	github.com/warthog618/sms v0.3.0
	go.bug.st/serial v1.6.4
	go.uber.org/zap v1.27.1
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/sync v0.20.0
	golang.org/x/sys v0.46.0
	golang.org/x/time v0.14.0
)

require (
	github.com/creack/goselect v0.1.2 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/lestrrat-go/strftime v1.2.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pion/datachannel v1.6.3 // indirect
	github.com/pion/dtls/v3 v3.1.9 // indirect
	github.com/pion/ice/v4 v4.4.4 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.1 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.18 // indirect
	github.com/pion/sctp v1.11.3 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.1.0 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.1 // indirect
	github.com/pion/turn/v5 v5.1.2
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/net v0.50.0 // indirect
	golang.org/x/text v0.34.0 // indirect
)
