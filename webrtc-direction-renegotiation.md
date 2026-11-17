# Pion WebRTC 音视频方向重协商：状态关系与生效时点分析

- 代码基线：`github.com/pion/webrtc/v4`，工作区提交 `60271fb`（`go.mod` 声明 `go 1.24.0`）
- 分析范围：原生 Go 实现（`//go:build !js`）。`rtptransceiver_js.go` 等 JS 绑定不在内。
- 交付性质：**仅源码分析报告**。结论以 `文件:行号` 引用实现与既有测试；凡本沙箱内未取得运行证据之处，在第 6 节统一标明。
- 运行环境事实：本机 `GOROOT` 默认指向缺失目录，修正为 `GOROOT=/Users/dengquan/.local/sdk/go1.25.9` 后 `go build ./...` 通过；纯 SDP/状态机测试可跑且通过，依赖 ICE 实际连通的测试在本沙箱内挂起（见第 6 节）。

---

## 1. 三套方向状态：本地方向、远端方向、最终协商方向

一个 `RTPTransceiver`（`rtptransceiver.go:19-38`）用三个 `atomic.Value` 承载方向语义，外加"实际是否在收发"的运行态：

| 状态字段 | 访问器 | 语义（对应 W3C 概念） | 写入时机 |
|---|---|---|---|
| `direction` | `Direction()` / `setDirection()` | **本地方向意图**（W3C `RTCRtpTransceiver.direction`） | 本地 API 调用立即改；处理远端 offer 时也会被改写 |
| `currentRemoteDirection` | `getCurrentRemoteDirection()` / `setCurrentRemoteDirection()` | 最近一次远端描述中该 mid 的方向（Pion 扩展，非 W3C 字段） | `SetRemoteDescription` 处理每个 m-line 时写入 |
| `currentDirection` | `getCurrentDirection()` / `setCurrentDirection()` | **最终协商方向**（W3C `currentDirection`） | 仅在应用 answer（或远端 answer 到达本端 offer）时由 `setRTPTransceiverCurrentDirection` 写入 |
| `RTPSender.sendCalled` / `RTPReceiver.received`/`closedChan` | `hasSent()` / `haveReceived()` / `haveClosed()` | **实际收发状态**：发送/接收通道是否已建立或已关闭 | 协商完成后启动 RTP 时建立；`Stop()` 时不可逆关闭 |

方向枚举只有四个值（`rtptransceiverdirection.go:11-30`）：`Unknown(0)`、`Sendrecv`、`Sendonly`、`Recvonly`、`Inactive`。**本版本没有 W3C 后来引入的 `stopped` 枚举，也没有 `stopping/stopped` 布尔槽位**——`checkNegotiationNeeded` 中对应的 W3C 5.1/5.4 步骤整段被注释掉（`peerconnection.go:418-420`、`469-474`）。"永久停止"在本代码库中被建模为"方向置 `Inactive` + sender/receiver 不可逆 Stop"，而不是一个独立方向。

### 1.1 本地方向意图何时变

本地 API 路径在调用线程内**同步**修改 `direction`，不等协商：

- `RemoveTrack`（`peerconnection.go:2519-2544`）→ `sender.Stop()` + `setSendingTrack(nil)`，`setSendingTrack` 的状态表（`rtptransceiver.go:374-402`）把 `sendrecv→recvonly`、`sendonly→inactive`，并在 track 为 nil 时把 sender 从收发器摘除（`t.setSender(nil)`，`rtptransceiver.go:378-380`）。
- `AddTrack`（`peerconnection.go:2481-2516`）复用可发送收发器时走 `SetSender`→`setSendingTrack(track)`，把 `recvonly→sendrecv`、`inactive→sendonly`（`rtptransceiver.go:383-386`）。
- `Transceiver.Stop()`（`rtptransceiver.go:316-332`）立即把 `direction` 与 `currentDirection` 都置 `Inactive`，并 Stop sender/receiver。
- 处理**远端 offer** 时，`SetRemoteDescription` 内的 switch（`peerconnection.go:1326-1363`）也会改写本地 `direction`，例如远端 `recvonly` 把本地 `sendrecv→sendonly`、`recvonly→inactive`；远端 `sendrecv` 把 `inactive→recvonly`。

因此"本地方向意图"的生效点是**本地 API 返回时（或处理远端 offer 的 `SetRemoteDescription` 内联执行时）**，但此时 SDP 尚未交换，对端与实际媒体流都不受影响。

### 1.2 远端方向何时记录

`SetRemoteDescription` 遍历远端描述的每个 m-line，解析 `getPeerDirection`（`sdp.go:877-889`，缺省按 `sendrecv` 处理），按 mid 匹配收发器后立即 `setCurrentRemoteDirection(direction)`（`peerconnection.go:1322-1324`；新建收发器时在 `1341` 再写一次）。该值只反映"对端这份描述说了什么"，是 `isSendAllowed` 决定收发器能否被 `AddTrack` 复用的依据（见第 4.2 节）。

### 1.3 最终协商方向何时落定

`currentDirection` 只在描述交换的 answer 侧落定，入口有两个：

- 本端是 offerer：远端 answer 进入 `SetRemoteDescription`，重协商路径 `peerconnection.go:1401`（首次协商 `1442`）调用 `setRTPTransceiverCurrentDirection(..., true)`。
- 本端是 answerer：`SetLocalDescription(answer)` 中 `peerconnection.go:1189` 调用 `setRTPTransceiverCurrentDirection(..., false)`。

该函数（`peerconnection.go:1705-1758`）按 mid 找收发器，若本端是 offerer 则把 answer 方向反转（`sendonly↔recvonly`，`1737-1745`），再 `setCurrentDirection`。特殊规则：answerer 由远端 `recvonly` offer 新建、且没有 sender 的收发器，协商方向强制记为 `Inactive`（`1750-1752`），以便后续 `AddTrack` 复用。

### 1.4 实际收发何时启动/停止

协商方向落定与媒体通道开关是**两件事**，中间隔着 ops 队列：

- 发送：answer 应用后 `startRTPSenders`（`peerconnection.go:1923-1935`）只对 `isNegotiated() && !hasSent()` 的 sender 调 `Send()`。`negotiated` 标记在 SDP 生成阶段 `generateMatchedSDP`/`generateUnmatchedSDP` 里对每个进入 m-line 的 sender 调 `setNegotiated()`（`peerconnection.go:3400-3402`、`3426-3429`）。`Send` 成功后 `close(sendCalled)`（`rtpsender.go:301-371`），sender 即进入"已发送"不可逆状态。
- 接收：`configureRTPReceivers` 按 SDP 中的 SSRC/RID 建 `TrackRemote`（`rtpreceiver.go:177-194`），`startRTPReceivers`→`startReceive` 打开 SRTP read stream 并 `close(received)`（`rtpreceiver.go:197-286`），随后在首个 RTP 包到达或 `fireOnTrackBeforeFirstRTP` 时触发 `OnTrack`（`peerconnection.go:1667-1702`）。
- 停止：`RTPSender.Stop`（`rtpsender.go:378-407`）`close(stopCalled)`，已发送过则 `ReplaceTrack(nil)` 解绑、`UnbindLocalStream`、关闭 SRTP 写流；`RTPReceiver.Stop`（`rtpreceiver.go:378-427`）关闭全部 RTP/RTCP/repair read stream、`UnbindRemoteStream`，并 `closed.Store(true)`。此后 `TrackRemote.read` 直接返回 `io.EOF`（`track_remote.go:142-144`）。

时序小结（本端主动改方向，本端为 offerer）：

```
AddTrack/RemoveTrack/Stop()          CreateOffer            SetRemoteDescription(answer)
  direction 立即变化        ──► SDP 写出 a=方向      ──► currentDirection 落定
  onNegotiationNeeded()            sender.setNegotiated()      startRTPSenders / configureRTPReceivers
  （实际收发不变）                  （实际收发仍不变）           ops 队列异步 startRTP → 实际收发切换
```

---

## 2. 双向改单向/不活动后再协商：三者各自的生效时点

以 `TestPeerConnection_Renegotiation_DisableTrack`（`peerconnection_renegotiation_test.go:934-974`）与 `TestPeerConnection_MediaDirectionInSDP`（`peerconnection_go_test.go:1581-1714`）覆盖的场景为准。

### 2.1 本端主动 RemoveTrack（sendrecv → recvonly；sendonly → inactive）

1. `RemoveTrack` 返回时：`direction` 已变、sender 已 Stop 并被摘除；`currentDirection` **保持上一次协商值不变**。`TestPeerConnection_Regegotiation_ReuseTransceiver` 在 RemoveTrack 前后断言 `getCurrentDirection()` 仍为 `Sendonly`（`peerconnection_renegotiation_test.go:1130-1132`），运行通过（见第 6 节）。
2. 下一次 `CreateOffer`：`addTransceiverSDP` 用当前 `direction` 写 `a=recvonly`/`a=inactive`（`sdp.go:657`）；因为 sender 已为 nil，`addSenderSDP` 不再写 msid/SSRC（`sdp.go:458-467`）。`checkNegotiationNeeded` 正是通过本地描述中的方向/msid 与当前 `direction` 不一致判定需要协商（`peerconnection.go:429-465`）。
3. 对端 answer 回来后：`currentDirection` 才更新为协商结果；对端接收侧因新 SDP 中该 m-line 不再声明 SSRC，在 `configureRTPReceivers` 的重协商分支里判定 `receiverNeedsStopped`（`peerconnection.go:1796-1852`），Stop 旧 receiver 并**新建 receiver 挂回同一收发器**，旧 `TrackRemote` 的读循环收到 EOF。`TestPeerConnection_Renegotiation_RemoveTrack` 即断言 `trackClosed` 被触发（`peerconnection_renegotiation_test.go:516-535`）。

### 2.2 本端主动 Stop() 整个收发器

`Stop()` 一次性把 `direction` 和 `currentDirection` 都置 `Inactive`（`rtptransceiver.go:328-329`），sender/receiver 立即不可逆关闭。下一份 offer 该 m-line 带 `a=inactive` 且保留 mid 与 codec 列表——`DisableTrack` 测试断言 offer 中 `a=inactive` 计数为 1、对端 answer 同样回 `a=inactive`（`peerconnection_renegotiation_test.go:959-971`，本沙箱运行通过）。

### 2.3 远端 offer 把 m-line 改成 inactive（本端为 answerer）

这是本代码库最特殊、也最偏离 W3C 文字规范的一条路径。`SetRemoteDescription` 按 mid 找到既有收发器后，**只要远端方向是 `Inactive` 就立即调 `transceiver.Stop()`**（`peerconnection.go:1317-1321`）：

```go
} else if direction == RTPTransceiverDirectionInactive {
    if err := transceiver.Stop(); err != nil {
        return err
    }
}
```

即：远端一句"暂时 inactive"在本端被当作**停止既有收发实例**处理——发生在 answer 生成之前、协商尚未完成之时。随后 `currentRemoteDirection` 记为 inactive（`1323`），本端 answer 也只能回 `a=inactive`（收发器方向已被 Stop 改成 inactive）。`TestOfferWithInactiveDirection` 用手写远端 offer 验证了处理后本地 `direction == Inactive`（`peerconnection_go_test.go:1923-1952`，运行通过）。

但需注意：被 Stop 的是 **sender/receiver 实例**，收发器对象、mid 与 codec 槽位仍然存活，且接收侧在后续重协商中存在重建路径（详见 4.4），因此这条路径的准确表述是"媒体实例立即不可逆停用、收发器本体保留"，而非整个媒体身份消亡。

### 2.4 双方方向不兼容时的协商结果

方向交集由 SDP 应答规则隐式表达，没有独立的"协商失败"错误：

- 远端 `sendonly`（对端只发）与本地只能发的意图（无 receiver 或本地 `sendonly`）组合时，本地应答方向收敛为 `inactive`；`satisfyTypeAndDirection` 的偏好表（`rtptransceiver.go:447-480`）对远端 `sendonly` 只接受本地 `recvonly` 收发器，对远端 `inactive` 返回空偏好列表（`465-467`），即不匹配任何现有收发器。
- 双方都 `sendonly`：应答 m-line 为 `inactive`，谁也收不到谁。九种方向组合的本地 `direction` 收敛由 `TestPeerConnection_TransceiverDirection` 全量覆盖（`peerconnection_go_test.go:1462-1579`，本沙箱运行通过）。
- 编解码不交集（比方向不兼容更硬的拒绝）：本地无可用 codec 且该 m-line 带 sender 时，`addTransceiverSDP` 直接返回 `ErrSenderWithNoCodecs`，`CreateOffer/CreateAnswer` 失败（`sdp.go:582-586`，测试 `TestPeerConnection_CreateOffer_NoCodecs`，`peerconnection_media_test.go:1553-1575`，运行通过）；无 sender 时则生成 **port 0 拒绝 m-line**（见第 3.2 节）。

---

## 3. 旧媒体身份与接收轨道：保留、停用还是重建

### 3.1 收发器身份（mid）默认保留

- mid 一旦设定不可更改：`SetMid` 对已设 mid 返回 `errRTPTransceiverCannotChangeMid`（`rtptransceiver.go:252-255`，错误定义 `errors.go:279`）。
- 重协商按 mid 配对（`findByMid`，`rtptransceiver.go:435-443`；answer 生成侧 `peerconnection.go:3396-3399`），收发器对象本身在方向变化、RemoveTrack、receiver 重建中都保持同一指针。`TestPeerConnection_Renegotiation_CodecChange` 两轮 RemoveTrack/AddTrack 后断言两端收发器数量仍为 1、mid 仍为 `"0"`（`peerconnection_renegotiation_test.go:468-474`）。
- 编码能力随收发器保留：codec 列表存于 `RTPTransceiver.codecs`（`rtptransceiver.go:27`），`getCodecs()`（`78-102`）不依赖 sender/receiver 的存活；`Stop()` 不清空它。因此方向变为 inactive 后，原 codec 偏好仍在，下一份 SDP 仍带同样的 rtpmap/fmtp。

### 3.2 接收轨道（RTPReceiver / TrackRemote）的三种命运

1. **SSRC 仍在新 SDP 中：保留并就地更新身份。** 重协商分支先按 SSRC/RID 回查 `trackDetails`，命中则只刷新 `id/streamID`，receiver 不动（`peerconnection.go:1810-1837`）。
2. **SSRC 从新 SDP 消失：停用并重建 receiver，但收发器保留。** `receiverNeedsStopped=true` → `receiver.Stop()`（旧 TrackRemote 读到 EOF）→ `api.NewRTPReceiver` 新建并 `transceiver.setReceiver`（`peerconnection.go:1839-1851`）。新 SSRC 到来时由 `startRTPReceivers` 在新 receiver 上启动并再次触发 `OnTrack`。`CodecChange` 测试验证两次 OnTrack 给出不同 track id（`peerconnection_renegotiation_test.go:454-489`）；Simulcast 的 RID 变更走同一机制（`1071-1107`）。
3. **m-line 被 port 0 拒绝：m-line 占位保留、无活动媒体。** 本地生成侧有两条 port 0 路径：无 codec 且无 sender 时手工构造 `m=<kind> 0 ...`、formats 为 `0`、带 `c=0.0.0.0`（`sdp.go:595-609`，测试 `TestPopulateSDP/rejected_track`，`sdp_test.go:967-999`，运行通过）；m-line 不在 BUNDLE 组内时被 `populateSDP` 改成 port 0（`sdp.go:786-791`）。注意 RFC 9143 的 **bundle-only 零端口不是拒绝**：`mediaSectionCryptexState` 对带 `a=bundle-only` 的零端口 m-line 仍按活动媒体处理（`peerconnection.go:1517-1522`），并有专门测试（`peerconnection_go_test.go:2887-2925`、`3938-3972`）。

### 3.3 发送侧身份

- `RemoveTrack` 后 sender 被 Stop 且从收发器摘除；再次 `AddTrack` 复用时是**新建 RTPSender**（`peerconnection.go:2493-2499`），新 sender 有新的随机 id（`rtpsender.go:67`），但收发器/mid 不变。
- 不复用、新建收发器的条件见 4.2。SSRC 是否复用因此取决于是否复用了同一收发器：`ReuseTransceiver` 测试断言复用路径 SSRC 相同、新建路径 SSRC 不同（`peerconnection_renegotiation_test.go:1155-1186`）。

---

## 4. 暂时不活动 vs 永久停止；恢复条件

### 4.1 两种"不活动"在本代码库中的本质区别

| 维度 | 暂时不活动（可恢复） | 永久停止 |
|---|---|---|
| 典型入口 | `RemoveTrack`（sender 侧）；对端移除 SSRC 导致的 receiver 重建 | `Transceiver.Stop()`；`Close()`；**远端 offer 带 `a=inactive`（`peerconnection.go:1317-1321`）** |
| `direction` | `sendrecv→recvonly` 或 `sendonly→inactive`，可再改回 | 强制 `inactive`（后续 offer 可再被 switch 改回，见 4.4） |
| sender | 被摘除并 Stop，但收发器可再 `SetSender` | Stop；且收发器仍挂着这个已 Stop 的 sender（`Stop()` 不摘 sender），`AddTrack` 无法复用，只能靠显式 `SetSender` 换新实例 |
| receiver | SSRC 消失时 Stop 后**立即有新 receiver 顶替** | Stop 后当次无顶替；但下一次重协商若新 SDP 不再含旧 SSRC，`configureRTPReceivers` 会用新 receiver 顶替（见 4.4） |
| 能否被 `AddTrack` 复用 | 满足 4.2 条件时可以 | 不能：`isSendAllowed` 要求 `t.Sender() == nil`（`rtptransceiver.go:404-407`），Stop 后的收发器 sender 非 nil 且已 `hasStopped()`，对该 sender 再操作返回 `errRTPSenderStopped`（`rtpsender.go:165-166`） |
| codec 能力 | 保留 | 保留（`Stop()` 不清 `RTPTransceiver.codecs`，新 receiver/后续 SDP 仍用同一套编码能力） |

### 4.2 恢复发送同时保留原编码能力的条件

`AddTrack` 复用既有收发器的全部条件在 `isSendAllowed`（`rtptransceiver.go:404-433`）：

1. kind 匹配且**当前没有 sender**（RemoveTrack 路径满足；Stop 路径不满足）；
2. `currentDirection` 不是 `sendrecv/sendonly`（即从未在当前收发器上处于发送态，或已被协商成 recvonly/inactive）；
3. `currentRemoteDirection` 不是 `sendonly/inactive`——对端必须仍愿意接收。注释明确列出：远端 `sendrecv/recvonly` 可发，远端 `sendonly/inactive` 不可发（`rtptransceiver.go:419-430`）。

`TestAddTransceiverAddTrack_Reuse/reuse_remote_direction_test` 用两组用例固化了第 3 条：远端 `sendrecv` 时复用同一收发器（数量 1），远端 `sendonly` 时新建第二个收发器（数量 2）（`peerconnection_media_test.go:721-846`，纯本地子测试运行通过；含完整信令的子测试在本沙箱受 ICE 限制，见第 6 节）。

因此"**恢复收发 + 保留原编码能力同时成立**"的充要条件是：

- 走的是 RemoveTrack/SSRC 变化的暂时不活动路径，**没有对该收发器调用过 `Stop()`、也没收到过远端 `a=inactive` offer**；
- 之后的协商中远端方向恢复为可接收（`sendrecv` 或 `recvonly`），使 `currentRemoteDirection` 解除封锁；
- 重协商完成（answer 应用、`currentDirection` 更新）后再 `AddTrack`，复用同一 mid 的收发器——`codecs` 槽位从未被清空，原 `SetCodecPreferences` 能力自然保留；
- 或者在 receiver 侧：新 SDP 重新声明 SSRC，使 `configureRTPReceivers` 在顶替上来的新 receiver 上 `startReceive`（收发器与 codec 不变，变的是 receiver 实例和 TrackRemote）。

### 4.3 远端拒绝媒体段（port 0）的后果

- 本地作为发送方收到含 port 0 m-line 的远端描述时，该 m-line 仍参与 mid 配对与方向解析（`SetRemoteDescription` 的循环不检查端口），但 `trackDetailsFromSDP` 不会为其产生入站 track，`startRTP` 也不会有可启动的入站流；对端明确不接收，本端 sender 即便 `negotiated` 也只是向一个被拒绝的段发送（实际 SRTP 流无消费者）。
- 本代码库**不会因为收到 port 0 就主动 Stop 本地收发器**——触发 Stop 的是 `a=inactive` 方向属性（`peerconnection.go:1317`），而非端口值。这是与"远端 inactive"路径的重要区别：port 0 拒绝在本地留下的是方向可再协商的休眠收发器（前提是对端后续 offer 重新开放该 m-line）。
- 边界（源码推断，无运行证据）：`getPeerDirection` 对不带方向属性的 port 0 m-line 缺省返回 `sendrecv`（`sdp.go:888`），因此被拒绝段的 `currentRemoteDirection`/`currentDirection` 可能记录为 sendrecv 而非 inactive；本库测试未覆盖"收到 port 0 后再重新开放同一 mid"的往返场景。

### 4.4 远端 inactive 后再激活：收发器保留，接收侧可恢复、发送侧受限

按第 2.3 节，远端 `a=inactive` 已在 answer 生成前触发本地 `transceiver.Stop()`：sender/receiver 实例不可逆关闭，但收发器、mid、codec 槽位都还在。远端下一份 offer 把同一 mid 改回 `sendrecv/sendonly` 时，两条数据路径的恢复能力不对称：

**接收侧（对端重新开始发）——源码路径上可以在同一 mid 恢复：**

1. `SetRemoteDescription` 按 mid 匹配到原收发器，更新 `currentRemoteDirection`，并由 switch 把本地 `direction` 改回 `recvonly`（inactive→recvonly，`peerconnection.go:1359-1362`）。
2. 本端 `CreateAnswer`/`SetLocalDescription(answer)` 进入 `configureRTPReceivers` 的重协商分支（`peerconnection.go:1796-1852`）：旧 receiver 已 closed，其名下 TrackRemote 的 SSRC/RID 在新 SDP 中查不到（inactive 段本就不带 SSRC；再激活通常带新 SSRC），`receiverNeedsStopped` 置位 → 对旧 receiver 再调 `Stop()`（幂等，`rtpreceiver.go:383-387`）→ `api.NewRTPReceiver` 新建并 `transceiver.setReceiver` 挂回**同一收发器**。
3. 随后 `startRTPReceivers`→`runIfNewReceiver`：新 receiver `haveReceived()==false`、方向为 recvonly/sendrecv（`peerconnection.go:1771-1775`），条件满足即 `startReceive` 并重新触发 `OnTrack`。编码能力沿用收发器的 codec 槽位，未丢失。

**接收侧的失败边角（已用临时状态机测试验证，测试代码运行后已删除，未留在交付物中）：** 若再激活 offer 复用了 Stop 之前的**同一 SSRC**，第 2 步的 `trackDetailsForSSRC` 会在新 SDP 中命中旧 track，`receiverNeedsStopped` 保持 false，旧 receiver 不会被替换；而 `runIfNewReceiver` 对 `haveReceived()==true` 的旧 receiver 直接跳过（`peerconnection.go:1774`），该 SSRC 的入站流无法启动。实测两个场景均成立：新 SSRC 再激活后 `receiverSame=false`、新 receiver `haveClosed()==false`、方向恢复 recvonly、收发器数量仍为 1；同 SSRC 再激活后 `receiverReplaced=false`，旧 receiver 被保留。换句话说，恢复成立依赖"再激活伴随新 SSRC/RID"这一常见但非必然的前提。

实测同时暴露一个细节：若 receiver 从未 `startReceive`（没有任何 RTP 到达过），`RTPReceiver.Stop` 走 `rtpreceiver.go:420-421` 的 default 分支，只 `close(closedChan)` 并置 `closed`，不关闭任何 read stream；这不影响 inactive 即 Stop 的方向语义，但意味着"未接收过媒体的 receiver"被停止后其资源释放路径更轻。

**发送侧（本端想重新发）——同一收发器上无法靠常规协商恢复：**

- Stop 不摘除 sender，`isSendAllowed` 的 `t.Sender() != nil` 条件恒 false（`rtptransceiver.go:405`），`AddTrack` 只会新建另一个收发器（新 mid）；对旧 sender 直接 `AddEncoding` 等操作返回 `errRTPSenderStopped`（`rtpsender.go:165-166`）。
- 同 mid 发送恢复只能绕过 `AddTrack` 走导出 API `Transceiver.SetSender(newSender, track)`（`rtptransceiver.go:224-228`）：其内部先经未导出的 `setSender` 原子替换 sender（旧 stopped sender 仅被摘除 transceiver 反向指针，不再参与任何操作），再对**新** sender 调 `setSendingTrack`/`ReplaceTrack`，因此不会触碰旧 sender 的 stopped 状态；应用随后还需把 `direction` 调回 `sendonly/sendrecv` 才能在 offer 中重新写出发送方向。这属于应用层显式操作，不属于 RemoveTrack/重协商自动流程。

**测试覆盖现状：** 仓库内没有任何常驻测试覆盖 inactive→active 的再激活往返（已全量检索 `*_test.go`）。本次分析曾编写临时测试 `TestTmpVerifyInactiveReactivationStateMachine`/`...SameSSRC`（不等待 ICE 连通，仅驱动 SDP 状态机）实测验证：新 SSRC 场景 receiver 被替换、同 SSRC 场景 receiver 保留，两个用例均 PASS 后测试文件已删除，结论留存于本报告但**不作为仓库内可复跑的测试资产**。接收侧恢复链所复用的 `configureRTPReceivers` 重建逻辑本身由 `TestPeerConnection_Renegotiation_CodecChange`、Simulcast RID 变更测试覆盖，但这些测试在本沙箱因 ICE 限制未跑通（见第 6 节）。

---

## 5. 关键代码与测试材料索引

| 主题 | 实现位置 | 既有测试 |
|---|---|---|
| 三套方向字段 | `rtptransceiver.go:19-38, 346-372` | `rtptransceiverdirection_test.go:12-70` |
| 本地方向状态表（Add/RemoveTrack） | `rtptransceiver.go:374-402` | `peerconnection_media_test.go:685-847` |
| 远端 offer 改写本地方向 + inactive 即 Stop | `peerconnection.go:1297-1370` | `peerconnection_go_test.go:1462-1579, 1923-1952` |
| 协商方向落定（含反转、无 sender 记 inactive） | `peerconnection.go:1705-1758` | `peerconnection_renegotiation_test.go:1130-1132`；`peerconnection_go_test.go:3938-3972` |
| sender 协商标记与启动 | `peerconnection.go:1923-1935, 3400-3429`；`rtpsender.go:86-97, 301-407` | `rtpsender_test.go` |
| receiver 重协商停用/重建、EOF | `peerconnection.go:1789-1877`；`rtpreceiver.go:378-427`；`track_remote.go:142-144` | `peerconnection_renegotiation_test.go:402-536, 976-1108` |
| port 0 拒绝 / bundle-only 例外 | `sdp.go:582-612, 786-791`；`peerconnection.go:1514-1534` | `sdp_test.go:967-999`；`peerconnection_go_test.go:2853-2925` |
| 无 codec 硬失败 | `sdp.go:582-586` | `peerconnection_media_test.go:1553-1575` |
| 收发器复用门禁 | `rtptransceiver.go:404-433` | `peerconnection_media_test.go:721-846`；`peerconnection_renegotiation_test.go:1110-1189` |
| Stop 不可逆 / Close 全量 Stop | `rtptransceiver.go:316-332`；`peerconnection.go:2847-2858` | `peerconnection_renegotiation_test.go:934-974` |
| negotiation-needed 判定（stopping 步骤注释） | `peerconnection.go:393-478` | `peerconnection_renegotiation_test.go:847-898, 1306-1330` |

---

## 6. 运行证据边界（未取得证据之处明确标注）

### 6.1 本沙箱内已实际运行并通过的测试（go1.25.9，修正 GOROOT 后）

- `go build ./...`：通过。
- `TestPeerConnection_TransceiverDirection`（9 个方向组合全 PASS）、`TestOfferWithInactiveDirection`、`TestPeerConnection_Renegotiation_DisableTrack`：PASS。
- `TestPeerConnection_satisfyTypeAndDirection`、`TestPeerConnection_MediaDirectionInSDP`（2 子用例）、`TestPeerConnection_CreateOffer_NoCodecs`、`TestNegotiationNotNeededAfterReplaceTrackNil`、`TestNewRTPTransceiverDirection`、`TestRTPTransceiverDirection_String`、`TestPopulateSDP`（含 `rejected_track` 等 11 子用例）、`TestAddTransceiverAddTrack_Reuse/reuse_test`：PASS。
- 一次性临时验证（测试文件已删除，仅结论留存）：inactive→再激活状态机两个用例 PASS——新 SSRC 时 closed receiver 被新实例替换、同 mid 收发器保留且方向恢复 recvonly；同 SSRC 时旧 receiver 不被替换。

### 6.2 受环境限制未取得运行证据的测试

下列测试依赖真实 ICE/DTLS 连通与 RTP 收发，在本沙箱内挂起。抓取的 goroutine 堆栈显示阻塞点为 `ICETransport.StartContext`（`icetransport.go:165`，经 `startTransports`→ops 队列）以及 `sendVideoUntilDone` 等待首个 RTP 包；即 localhost UDP 连通性受限，**不是断言失败**：

- `TestPeerConnection_Renegotiation_RemoveTrack`（30s 用例超时触发 `panic: timeout`）、`TestPeerConnection_Regegotiation_ReuseTransceiver`、`TestPeerConnection_Renegotiation_CodecChange`、`TestAddTransceiverAddTrack_Reuse/reuse_remote_direction_test`、`TestNegotiationNeededRemoveTrack`（600s 包级超时被终止）。
- 连带影响：第 2.1 节 receiver 重建后 TrackRemote EOF、第 3.2 节 OnTrack 再次触发、SSRC 复用/不复用等**运行时行为**在本环境未亲验，报告中相关结论依据源码与测试断言文本给出，可信但无本地运行背书。

### 6.3 仓库本身缺失的测试覆盖（即使环境正常也无现成运行证据）

1. 远端 `a=inactive` 触发 Stop 后，同一 mid 再激活（inactive→sendrecv/sendonly）的往返在**常驻测试套件中无覆盖**。第 4.4 节的结论已由一次性临时测试实测（新 SSRC 可恢复、同 SSRC 不可恢复，PASS 后删除），但仓库本身不携带该回归测试；发送侧"仅能显式 SetSender"的限制仍为源码推断（临时测试中即通过 `setSender`+`setDirection` 手工恢复后才生成再激活 offer）。
2. 收到 port 0 拒绝 m-line 后重新开放该 mid 的协商往返；port 0 段缺省方向被记为 sendrecv 的实际后果（第 4.3 节）。
3. `RTPTransceiverDirection` 无 `stopped` 枚举、W3C stopping/stopped 检查被注释（`peerconnection.go:418-420, 469-474`）与浏览器互操作时的差异，本仓库无互操作测试，e2e 目录需浏览器环境（`e2e/Dockerfile` + `test.html`），本次未运行。
4. 本报告未修改任何产品代码；所有结论限定于提交 `60271fb` 的源码静态关系与上述已运行测试。
