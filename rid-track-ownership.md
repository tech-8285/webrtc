# RID 与接收轨道的稳定归属分析（pion/webrtc v4）

> 分析对象：`github.com/pion/webrtc/v4`（当前工作区，分支 `task/38`）
> 主题：同一媒体（一个 m-line / 一个 transceiver）下多路编码（simulcast）如何通过 RID 稳定归属到对应的 `TrackRemote`，以及 MID / RID / SSRC / RTX 四种标识各自的职责与失效边界。
> 所有结论均标注实际代码位置，格式为 `文件:行`。

---

## 0. 结论速览

- **协商身份是 MID**：一个 m-line（media section）由 `a=mid` 唯一标识，并与一个 `RTPTransceiver` 一一绑定；多路编码共享同一个 MID。
- **编码层身份是 RID**：同一 MID 下的每一路编码（高/中/低层）由 `a=rid` 声明，接收端为每个 RID 预建一个 `TrackRemote` 占位（此时 SSRC 为 0）。
- **运行时包标识是 SSRC**：SSRC 在 simulcast 场景下通常**不在 SDP 中声明**，而是在首个 RTP 包到达时，通过包内 RTP 头扩展 `mid` + `rtp-stream-id` 动态获知并回填到对应 RID 的轨道上。
- **重传关联是 RTX（FID/rsid）**：RTX 是独立 SSRC 的修复流，非 simulcast 时由 `ssrc-group:FID` 关联，simulcast 时由 `repaired-rtp-stream-id`（rsid）扩展关联到对应 RID 的主编码。
- **归属成立的充要条件**：包内 MID 能匹配到一个已协商的 transceiver，且（RID 或 RSID）能在该 transceiver 的 receiver 已预建的轨道集合中找到同名 RID。任一条件不满足，该 SSRC 经过最多 `simulcastProbeCount = 10` 个包的探测后被丢弃，`OnTrack` 不会触发。

---

## 1. 端到端归属链路

### 1.1 SDP 阶段：simulcast / RID 的声明与解析

#### 1.1.1 发送侧如何把多路编码写进 SDP

一个 `RTPSender` 内部持有 `trackEncodings []*trackEncoding`（`rtpsender.go:21-32,36`），每个 encoding 对应一路编码，各自独立分配 SSRC / RTX SSRC / FEC SSRC（`rtpsender.go:200-215`，`util.RandUint32()`）。

多路编码通过 `RTPSender.AddEncoding` 组装（`rtpsender.go:152-198`），并有严格的同信封校验：

- 新 track 的 RID 不能为空 → `errRTPSenderRidNil`（`rtpsender.go:161-163`）；
- 必须先有一个带 RID 的 base encoding → `errRTPSenderNoBaseEncoding`（`rtpsender.go:177-179`）；
- 新增编码必须与 base 具有相同的 `ID()`、`StreamID()`、`Kind()` → `errRTPSenderBaseEncodingMismatch`（`rtpsender.go:181-183`）；
- 同一 sender 内 RID 不可重复 → `errRTPSenderRIDCollision`（`rtpsender.go:185-193`）；
- `Send()` 已调用或 sender 已停止后不允许再添加（`rtpsender.go:165-171`）。

也就是说，**“同一媒体的多路编码”在发送侧的定义是：同一个 sender（进而同一个 transceiver/m-line）下，相同 track id + stream id + kind、不同 RID 的多个 `TrackLocal`**。

SDP 生成在 `addSenderSDP`（`sdp.go:460-538`）：

- 对每个 encoding 写入 `a=ssrc`、`a=msid`，并在有 RTX/FEC 时写 `a=ssrc-group:FID` / `FEC-FR`（`sdp.go:470-521`）；
- **仅当 encoding 数量 > 1 时**，才输出 RID 与 simulcast 属性（`sdp.go:523-532`）：
  - 每路：`a=rid:<rid> send`
  - 汇总：`a=simulcast:send <rid1>;<rid2>;...`

接收方向（answer 侧回应 recv RID）在 `addTransceiverSDP`（`sdp.go:636-649`）输出 `a=rid:<rid> recv` 与 `a=simulcast:recv ...`；被暂停的层以 `~<rid>` 表示（暂停语义解析见 `getRids`，`sdp.go:295-313`），是否回显暂停可由 `SettingEngine.SetIgnoreRidPauseForRecv` 控制（`settingengine.go:727-731`，调用点 `sdp.go:642`）。

#### 1.1.2 接收侧如何从 SDP 抽出“一个多路编码轨道”

入口 `trackDetailsFromSDP`（`sdp.go:79-258`）。关键数据结构 `trackDetails` 的注释明确说明它**不能以 SSRC 为键，因为还要支持基于 RID 的源**（`sdp.go:24-35`）。

- 普通（非 simulcast）路径：每个 `a=ssrc` 生成一个 `trackDetails`；`a=ssrc-group:FID` 的第二个 SSRC 被识别为 RTX 修复流，记入 `rtxSsrc` 而**不会**被当成独立轨道（`sdp.go:110-141,191-193`）；FEC 同理（`sdp.go:142-170,194-196`）。
- simulcast 路径（`sdp.go:239-252`）：当 m-line 同时具备
  1. 至少一个 `a=rid`（`getRids`，`sdp.go:284-316`），且
  2. 能从 `a=msid` / `a=ssrc ... msid:` 拿到非空的 `trackID` 与 `streamID`，

  则整个 m-line 的所有 SSRC 细节被**折叠为单个 `trackDetails`**，其 `rids []string` 持有该媒体下全部 RID，`ssrcs` 为空。这就是“同一媒体的多路编码在协商层是一个对象、多个 RID”的代码体现。

随后 `trackDetailsToRTPReceiveParameters`（`sdp.go:260-282`）按 `max(len(rids), len(ssrcs))` 生成 `RTPDecodingParameters` 列表：RID 模式下每个 encoding 只填 `RID`，SSRC 留空（0）。

### 1.2 接收轨道的预建：先有 RID 占位，后知 SSRC

`SetRemoteDescription` 触发 `configureRTPReceivers`（`peerconnection.go:1789-1877`），对每个新 m-line 经 `runIfNewReceiver`（`peerconnection.go:1760-1784`，按 **MID 相等**匹配 transceiver）调用 `configureReceiver`（`peerconnection.go:1654-1665`）。

`RTPReceiver.configureReceive`（`rtpreceiver.go:177-194`）为每个 encoding 调 `newTrackRemote(kind, ssrc=0, rtxSsrc=0, rid, r)`（`track_remote.go:48-56`）。因此 simulcast 下一个 receiver 持有多个 `trackStreams`，每个 `TrackRemote.rid` 已确定而 `ssrc == 0`。`RTPReceiver.Tracks()` 返回全部层（`rtpreceiver.go:155-165`），而单路专用的 `Track()` 仅在恰好 1 个轨道时返回（`rtpreceiver.go:142-151`）。

`startReceive`（`rtpreceiver.go:197-286`）对 **RID 非空的 encoding 直接 `continue` 跳过**（`rtpreceiver.go:212-216`）：此时不开 SRTP read stream、不触发 OnTrack，一切等首个数据包动态建立。这就是 `startReceiver` 中 `track.SSRC() == 0 || track.RID() != ""` 时直接 return、不发 OnTrack 的原因（`peerconnection.go:1674-1677`）。

### 1.3 动态 SSRC 获知：未声明 SSRC 的探测与归属

SRTP 层每出现一个 SDP 未声明的新 SSRC，`undeclaredRTPMediaProcessor` 通过 `srtpSession.AcceptStream()` 收到它（`peerconnection.go:2277`），交给 `handleIncomingSSRC`（`peerconnection.go:2066-2252`）。完整判定顺序：

1. **已声明则不处理**：若 SSRC 已在 RemoteDescription 的主 SSRC / rtxSsrc / fecSsrc 中，直接返回（`peerconnection.go:2072-2083`）。
2. **单 m-line 兼容回退**：非 answer（或显式开启 `handleUndeclaredSSRCWithoutAnswer`）且只有一个 media section 时，先走 `handleUndeclaredSSRC`（`peerconnection.go:2085-2098,1952-2003`）。该路径发现 m-line 含 `a=rid` 会立即放弃（`peerconnection.go:1975-1976`），把流量让给 simulcast 探测；含显式 `a=ssrc` 则返回 `errMediaSectionHasExplictSSRCAttribute`（`peerconnection.go:1977-1978`）。
3. **扩展能力校验**：
   - 未协商 `urn:ietf:params:rtp-hdrext:sdes:mid` → answer 场景返回 `errPeerConnEarlyMediaWithoutAnswer`，否则最终返回 `errPeerConnSimulcastMidRTPExtensionRequired`（`peerconnection.go:2118-2136`）；
   - 未协商 `urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id` → `errPeerConnSimulcastStreamIDRTPExtensionRequired`（`peerconnection.go:2138-2143`）；
   - `urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id` 可选，仅取出其 ID（`peerconnection.go:2145-2147`）。
   - 这三个扩展在默认拦截器配置里由 `ConfigureSimulcastExtensionHeaders` 注册（`interceptor.go:251-268`，默认装配点 `interceptor.go:65-67`）。
4. **读包解析身份**：`handleUnknownRTPPacket`（`rtptransceiver.go:482-514`）从 RTP 头扩展取出三个字符串：
   - MID 扩展 → `mid`
   - rtp-stream-id 扩展 → `rid`
   - repaired-rtp-stream-id 扩展 → `rsid`
   并识别 padding-only 包。
5. **有限探测**：首个包缺身份时继续读包，循环上界 `simulcastProbeCount = 10`（`constants.go:17-20`，循环 `peerconnection.go:2181-2247`）。padding-only 包不计入探测预算（`peerconnection.go:2183-2186`）；探测期间读过的包存入 `peekedPackets`，归属成功后交还轨道，保证不丢包（`peerconnection.go:2193-2196` → `rtpreceiver.go:577` → `track_remote.go:136-151`）。
6. **二级匹配并建立归属**（`peerconnection.go:2210-2246`）：在 `pc.GetTransceivers()` 中找 `t.Mid() == mid` 且有 receiver 的 transceiver：
   - **`rsid != ""`（RTX 修复包）**：调 `receiver.receiveForRtx(ssrc=0, rsid, ...)`（`peerconnection.go:2216-2227`）；
   - **否则（主编码包）**：调 `receiver.receiveForRid(rid, ...)`（`peerconnection.go:2229-2242`），成功后 **在此处触发 `pc.onTrack`**（`peerconnection.go:2243`）。
7. 探测耗尽仍未匹配 → 解绑拦截器并返回 `errPeerConnSimulcastIncomingSSRCFailed`（`peerconnection.go:2249-2251`），上层仅记录 `incomingUnhandledRTPSsrc` 日志，**OnTrack 不触发**（`peerconnection.go:2318-2321`，日志模板 `constants.go:47`）。

`receiveForRid`（`rtpreceiver.go:550-593`）在 receiver 已有的 `r.tracks` 中按 **RID 相等**找到预建轨道，把动态获知的 `streamInfo.SSRC` 回填到 `track.ssrc`，并装配 codec、RTP/RTCP read stream、interceptor 与 peeked 包；找不到同名 RID → `errRTPReceiverForRIDTrackStreamNotFound`（`rtpreceiver.go:592`）。

> 注意：Pion 只负责协商这些头扩展的 ID（extmap），**不在发送路径自动填充 MID/RID**。发送侧 `TrackLocalStaticRTP.writeRTP` 仅用 binding 的 SSRC/PT 覆写包头（`track_local_static.go:195-215`）；MID/RID 扩展由应用写入（测试中的标准用法见 `peerconnection_media_test.go:1736-1737`、`2145-2146`）。

### 1.4 同一媒体多路编码应如何归属到视频轨道

综合上述代码，归属规则为：

```
RTP 包（未知 SSRC）
  └─ 头扩展 MID ──匹配──► RTPTransceiver（= 一个 m-line = 一个 RTPReceiver）
        └─ 头扩展 RID（主编码）/ RSID（RTX 修复）
              └─ 相等匹配 ──► receiver.tracks[i]（预建的 TrackRemote，rid 已固定）
                    └─ 回填该流的动态 SSRC，触发 OnTrack
```

- **横向隔离靠 MID**：不同 m-line（即使 RID 同名）不会串轨，因为 `handleIncomingSSRC` 先按 MID 选 transceiver（`peerconnection.go:2210-2214`），`trackDetailsForRID` 也同时校验 mid 与 rid（`sdp.go:47-59`）。
- **纵向区分靠 RID**：同一 transceiver 内的多层各有一个 `TrackRemote`，RID 是其稳定主键；`TrackRemote.RID()` 的注释直接说明“simulcast 下会有多个相同 ID 但不同 RID 的轨道，且 RID 可能为空，使用前必须断言非空”（`track_remote.go:68-76`）。
- **SSRC 是易失的运行时值**：simulcast 下 SSRC 不进 SDP、可在重协商后变化，轨道身份不依赖它；首个带身份扩展的包到达后才写入（`rtpreceiver.go:576`）。应用层应使用 RID（而非 SSRC）作为多层的长期归属键，SSRC 仅用于发包/RTCP 寻址（如示例中的 PLI：`examples/simulcast/main.go:128-139`）。
- **应用读取多路数据**：必须用 `receiver.ReadSimulcast(b, rid)` / `ReadSimulcastRTCP(rid)` 按 RID 取流；在多轨道 receiver 上调用普通 `Read()` 只会读第 0 路并打印错误日志 `Use ReadSimulcast(rid) instead of Read() when multiple tracks are present`（`rtpreceiver.go:296-307`，常量 `constants.go:49`）。RID 无对应轨道时返回包装了 `errRTPReceiverForRIDTrackStreamNotFound` 的错误（`rtpreceiver.go:323-325`）。

### 1.5 重协商下的归属保持与失效

`configureRTPReceivers` 的重协商分支（`peerconnection.go:1796-1853`）对每个现存轨道重新核对：

- RID 轨道：用 `trackDetailsForRID(incomingTracks, mid, track.rid)` 在新 SDP 中查找（`peerconnection.go:1815-1821`）；
- SSRC 轨道：用 `trackDetailsForSSRC` 查找（`peerconnection.go:1822-1829`）；
- 任一路在新 SDP 中找不到对应身份 → 整个 receiver 被 `Stop()` 并用新 receiver 替换（`peerconnection.go:1831-1852`），旧轨道读取出 `io.EOF`。对应测试 `TestPeerConnection_Renegotiation_Simulcast` 的 “Disable Transceiver” 与 “Change RID” 两个子用例（`peerconnection_renegotiation_test.go:1042-1107`）验证：RID 集合改变后旧 TrackRemote 全部关闭。

---

## 2. MID / RID / SSRC / RTX 的角色分工

| 标识 | 载体 | 生命周期阶段 | 在本代码库中的职责 | 关键代码 |
|---|---|---|---|---|
| **MID** | SDP `a=mid`；RTP 扩展 `sdes:mid` | 协商身份 | 绑定 m-line ↔ `RTPTransceiver`；多路编码共享同一 MID；动态 SSRC 归属的第一级键 | `rtptransceiver.go:435-443`（`findByMid`）、`peerconnection.go:2210-2214`、`rtptransceiver.go:501-503` |
| **RID** | SDP `a=rid`/`a=simulcast`；RTP 扩展 `sdes:rtp-stream-id` | 协商声明 + 运行时编码身份 | 同一 MID 下区分各编码层；接收轨道的稳定主键；simulcast 下先于 SSRC 存在 | `sdp.go:239-252,284-316`、`rtpreceiver.go:570-589`、`track_remote.go:68-76` |
| **SSRC** | SDP `a=ssrc`（可选）；每个 RTP 包头 | 运行时包标识 | SRTP/SRTCP 流的实际解复用键；simulcast 下动态获知、可重协商变化；单路（非 simulcast）场景下作为主身份 | `rtpsender.go:200-204`（随机分配）、`peerconnection.go:2277`（AcceptStream）、`rtpreceiver.go:576`（回填） |
| **RTX** | 独立 SSRC；SDP `ssrc-group:FID` 或扩展 `sdes:repaired-rtp-stream-id`(rsid) | 运行时重传关联 | 标识某主编码的修复流；接收端改写为原始包（OSN、主 PT、主 SSRC）后透明交付 | SDP 关联 `sdp.go:110-141`；动态关联 `rtpreceiver.go:621-667`；包改写 `rtpreceiver.go:700-754` |

### 2.1 RTX 的两条关联路径

1. **SDP 声明式（非 simulcast 或 SSRC 已声明）**：`a=ssrc-group:FID <主> <修复>` 把修复 SSRC 记入 `trackDetails.rtxSsrc`（`sdp.go:117-140`），`startReceive` 据此为修复 SSRC 单独开流并调 `receiveForRtxInternal`（`rtpreceiver.go:249-280`）。
2. **动态扩展式（simulcast，SDP 无 SSRC）**：修复包带 `rsid` 扩展，`handleIncomingSSRC` 以 `rsid` 调 `receiveForRtx`（`peerconnection.go:2216-2227`）。`receiveForRtxInternal` 的归属逻辑（`rtpreceiver.go:636-654`）：
   - 单轨道 receiver 且 ssrc 非 0：直接唯一定位；
   - 多轨道（simulcast）：按 `track.RID() == rsid` 找层，并在轨道尚未记录 rtxSsrc 时回填（`rtpreceiver.go:640-648`）；
   - 两者都不满足 → `errRTPReceiverForRIDTrackStreamNotFound: ssrc(...) rsid(...)`（`rtpreceiver.go:652-653`）。

修复流读出的 RTX 包在 `maybeStartRepairStreamReader` 中被还原：用 OSN 替换序列号、改写 payload type 与 SSRC 为主编码的值，原始 RTX 信息保存在 interceptor attributes（`AttributeRtxPayloadType/AttributeRtxSequenceNumber/AttributeRtxSsrc`，`constants.go:53-61`；改写逻辑 `rtpreceiver.go:700-754`），再经 `TrackRemote.read` 在主轨道上透明返回（`track_remote.go:153-160`）。修复读取器按需启动（首次 `Read` 触发 `requestRepairStreamReader`，`track_remote.go:124-127`）。RFC 4588 §6.3 约束：NACK 只针对主流，故 RTX streamInfo 清空 RTCPFeedback（`rtpreceiver.go:250-254`）。

### 2.2 无法唯一归属时的行为与错误边界

| 失败情形 | 行为 / 错误 | 位置 |
|---|---|---|
| sender 添加空 RID 编码 | 拒绝，`errRTPSenderRidNil` | `rtpsender.go:161-163` |
| sender 内 RID 重复 | 拒绝，`errRTPSenderRIDCollision` | `rtpsender.go:190-191` |
| 多编码信封上 `ReplaceTrack` | 拒绝，`ErrRTPSenderNewTrackHasIncorrectEnvelope`（不能替换 simulcast 信封） | `rtpsender.go:240-243` |
| 按 RID 读 RTCP 但无此 RID | `errRTPReceiverForRIDTrackStreamNotFound: <rid>` | `rtpreceiver.go:323-325,592,779` |
| answer 前早到媒体且未开兼容开关 | `errPeerConnEarlyMediaWithoutAnswer` | `peerconnection.go:2122-2125` |
| 缺 MID 扩展协商 | `errPeerConnSimulcastMidRTPExtensionRequired` | `peerconnection.go:2135` |
| 缺 RID 扩展协商 | `errPeerConnSimulcastStreamIDRTPExtensionRequired` | `peerconnection.go:2142` |
| 探测 10 包仍无 MID/RID，或 MID 找不到 transceiver，或 RID 不在预建集合 | 解绑流并返回 `errPeerConnSimulcastIncomingSSRCFailed`；**OnTrack 不触发**，仅错误日志 | `peerconnection.go:2249-2251,2318-2321` |
| RSID 找不到同名 RID 轨道 | `errRTPReceiverForRIDTrackStreamNotFound: ssrc(..) rsid(..)`，修复流不建立 | `rtpreceiver.go:652-653` |
| 并发探测例程数达上限 25 | 新 SSRC 被丢弃，日志 `ErrSimulcastProbeOverflow` | `peerconnection.go:2311-2316`，`constants.go:22-24`，`errors.go:192-194` |
| m-line 含显式 `a=ssrc` 却走未声明回退 | `errMediaSectionHasExplictSSRCAttribute` | `peerconnection.go:1977-1978` |
| receiver 已关闭后迟到的 RID/RSID 包 | `receiveForRid`/`receiveForRtxInternal` 直接返回 `io.EOF`，不重建任何流 | `rtpreceiver.go:566-568,632-634`（测试 `peerconnection_media_test.go:2203-2356`） |
| 重协商后 RID/SSRC 在新 SDP 消失 | 旧 receiver 停止、换新 receiver，旧轨道读返回 EOF | `peerconnection.go:1810-1852` |
| 未处理的 RTCP SSRC | 不触发 OnTrack，仅告警并累积关闭 | `peerconnection.go:2327-2350` |

边界说明：

- **MID 是硬前提**。包内 MID 与任何本地 transceiver 的 `Mid()` 都不相等时，循环无匹配，必然耗尽探测预算（`peerconnection.go:2210-2247`）。
- **RID 的作用域是 MID**。同名 RID 出现在不同 m-line 不构成冲突，因为匹配始终带 MID 约束（`sdp.go:47-59`、`peerconnection.go:2212`）；但同一 receiver 内 RID 必须唯一，sender 侧有碰撞校验，receiver 侧依赖 SDP 声明的 RID 集合。
- **SSRC 不具备跨重协商的身份语义**。应用若缓存 SSRC 作为轨道键，在层重启或重协商后会失效；RID 才是稳定键。
- **探测预算有限且有全局并发上限**：单流 10 包、全局 25 个探测例程。身份扩展持续缺失的源会被静默丢弃（仅日志），不会无限占用资源。
- padding-only 探测包不消耗预算但也不能建立归属（`rtptransceiver.go:495`、`peerconnection.go:2183-2186`），测试 `TestPeerConnection_Simulcast` 明确断言纯 padding 不会触发 OnTrack（`peerconnection_media_test.go:1703-1722`）。

---

## 3. 轨道归属成立与失败的判据

### 3.1 归属成立判据（须同时满足）

1. **协商层存在对应媒体**：远端 SDP 的该 m-line 含 `a=mid`，且本地存在 `Mid()` 相等、方向为 recvonly/sendrecv 且尚未 `haveReceived()` 的 transceiver/receiver（`peerconnection.go:1760-1784`）。
2. **simulcast 信封完整**：m-line 含至少一个 `a=rid` 与 `a=simulcast`，且具备非空 msid（trackID + streamID），从而 `trackDetailsFromSDP` 折叠出携带 RID 列表的单一 `trackDetails`（`sdp.go:239-252`）。
3. **接收轨道已按 RID 预建**：`configureReceive` 为每个 RID 建立 `TrackRemote`（rid 非空、ssrc=0）（`rtpreceiver.go:181-193`）。
4. **头扩展已协商且出现在包内**：`sdes:mid` 与 `sdes:rtp-stream-id` 扩展 ID 已在 MediaEngine 注册并出现在 offer/answer（RTX 还需 `sdes:repaired-rtp-stream-id`），且到达包在探测预算（10 包）内携带非空 MID 与 RID/RSID（`peerconnection.go:2118-2147,2181-2208`）。
5. **二级匹配全部命中**：
   - 主编码：包内 MID 命中 transceiver **且** 包内 RID 命中该 receiver 的某个预建轨道 → `receiveForRid` 回填 SSRC 并返回该 `TrackRemote`，随后触发 OnTrack（`rtpreceiver.go:570-589`、`peerconnection.go:2229-2243`）。
   - RTX：包内 RSID 命中同名 RID 轨道（或单轨道 receiver 唯一定位）→ 修复流装配成功（`rtpreceiver.go:636-664`）。
6. **运行期仍有效**：receiver 未 Stop/Close（否则返回 EOF），且后续重协商的 SDP 仍保留该 MID+RID（否则 receiver 被替换、轨道 EOF）。

满足后可观测的成立标志：`OnTrack(track, receiver)` 被调用，`track.RID()` 非空，首个数据包后 `track.SSRC()` 非零，`receiver.Tracks()` 包含该层，`ReadSimulcast(rid)` 可读，stats 中出现带相同 `Mid`/`Rid` 的 inbound-rtp 项（`rtpreceiver.go:444-471`）。

### 3.2 归属失败判据（任一成立即失败）

1. **无 MID 归属**：包内 MID 为空，或与所有 transceiver 的 MID 不匹配 → 探测耗尽，`errPeerConnSimulcastIncomingSSRCFailed`，无 OnTrack。
2. **RID 未声明/不匹配**：包内 RID（或 RTX 的 RSID）不在该 receiver 由 SDP 预建的 RID 集合中 → 主流探测失败丢流；修复流返回 `errRTPReceiverForRIDTrackStreamNotFound`。
3. **必要扩展未协商**：缺 mid 扩展（`errPeerConnSimulcastMidRTPExtensionRequired` / 早媒体场景 `errPeerConnEarlyMediaWithoutAnswer`）或缺 rtp-stream-id 扩展（`errPeerConnSimulcastStreamIDRTPExtensionRequired`）。
4. **探测预算耗尽**：连续超过 `simulcastProbeCount`（10）个可计包仍读不到身份扩展（padding-only 不计）；或全局探测例程超过 `simulcastMaxProbeRoutines`（25）触发 `ErrSimulcastProbeOverflow`。
5. **信封校验不过（发送侧）**：空 RID、RID 碰撞、base 编码 id/stream/kind 不一致、Send 后追加编码等，多路编码在 SDP 生成前即被拒绝。
6. **生命周期失效**：receiver 已关闭后包才到达（EOF，不重建）；重协商移除了该 MID 或更换了 RID 集合（旧轨道全部 EOF）。
7. **RTCP 侧无对应流**：未声明 RTCP SSRC 仅告警，`OnTrack will not be fired`（`peerconnection.go:2348`）。

失败的统一外在表现：**该 SSRC 不会产生 `TrackRemote` / `OnTrack`，数据被丢弃，仅以日志或返回错误形式暴露**；已建立的层若在重协商中失去身份，则读取端收到 `io.EOF`。这界定了错误边界——Pion 不会猜测归属（单 m-line 的旧式兼容回退除外，且该回退在检测到 `a=rid` 时主动让路，`peerconnection.go:1975-1976`），身份不全的源不会被错误挂到任何视频轨道上。
