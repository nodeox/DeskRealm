# DeskRealm 区域中继登记

区域中继的中心登记与健康回执使用独立节点身份。中心的 `relay_enrollment` 任务可从任务中心查看。

## 中心 API

平台管理员在同源会话中调用 `POST /v1/admin/relays`，提交 `name`、`site`、`region`、`endpoint`、`build` 和 `capacity`。公布端点使用带端口的 DNS 名称或 IP 地址。成功响应包含节点 ID、任务 ID 和 15 分钟有效的一次性登记凭据。将凭据通过授权渠道交给相应节点的运维人员。

登记未完成时，可调用 `POST /v1/admin/relays/{id}/credential` 为同一节点补发凭据。补发后旧凭据及旧挑战失效。节点完成登记后保持已登记的身份。

节点 API 使用已验证的中心 HTTPS 地址。节点生成并保存独立的 Ed25519 密钥，以无填充 base64url 编码公钥和签名。

1. 使用登记凭据作为 Bearer Token，向 `POST /v1/relays/enrollment/challenge` 提交 `node_id` 和 `public_key`。
2. 按 `relayprotocol.Enrollment.Frame` 对节点 ID、公布端点、站点、区域、版本、容量、挑战、有效期和公钥签名。使用原 Bearer Token，向 `POST /v1/relays/enrollment/complete` 提交 `node_id`、`public_key` 和 `signature`。
3. 通过 `POST /v1/relays/health` 上报 `relayprotocol.Health` 和对应签名。每次报告使用递增序号、当前观测时间和中心配置版本。中心拒绝重放、超出容量和版本不匹配的报告。

`POST /v1/relays/configuration` 接受已登记节点签名的 `relayprotocol.ConfigRequest`，用于同步中心批准的配置。请求携带节点 ID、随机 nonce 和当前观测时间。

`GET /v1/admin/relays` 返回节点状态。中心每五秒从自己的网络连接公布的 TCP 端点，发送随机挑战，并用登记公钥核对端点对构建版本与会话数的签名。首次通过探测并具有 15 秒内的签名健康回执和可用容量时，节点进入 `available`，登记任务写入 `relay_available` 和审计记录。探测结果 15 秒后过期，连续三次失败时节点进入 `probe_unreachable`，下一次有效探测可恢复。节点达到容量时显示为 `at_capacity`。

## 控制台与生命周期

平台管理员从「任务中心」选择「接入区域中继」可生成节点的登记命令和一次性凭据。「区域中继」按区域筛选，节点详情展示登记任务、最近操作、最近 12 次探测和已建立会话。概览中的「可用中继」统计当前具有签名健康、有效探测和剩余容量的可选节点。

`POST /v1/admin/relays/{id}/lifecycle` 接受节点详情中的 `revision` 及 `action`。支持 `drain`、`disable`、`resume` 和 `change_endpoint`。地址切换还需提交 `endpoint`。每次执行返回 `task_id` 和 `result_code`，从任务详情可关联审计。对同一修订的并发变更只有一个能完成。

排空和停用会把节点移出新会话候选。已建立的会话以自身租约为准。恢复需通过独立探测和当前配置的签名健康检查。更新公布地址前，应将新端点连到同一节点进程，使用端口映射时保持本机端口可用。中心核对新地址上的节点身份后才提交地址和配置版本。节点进程同步配置并上报新版本的健康回执后，才能用新地址入池。

## 中继主机登记

从仓库根目录执行 `go build -o deskrealm-relay ./cmd/deskrealm-relay`。使用中心签发的节点 ID，并将一次性凭据放在仅服务账户可读的文件中，权限设为 `0600`。以下命令使用已经存在的状态父目录 `/var/lib/deskrealm`。

```sh
./deskrealm-relay prepare --state /var/lib/deskrealm/relay --center https://deskrealm.example.com --node "$NODE_ID"
./deskrealm-relay enroll --state /var/lib/deskrealm/relay --credential-file /run/deskrealm/relay-credential
./deskrealm-relay run --state /var/lib/deskrealm/relay --hbbr /usr/local/bin/hbbr
./deskrealm-relay status --state /var/lib/deskrealm/relay
```

中心使用私有 CA 时，在 `enroll` 和 `run` 中指定 `--ca /path/to/center-ca.pem`。登记成功后移除凭据文件，并将状态目录作为私密数据持久挂载或备份。中心已经完成登记但回复丢失时，用原状态目录运行 `enroll` 可取回配置，无需再次使用凭据。

用 `scripts/deskrealm-managed-source prepare /path/to/source --apply-patches` 准备锁定源码，再在准备好的 `server` 目录运行 `cargo build --locked --release --bin hbbr`。

`run` 在前台启动补丁版 `hbbr`，默认监听公布端口。使用端口映射时可用 `--port` 指定本机端口，中心仍探测登记的公布端点。进程从本机验证 `hbbr` 并上报签名健康。重启沿用状态目录中的密钥和健康序号。

当前区域模式提供身份验证和候选目录所需的探测监听。双端选点、节点票据和实际会话转发由 04-04 接续。

## 会话探测轮次

主控使用发起会话的受管用户凭据，受控端使用此会话目标设备的受管设备凭据。两端各自用设备密钥签署加入证明，向 `POST /v1/sessions/{id}/relay-candidates` 提交 `connection_id` 和 `signature`。主控先创建等待轮次，目标设备加入后开启最长两秒的采样窗口。新连接开始时替换此前轮次。

`GET /v1/sessions/{id}/relay-candidates?connection_id=…` 返回当前轮次、有效期、快照中的公布地址、公钥、构建及配置版本。候选快照按组织及当前健康、探测、排空与容量状态生成。轮次内配置或资格变动的节点标记为不可用。

两端通过 `POST /v1/sessions/{id}/relay-probes` 各自为每个节点提交一批三个有序采样。请求包含 `connection_id`、`round_id`、`relay_id`、`samples` 和 `signature`。每个采样的 `result` 为 `ok`、`unreachable`、`timeout`、`protocol_mismatch` 或 `missing`。仅成功采样携带 `rtt_us`。至少两个成功采样才计算中位数。加入和报告使用共用的 Go/Rust 签名帧，报告签名覆盖会话、连接、轮次、角色、节点、配置版本及公布端点摘要。

此检查点已提供中心轮次与结果接口。站点及 CIDR 规则、受管客户端采样入口、节点探测授权与实际转发接续开发。

## 开发验证

```sh
bash scripts/deskrealm-m3-test relay-enrollment
bash scripts/deskrealm-m3-test relay-lifecycle
bash scripts/deskrealm-m3-test relay-probes
```

登记检查构建受管 `hbbr` 并运行两个隔离的 Linux 实例。生命周期检查使用临时 PostgreSQL、受控端点和 Chromium。会话探测检查核对签名帧与中心授权，并由两份独立端点夹具对两套实际 Linux `hbbr` 采样。目标环境的 Linux 中心、中继和双 Windows 验收安排在 M0–M3 开发完成后。
