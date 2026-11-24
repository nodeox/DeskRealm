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

区域模式在同一 TCP 端口提供验活、会话 RTT 探测和受管会话转发。节点在已配对且仍转发的连接数内执行配置容量，签名验活响应报告当前会话数。

## 排空并升级区域中继

此流程适用于同一构建版本、与现行配置和会话协议兼容的受管 `hbbr` 二进制。中心需要应用中继升级迁移。用节点服务账户启动主管进程，持久挂载节点状态目录，并允许从该目录执行二进制。首次改用此方式时，先通过现有排空流程安排一次服务重启。

```sh
deskrealm-relay run --managed-upgrades --state /var/lib/deskrealm/relay \
  --hbbr /usr/local/bin/hbbr
```

在「区域中继」打开节点详情，选择「排空并升级」，填写已核验的候选文件 SHA-256。中心立即停止向该节点分配新会话。已建立的连接继续按租约运行。升级任务处于待领取时可以取消，取消后节点保持排空。

在该中继主机用同一服务账户提交候选文件，使用控制台升级任务的 ID。

```sh
deskrealm-relay upgrade --state /var/lib/deskrealm/relay \
  --task "$TASK_ID" --hbbr /srv/deskrealm/releases/hbbr --sha256 "$SHA256"
deskrealm-relay upgrade-status --state /var/lib/deskrealm/relay
```

私有 CA 部署在 `run` 和 `upgrade` 使用相同的 `--ca`。主机会复制并校验候选文件。中心等待已有配对票据到期，确认没有有效且未结束的会话租约，再从公布端点独立验证节点身份和零活动连接。主机也检查本机报告为零才切换。保持中心和中继的系统时钟同步。领取升级时中心要求两端时钟差在 2 秒以内。

候选通过本机验活、签名健康上报及中心端点探测后，任务记录为升级已验证。如果候选失败，主机启动保存在状态目录中的原二进制，由中心复验后记录回滚。升级与回滚结束后，由平台管理员验证任务并选择「恢复」使节点重新接收会话。状态为 `switching` 或 `rolling_back` 时，使用原来的 `run --managed-upgrades` 参数重启可续接记录。监督进程异常终止时，先确认旧 `hbbr` 已退出并释放端口。若原二进制也无法启动，节点保持排空，检查主机日志和状态目录中的已校验文件。状态目录同时保存节点密钥、升级记录和升级前后的二进制。

## 会话探测轮次

主控使用发起会话的受管用户凭据，受控端使用此会话目标设备的受管设备凭据。两端各自用设备密钥签署加入证明，向 `POST /v1/sessions/{id}/relay-candidates` 提交 `connection_id` 和 `signature`。主控先创建等待轮次，目标设备加入后开启最长两秒的采样窗口。新连接开始时替换此前轮次。

`GET /v1/sessions/{id}/relay-candidates?connection_id=…` 返回当前轮次、有效期、网络规则修订、快照中的公布地址、公钥、构建及配置版本。候选须通过健康、探测、排空、容量和网络规则。轮次内配置或资格变动的节点标记为不可用。任一端来源地址变更时，主控应以新连接 ID 发起轮次。

两端通过 `POST /v1/sessions/{id}/relay-probes` 各自为每个节点提交一批三个有序采样。请求包含 `connection_id`、`round_id`、`relay_id`、`samples` 和 `signature`。每个采样的 `result` 为 `ok`、`unreachable`、`timeout`、`protocol_mismatch` 或 `missing`。仅成功采样携带 `rtt_us`。至少两个成功采样才计算中位数。加入和报告使用共用的 Go/Rust 签名帧，报告签名覆盖会话、连接、轮次、角色、节点、配置版本及公布端点摘要。

有效轮次内，每个可用候选仅向当前角色返回自己的 `probe_permit`。中心签名覆盖会话、授权、连接、轮次、节点、端点摘要、配置版本、端点设备公钥及最长两秒的有效期。中继通过已验证的中心 HTTPS 获取验签公钥，启动后在公布的 TCP 端点核对凭据和节点持久配置。两端各用设备私钥应答中继生成的随机挑战，每份凭据允许三个不同采样序号。RTT 计时从发送持钥证明到收到中继签名响应，TCP 建连和挑战交换在计时前完成。

中继按本机时间校验凭据，节点与中心应保持时钟同步。规则更新或会话撤销时，中心停止签发和接受报告。节点在凭据到期时停止接受该轮探测。独立健康探测继续使用既有协议。

受管 Rust 主控在发起中继连接时创建探测轮次，受控端收到 `hbbs` 转发的区域中继请求后加入。双方用各自的设备密钥独立采样并提交报告。组织有已登记的区域中继，或启用 `restricted` 网络策略时，受管中继会话要求区域票据。其余组织继续使用原先配置的单中继。

### 确认和回退

采样结束后，主控可请求 `GET /v1/sessions/{id}/relay-selection?connection_id=…` 查看按双端中位 RTT 之和排序的候选。同分时比较当前会话占容量的比例，再按节点 ID 排序。中心在接收 `POST /v1/sessions/{id}/relay-selection` 时重新校验候选、健康、负载和排序。提交包含 `connection_id`、`round_id`、`relay_id`、`action` 和主控设备签名。以 `select` 确认可用首选；连接失败时以 `fail` 记录失败并取得其余候选。受控端可查看已确认的节点。新的连接 ID 会启动新轮次。

没有可用双端中位数时，依次按端点所属站点、GeoIP 区域、默认区域和负载排序。任意一端的三个采样均明确不可达或协议不匹配的候选会被排除。平台管理员可通过网段策略的可选字段 `geoip_regions`（由 `cidr` 与 `region` 组成的静态映射）和 `default_region` 配置区域回退。GeoIP 也按最长前缀匹配。确认和重试共享采样结束后的 10 秒窗口，并受会话授权有效期约束。

确认后，中心分别返回绑定会话、轮次、所选中继和端点角色的短期票据。`hbbr` 对两端发起随机挑战，仅配对该节点、该连接和配置版本下持有对应设备私钥的主控与受控端。受控端将节点签回的配对回执提交至 `POST /v1/sessions/{id}/relay-pairing`，随后核销授权并开始租约。会话仍使用端到端加密。确认的节点连接失败时，主控记录失败并在当前选择窗口内尝试下一候选。

已建立的会话断线后，受控端上报连接中断。用户再次发起连接时，主控重新申请授权，使用新的连接 ID 创建探测轮次，并从当时可用的节点重新选点。受管会话的详情展示各自的实际核销路径、连接 ID、停止原因，以及本会话的实际核销轮次和近期轮次。候选表列出主控和受控的 RTT 中位数、每次采样结果、选点依据和失败后排除的节点。

### 站点与网络策略

平台管理员通过 `GET /v1/admin/relay-network-policy` 查看当前规则与修订号，并在同源会话中提交 `POST /v1/admin/relay-network-policy`。保存时提供当前 `revision` 和完整规则，中心返回新修订。保存后，尚未核销会话的当前探测轮次失效，已提交的历史样本保留。初始修订为 0，使用 `organization` 模式，候选按组织与节点可用状态筛选。将模式设回 `organization` 时，提交空的规则数组。

`restricted` 模式按最长 CIDR 前缀确定每端的站点，站点对按主控到受控的方向匹配。节点须位于站点对许可的区域，且双方的来源地址分别命中该节点的一个 CIDR。没有匹配规则的节点不会出现在候选中。以下例子中的节点 ID 应替换为控制台已登记的节点 ID。

```json
{
  "revision": 0,
  "mode": "restricted",
  "networks": [
    { "cidr": "10.10.0.0/16", "site": "office" },
    { "cidr": "2001:db8:20::/64", "site": "plant" }
  ],
  "site_pairs": [
    { "controller_site": "office", "target_site": "plant", "regions": ["north"] }
  ],
  "relay_networks": [
    { "relay_id": "1c2fa0ac-3c92-4cdc-8b5e-803acdbb80ca", "cidrs": ["10.10.0.0/16", "2001:db8:20::/64"] }
  ]
}
```

直连中心时使用 TCP 对端地址。经 HTTPS 反代的部署可配置 `DESKREALM_RELAY_TRUSTED_PROXIES`，值为逗号分隔的直接反代 IP。反代须覆盖客户端提供的 `X-Forwarded-For`，并只转发一个已核验的客户端 IP。中心忽略其他来源的转发头；受信反代缺少该头或提供多值时，中心拒绝该轮次请求。通过安装器部署时使用 `https.relay_trusted_proxies`。

## 开发验证

```sh
bash scripts/deskrealm-m3-test relay-enrollment
bash scripts/deskrealm-m3-test relay-lifecycle
bash scripts/deskrealm-m3-test relay-upgrade
bash scripts/deskrealm-m3-test relay-probes
bash scripts/deskrealm-m3-test relay-selection
bash scripts/deskrealm-m3-test multi-relay
```

登记检查构建受管 `hbbr` 并运行两个隔离的 Linux 实例。生命周期检查使用临时 PostgreSQL、受控端点和 Chromium。会话探测检查核对签名帧与中心授权，并通过 Rust 受管端点对两套实际 Linux `hbbr` 采样，从 PostgreSQL 核对已提交结果。目标环境的 Linux 中心、中继和双 Windows 验收安排在 M0–M3 开发完成后。
