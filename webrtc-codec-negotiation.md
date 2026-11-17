# WebRTC 远端编解码协商、格式参数匹配与 RTX 关联的责任边界

分析对象：本仓库（`github.com/pion/webrtc/v4`，分支 `task/24`）。
方式：只读源码 + 复用仓库已有测试；另外用临时进程内探针（`package webrtc` 内部测试，构造 SDP 文本后调用 `updateFromRemoteDescription` / `SetRemoteDescription` / `CreateAnswer` 等纯本地 API，不建真实对等端、不连外网）逐条验证下文标注为「探针验证」的结论。探针文件已删除，仓库工作区保持干净。

> 说明：文中所有「已证实」均为本仓库源码可直接读出、且（除特别注明外）我已用探针复现的行为。凡是只能由测试间接证明、或完全没有覆盖的判据，单独在 §10 汇总。

---

## 0. 结论速览

1. 协商的最小判定单元是 **媒体段（m= 行）× 负载类型（PT）**，但结果的存储单元是 **kind 级的扁平列表**（`videoCodecs`/`audioCodecs`、`negotiatedVideoCodecs`/`negotiatedAudioCodecs`）。这个「按段判定、按 kind 存储」的不对称是所有边界问题的根源。
2. 「精确匹配」不等于名字相同：每种 MIME 有各自的 fmtp 兼容规则（§3.2）。不满足精确匹配时还有**部分匹配**兜底（仅 MIME + 时钟率 + 声道数），且部分匹配在**该段存在任何精确匹配时会整体丢弃**（§3.3）。
3. 名字相同但时钟率/声道数不同 → 既不精确也不部分匹配 → **不报错**，只把该 kind 标记为「已协商」但编码器列表为空；后续表现为 `ErrNoCodecsAvailable` 或拒绝媒体段（§7）。
4. `apt` 只影响**发送侧 RTX 关联**，且 `findRTXPayloadType` 要求 fmtp **整串等于 `apt=N`**；`apt=96;rtx-time=2000`、`APT=96`、参数顺序变化都会导致「SDP 里通告了 RTX/ssrc-group，但修复流 PT=0，无法真正发 RTX」（§5.2）。
5. 接收侧 RTX 关联**完全不看 apt**，只看 `a=ssrc-group:FID` 的 SSRC 配对（§5.3）。因此「apt 匹配」与「SSRC 配对」证明的是两件事。
6. 三个层次的证明力互不可替代：**编解码匹配**证明格式配置兼容；**负载类型分配**只证明 SDP 声明；**RTX 关联**在发送侧靠 apt 字符串、在接收侧靠 SSRC 组。任何一层都不能推出「可以互通」。
7. SDP 通告的 PT 与真正上线的 PT 可以不一致：transceiver 的 `getCodecs()`（决定 SDP）与 `TrackLocal.Bind` 用的 `mediaEngine.getRTPParametersByKind()`（决定报文头部）是两个来源；用 `SetCodecPreferences` 指定自定义 PT 时，answer 会声明该 PT，但发送侧仍用 kind 级协商列表里的 PT（§6.2，探针验证）。
8. 错误可见性差：`apt` 解析失败会**抛错但状态已提交、无法回滚**；时钟率不兼容、fmtp 取值冲突、同 PT 同 MIME 不同 fmtp、RTX fmtp 非规范写法全部**静默降级**（§7）。
9. 旧行为（`DisableMediaEngineMultipleCodecs`）下，只有**第一个音频段和第一个视频段**参与协商，后续同 kind 媒体段直接沿用第一个段的编码器集合，answer 会在后续段里回写第一段的 PT/编码器（§8，探针已验证该行为会产出协议上不应出现的 answer）。
10. 存在一处已复现的**内存数据被就地改写**缺陷：`getCodecs()` 在「未设置 codec preferences」分支把 MediaEngine 的**实时切片**交给 `filterUnattachedRTX`，后者用 `append` 原地删除元素，导致「未挂接的 RTX 位于列表中段」时列表末元素被复制、并泄漏进后续生成的 SDP（§6.4）。

---

## 1. 责任链路总览

远端 SDP 进入本地后，编码器相关处理分五步，每步职责不同：

| 步骤 | 入口 | 职责 | 输出 |
|---|---|---|---|
| ① 段内解析 | `codecsFromMediaDescription`（`sdp.go:1130`） | 把 `m=` 行的每个 PT 与 `a=rtpmap`/`a=fmtp`/`a=rtcp-fb` 合成 `RTPCodecParameters` | 该段远端编码器列表（PT、远端 fmtp 原样保留） |
| ② 逐段匹配 | `matchRemoteCodec`（`mediaengine.go:463`） | 处理 apt、调用 `codecParametersFuzzySearch`，给出 `Exact`/`Partial`/`None` | 匹配类型 + 命中的本地编码器 |
| ③ 段级选择 | `updateFromRemoteDescription`（`mediaengine.go:606`） | 两遍扫描；「有精确匹配就只用精确匹配」；把结果 push 进 kind 级列表 | `negotiatedVideoCodecs`/`negotiatedAudioCodecs` |
| ④ SDP 生成 | `RTPTransceiver.getCodecs`（`rtptransceiver.go:78`）+ `sdp.go` 的 `WithCodec` | 决定 answer/offer 里出现哪些 `a=rtpmap`/`a=fmtp` | 声明集合（**声明**，不等于**行为**） |
| ⑤ 线上行为 | `TrackLocal.Bind`（`track_local_static.go:90`）→ `RTPSender.Send`（`rtpsender.go:301`）；接收侧 `TrackRemote.checkAndUpdateTrack`（`track_remote.go:173`） | 决定实际写入的 PT/RTX PT/SSRC，以及按 PT 反查编码器 | 报文头 + `StreamInfo` |

关键事实：④ 与 ⑤ 读取的**不是同一份数据**。④ 读 transceiver 的 `codecs`（可能来自用户 `SetCodecPreferences`），⑤ 读 `mediaEngine.getRTPParametersByKind()`（kind 级协商列表）。这是 §6.2 中 PT 声明与实际上线不一致的原因。

---

## 2. 音频与视频分区：共享什么、隔离什么

源码结构（`mediaengine.go:32-44`）：

- **隔离**：本地能力 `videoCodecs`/`audioCodecs` 分列表；协商标志 `negotiatedVideo`/`negotiatedAudio` 分开；`getCodecsByKind`、`isRTXEnabled`、`getRTPParametersByKind` 都按 kind 取值。
- **共享**：PT → 编码器的反查表是**两个 kind 各自的列表**，但反查函数 `getCodecByPayload`（`mediaengine.go:404`）先查 video、再查 audio，且**不接收 kind 参数**。

后果（探针验证）：当远端在 audio 段用 PT 96 协商 opus、在 video 段用 PT 96 协商 VP8 时：

```
negotiatedAudio=[pt=96 audio/opus 48000]
negotiatedVideo=[pt=96 video/VP8 90000]
getCodecByPayload(96) -> video/VP8        // 永远返回 video
getRTPParametersByPayloadType(96) -> [pt=96 video/VP8]
```

接收侧 `TrackRemote.checkAndUpdateTrack` 与 `PeerConnection.handleIncomingSSRC`（`peerconnection.go:2113`）都只按 PT 调用该函数，因此**跨 kind 复用同一 PT 时，audio 报文会被解析成 video 编码器**，且没有任何告警。RFC 3551/8866 允许 PT 在不同媒体段独立编号，所以这是「合法输入 + 错误结果」，属于必须靠调用方自律（保证 PT 在 kind 内唯一）的边界。

视频侧还多一层：`m=video` 段的 rtpmap 名字不必是视频编码器——`MimeType` 由 `mediaDesc.MediaName.Media + "/" + codec.Name` 拼成（`sdp.go:1169-1175`）。探针验证：`m=video` 段里写 `a=rtpmap:111 opus/48000/2` 会被解析成 `video/opus`，随后匹配不到任何视频编码器，**不报错**，该段 negotiated 为空。即「媒体种类来自 m= 行，不来自 rtpmap 名字」。

---

## 3. 匹配判定：精确、部分、不可用

### 3.1 三态定义（`rtpcodec.go:108-145`）

| 状态 | 判据 | 用途 |
|---|---|---|
| `codecMatchExact` | 调用 `fmtp.Parse(...).Match(...)`，即**按 MIME 的语义规则**判定 | 优先采用 |
| `codecMatchPartial` | `strings.EqualFold(MimeType)` + `fmtp.ClockRateEqual` + `fmtp.ChannelsEqual`，**忽略 fmtp** | 兜底 |
| `codecMatchNone` | 以上都不满足 | 丢弃 |

`ClockRateEqual`/`ChannelsEqual`（`internal/fmtp/fmtp.go:53-89`）对 `0` 做默认值填充：`0 → 48000(opus)/8000(pcmu,pcma)/90000(其它)`；声道 `0 → 2(opus) / 1(其它 audio) / 0(video)→1`。这是「用户/对端省略参数」的兼容处理，源码注释明确写了「希望在将来大版本移除」。

### 3.2 各 MIME 的「精确」判据（不是名字相同就精确）

| MIME | 精确匹配要求 | 被忽略的字段 | 依据 |
|---|---|---|---|
| H264 | 双方的 `packetization-mode`、`profile-level-id` **都必须存在**；pk-mode 相等；profile-level-id 的**前两字节**（profile + constraint）相等 | level 字节（第三个字节）；其余参数如 `level-asymmetry-allowed` | `internal/fmtp/h264.go`，RFC 6184 §8.2.2 |
| H265 | `profile-id`（默认 `1`）、`tier-flag`（默认 `0`）、`tx-mode`（默认 `SRST`）相等 | `level-id` 等 | `internal/fmtp/h265.go`，draft-hevc-webrtc-09 + W3C candidate correction 52 |
| VP9 | `profile-id`（缺失视为 `0`）相等 | 其余 | `internal/fmtp/vp9.go` |
| AV1 | `profile`（缺失视为 `0`）相等 | 其余 | `internal/fmtp/av1.go` |
| 其它（Opus/PCMU/PCMA/VP8/RTX/自定义） | MIME + 时钟率 + 声道数 + **参数双向子集相等** | — | `internal/fmtp/fmtp.go:91-105,173-183` |

注意最后一行是**子集语义**，不是全等：`paramsEqual` 只校验「双方共有的键取值相同」，**单侧多出来的键不算冲突**。探针验证：

```
本地 opus fmtp="minptime=10;useinbandfec=1"，远端 fmtp="minptime=10"
-> 精确匹配，negotiated 采用远端 fmtp "minptime=10"（本地多出的 useinbandfec 被丢弃）
```

而取值冲突时：`本地 minptime=10` vs `远端 minptime=99` → 精确失败 → 回落到部分匹配（MIME+clock+channels 相同）→ 若该段没有别的精确匹配，**冲突值被直接接受**，`negotiated` 写入远端 fmtp `minptime=99`。这属于规范上应当拒绝、实现上却静默接受的判据（§10 列为待补）。

### 3.3 段级选择：精确匹配会「一票否决」部分匹配

`mediaengine.go:694-703`：

```go
switch {
case len(exactMatches) > 0:   err = m.pushCodecs(exactMatches, typ)
case len(partialMatches) > 0: err = m.pushCodecs(partialMatches, typ)
default:                      continue   // 该段不贡献任何编码器
}
```

即：**只要该 kind 的段里出现过任意一个精确匹配，整段的部分匹配全部丢弃**。探针验证（同段内 VP8 精确 + H264 仅部分匹配 profile-level-id）：

```
远端 m=video ... 96 98；96=VP8，98=H264 profile-level-id=640c1f
本地 H264 profile-level-id=42e01f
-> negotiatedVideoCodecs = [pt=96 video/VP8]        // H264 整条消失
-> getCodecByPayload(98) = codec not found
```

仓库已有测试覆盖了这一点：`mediaengine_test.go:250`「Prefers exact codec matches」。同一规则也解释了 `mediaengine_test.go:390`「Matches when rtx apt for partial match codec」为何断言 PT 97 查不到（VP8 精确命中后，VP9 的部分匹配连同其 RTX 被整体丢弃）。

如果**整段只有部分匹配**，则该部分匹配会被采纳，并且**远端（不兼容）的 fmtp 会被原样写进 negotiated 列表、再写进 answer**。探针验证（本地 H264 pk-mode=1，远端只给 pk-mode=0）：

```
answer: a=rtpmap:96 H264/90000 + a=fmtp:96 packetization-mode=0;profile-level-id=42e01f
streamInfo.SDPFmtpLine = "packetization-mode=0;..."   // 发送侧也按远端 fmtp 建流
```

也就是说：部分匹配是「接受但不保证语义一致」的口子——本地打包器仍按自己的策略打包，远端声明的 packization-mode 并没有被强制。

### 3.4 「不可用」的三种形态

| 形态 | 触发 | 表现 |
|---|---|---|
| 段级不可用 | 三态全为 None | 该段不 push；kind 标志仍可能被置 `true`（见下） |
| kind 级不可用 | 所有同 kind 段都无匹配 | `negotiatedX=true` 但列表为空；`getCodecsByKind` 返回空（`mediaengine.go:716-735`） |
| 结构不可用 | `m=` 行里的 PT 在本段找不到 rtpmap/fmtp（且 PT≠0） | 返回 `errSDPPayloadTypeNotFound`（`sdp.go:1147-1148`） |

`updateFromRemoteDescription` 在解析编码器**之前**就把 `negotiatedAudio/negotiatedVideo` 置 `true`（`mediaengine.go:620-637`）。因此「远端有我认识的 kind，但一个编码器都不兼容」会得到：`SetRemoteDescription` 成功 → 之后 `AddTransceiverFromKind` 返回 `ErrNoCodecsAvailable` → 若已有 sender 则 `CreateAnswer` 输出 `m=audio 0`（探针验证：opus/16000/1 vs 本地 48000/2，answer 为 `m=audio 0 UDP/TLS/RTP/SAVPF 0`）。

---

## 4. 重复负载类型与出现冲突时的处理

### 4.1 注册侧（本地能力）

`addCodec`（`mediaengine.go:239-253`）只比较 `PayloadType`，同 PT 时比较 MIME（大小写不敏感）+ 时钟率 + 声道数：

| 情形 | 结果 |
|---|---|
| 同 PT + 同 MIME + 同时钟 + 同声道 | 静默去重（返回原列表） |
| 同 PT + 不同 MIME / 时钟 / 声道 | `ErrCodecAlreadyRegistered` |
| **同 PT + 同 MIME + 不同 fmtp** | **静默忽略第二次注册**（fmtp 不参与比较） |

第三行是探针验证的：先后注册 `H264 pk-mode=1/42e01f@102` 与 `H264 pk-mode=0/64001f@102`，第二次返回 `nil`，列表保持第一条。也就是说本地能力表里**同一 PT 只能有一种 fmtp**，第二种能力在注册阶段就丢失了，且无告警。

同 PT 跨 kind 合法（两个列表独立），这与 §2 的接收侧歧义叠加。

### 4.2 协商侧（远端 SDP → negotiated 列表）

`pushCodecs` 复用 `addCodec`（`mediaengine.go:588-603`），错误用 `errors.Join` 聚合后返回给 `SetRemoteDescription`：

- 远端不同媒体段（或不同 kind）给**同一 PT 配不同 MIME/时钟/声道** → `SetRemoteDescription` 返回 `ErrCodecAlreadyRegistered`。探针验证：两个 video 段都用 PT 96，一段 VP8、一段 VP9 → `codec already registered for same payload type`。
- 同一 PT、同 MIME、**不同 fmtp** → 无错误，保留先到者。探针验证：两个 video 段 PT 96 均为 H264，但 pk-mode/profile 不同 → 只保留第一段的 fmtp。
- 重协商时同一 PT 换了编码器 → 同样触发 `ErrCodecAlreadyRegistered`。探针验证：先 VP8@96，重协商改 VP9@96 → `SetRemoteDescription` 报错（此时信令已进入 `have-remote-offer`，见 §7.3）。

结论：**PT 冲突的可见性取决于「是否跨 MIME/时钟/声道」**；纯 fmtp 差异一律静默。这与 RFC 8866 的「PT 语义由本段 rtpmap 定义」并不完全一致，是 Pion 用 kind 级扁平表存储的直接代价。

### 4.3 同一段内的重复声明

`codecsFromMediaDescription` 依赖 `sdp.GetCodecMap()`（pion/sdp），后者对同一 PT 的 fmtp 做「先到先得」合并：探针验证同一段里写两行 `a=fmtp:96`，只有第一行生效。`m=` 行里同一 PT 重复出现则会生成两个相同的 `RTPCodecParameters`，后续 `addCodec` 会去重。

---

## 5. `apt` 与 RTX：三层各自证明什么

### 5.1 协商时的 apt 处理（`mediaengine.go:479-529`）

- 用 `strconv.ParseUint(apt, 10, 8)` 解析。失败（`apt=abc`、`apt=`、`apt=300`）→ **直接返回错误**，`SetRemoteDescription` 失败。
- apt 目标只在**本段已匹配到的** `exactMatches`/`partialMatches` 里找，找不到 → `codecMatchNone` 且**不报错**（注释：「not an error, we just ignore this codec」）。
- 找到后，若 apt 目标的匹配类型等于 RTX 自身的匹配类型，则把 `apt=<远端PT>` 改写为 `apt=<本地PT>`，再对本地列表做模糊搜索。
- 「apt 目标是部分匹配 ⇒ RTX 也降级为部分匹配」。

因此：**RTX 能否进入 negotiated 列表，取决于「同段内它的主编码器是否也匹配成功」**，而不是取决于 apt 是否指向本地已注册的 PT。由于有两遍扫描（`mediaengine.go:661-692`），RTX 写在主编码器**之前**也能匹配成功（探针验证：`m=video ... 95 94`，`95 rtx apt=94` 在前 → 两者都进 negotiated 列表）。但「跨越媒体段」的 apt 不成立——第二遍只重扫本段的精确/部分列表。还有一处连带后果：**apt 指向另一个 RTX**（如 `99 rtx apt=97`）永远匹配不到，因为 `exactMatches`/`partialMatches` 里只会有主编码器；探针验证 `m=... 96 97 98 99`（97、98 均 `apt=96`，99 为 `apt=97`）的结果是 99 被静默丢弃，且 apt 目标重复时只保留两条 RTX。

### 5.2 发送侧：`findRTXPayloadType` 的字符串匹配是硬约束

`rtpcodec.go:197-206`：

```go
aptStr := fmt.Sprintf("apt=%d", needle)
for _, c := range haystack { if aptStr == c.SDPFmtpLine { return c.PayloadType } }
return PayloadType(0)
```

**整串相等**，不是参数解析。这些看似合法的写法全部返回 0：

| 远端/本地 fmtp | `findRTXPayloadType(96)` |
|---|---|
| `apt=96` | 97 ✔ |
| `apt=96;rtx-time=2000` | 0 ✘ |
| `rtx-time=2000;apt=96` | 0 ✘ |
| `APT=96` | 0 ✘ |

（探针验证全部四项。）

与之对照，另一套判定用的是解析式：`primaryPayloadTypeForRTXExists`/`filterUnattachedRTX`（`rtpcodec.go:210-251`）用 `fmtp.Parse(...).Parameter("apt")`（大小写不敏感、支持附加参数）并做 0..255 范围校验。于是同一份实现里存在两套不一致的 apt 语义。

**可观测的后果**（探针验证，端到端 SDP 输出）：

```
远端 a=rtpmap:97 rtx/90000 + a=fmtp:97 apt=96;rtx-time=2000
-> negotiated 含 [pt=97 video/rtx "apt=96;rtx-time=2000"]
-> isRTXEnabled(video) == true
-> answer 含 a=rtpmap:97 rtx/90000 与 a=ssrc-group:FID <base> <rtx>
-> 但 streamInfo.PayloadTypeRetransmission == 0
```

`isRTXEnabled`（`mediaengine.go:842-850`）只检查 MIME 里有没有 `video/rtx`，所以 SSRC 会分配、FID 组会写出；而 NACK 响应器生成 RTX 包要求 `rtxSsrc != 0 && rtxPayloadType != 0`（pion/interceptor `internal/rtpbuffer/packet_factory.go:81` 的分支），`rtxPayloadType==0` 时它只做普通缓冲、不会重写为 RTX。**结果：SDP 声称支持 RTX，实际不会发 RTX，且没有任何日志或错误。**

`filterUnattachedRTX` 也挡不住这类条目——它能识别 `apt=96`（解析式），所以 `apt=96;rtx-time=2000` 被判定「有主编码器」并保留。

### 5.3 接收侧：关联靠 SSRC 组，与 apt 无关

`trackDetailsFromSDP`（`sdp.go:79-190`）解析 `a=ssrc-group:FID <base> <rtx>`，把 `rtxSsrc` 记录到 `trackDetails`；`receiveForRtxInternal` 用该 SSRC 打开独立读流，`readRTX` 里重写报文头：PT 改为 `remoteTrack.PayloadType()`、SSRC 改为 `remoteTrack.SSRC()`，把 OSN 从载荷搬到头部（`rtpreceiver.go:700-755`）。

这意味着：

- 接收侧 RTX 能否工作，取决于**对端是否发了 FID 组**，以及本地能否解出该段的 base SSRC；`apt` 完全不参与。
- 若对端只声明 `apt` 不给 FID，本地不会为 RTX 建流（`trackDetails.rtxSsrc` 为空）。
- `Test_SSRC_Groups`（`sdp_test.go:1370`）已覆盖「本地是否启用 RTX」「远端是否给出 FID 组」的四种组合。

### 5.4 三个层次的能力边界

| 层次 | 由什么证明 | **能**证明 | **不能**证明 |
|---|---|---|---|
| 编解码匹配 | `fmtp.Match` / 三态 | 双方对该格式的**配置参数**可共存 | PT 是否相同；RTX 是否配对；报文实际用哪个 PT |
| 负载类型分配 | negotiated 列表、`getCodecs()`、answer 的 `a=rtpmap` | **声明**了哪些 PT/编码器 | 发送侧真正写入的 PT（§6.2）；接收侧能否反查（§6.3） |
| RTX 修复流关联 | 发送：`apt` 字符串；接收：`FID` 组 | 发送侧「用哪个 PT 发重传」；接收侧「哪个 SSRC 是修复流」 | 二者互不蕴含；`apt` 存在也不代表能发（§5.2） |

---

## 6. 协商结果如何收敛为「可发送 / 可接收」集合

### 6.1 发送集合

`RTPSender.Send`（`rtpsender.go:301-375`）对每个 encoding：

```go
rtpParameters := mediaEngine.getRTPParametersByKind(track.Kind(), sendonly)
codec, err := track.Bind(&baseTrackLocalContext{params: rtpParameters, ...})
...
createStreamInfo(..., codec.PayloadType,
    findRTXPayloadType(codec.PayloadType, rtpParameters.Codecs),
    findFECPayloadType(rtpParameters.Codecs), ...)
```

`TrackLocalStaticRTP.Bind`（`track_local_static.go:90-113`）用 `codecParametersFuzzySearch(轨道声明的 capability, 上下文里的编码器列表)`；命中后把 `codec.PayloadType` 与 `findRTXPayloadType(...)` 写进 binding，`writeRTP` 时**强制覆写**报文头的 PT 和 SSRC（`track_local_static.go:203-210`）。

要点：

- 发送 PT 来自 **kind 级**列表（远端 SDP 决定的 negotiated 列表），不是 transceiver 的 `codecs`。
- 若轨道的 capability 在列表里只有部分匹配，`Bind` 仍成功，并用**远端 fmtp** 建流（§3.3 已验证）。
- 若完全无匹配，`Bind` 返回 `ErrUnsupportedCodec`，`Send` 失败 → `SetLocalDescription` 返回错误（探针验证：本地轨道 H264、协商列表只有 VP8 → `unable to start track, codec is not supported by remote`，`sender.hasSent()==false`，无 binding）。注意此时 answer **已经生成并声明了 VP8**，错误发生在提交 answer 的瞬间。
- RTX SSRC 是否分配由 `addEncoding`（`rtpsender.go:200-212`）在**加轨道时**决定，重协商后再由 `configureRTXAndFEC`（`rtpsender.go:516-530`，被 `SetRemoteDescription` 在 `peerconnection.go:1283-1285` 调用）按 `isRTXEnabled(kind)` 清零。两者都是 **kind 级全局判断**，与「这条轨道的主编码器是否有 apt 配对的 RTX」无关。

### 6.2 PT 声明与线上 PT 可以不一致（已复现）

探针：answerer 用 `SetCodecPreferences([{VP8, PT:54}])`，offer 里 VP8 是 PT 96。结果：

```
answer SDP:   a=rtpmap:54 VP8/90000
transceiver.getCodecs(): [pt=54 video/VP8]
sender.GetParameters().Codecs: [pt=54 video/VP8]
mediaEngine.getRTPParametersByKind(sendonly): [pt=96 video/VP8]   <-- 实际建流用的
track binding.payloadType: 96
streamInfo.PayloadType: 96
接收侧反查 PT 54: codec not found
```

成因是 §1 的 ④/⑤ 分离：`getCodecs()` 在「用户设置了 preferences」分支用用户给的 PT（`rtptransceiver.go:82-104`），而 `Bind` 用 MediaEngine 的列表。用户自定义 PT 只在 **offerer** 场景下「碰巧」正确，因为 offer 发出后 answer 会回显同一 PT，而 offerer 的 negotiated 列表正是从 answer 解析来的；一旦在 **answerer** 侧自定义 PT，就会产生「声明了但不会用、也解析不了」的 PT。

同一机制也解释了 `rtptransceiver_test.go:91`（`Test_RTPTransceiver_SetCodecPreferences_PayloadType`）只断言 SDP 文本包含 `a=rtpmap:54`，没有断言线上 PT——测试覆盖的是声明层，不是行为层。

### 6.3 接收集合

接收侧按 PT 反查编码器，两处入口：

- `handleIncomingSSRC` → `getRTPParametersByPayloadType` → `getCodecByPayload`（video 优先，§2）。
- `TrackRemote.checkAndUpdateTrack`：每收到 PT 变化就重新反查；失败则**整条读取返回错误**，`OnTrack` 不会被触发（`peerconnection.go:1684-1700` 的 `startReceiver` 会 `Warnf` 后直接 `return`）。

所以「answer 里声明的 PT」必须与「MediaEngine 协商列表里的 PT」一致，接收才成立。由于 negotiated 列表就是「远端 SDP 的 PT」，**answerer 再自定义 PT 会直接破坏接收路径**（§6.2 的 `PT 54 not found`）。这也是 `findMediaSectionByPayloadType`（`peerconnection.go:2008-2035`）注释里「假定 PT 在媒体段内唯一」的现实约束。

### 6.4 一处已复现的列表就地改写缺陷

`getCodecs()`（`rtptransceiver.go:78-104`）在未设置 preferences 时执行：

```go
mediaEngineCodecs := t.api.mediaEngine.getCodecsByKind(t.kind)  // 返回 MediaEngine 的实时切片
if len(t.codecs) == 0 { return filterUnattachedRTX(mediaEngineCodecs) }
```

`getCodecsByKind` 直接返回 `m.videoCodecs`（未协商）或 `m.negotiatedVideoCodecs`（已协商），**不是副本**；`filterUnattachedRTX` 用 `codecs = append(codecs[:i], codecs[i+1:]...)` 原地删除，len 不变但元素前移、末尾重复。

探针（本地列表 `[rtx@51 apt=50(无主), vp8@54, vp9@56]`）：

```
getCodecs #1          = [vp8@54, vp9@56]
MediaEngine 之后      = [vp8@54, vp9@56, vp9@56]   // 末元素被复制
CreateOffer #2 的 video 段 = a=rtpmap:54 VP8 + a=rtpmap:56 VP9 + a=rtpmap:56 VP9
```

即：**「未挂接的 RTX 不在列表末尾」时，第二次及以后的协商会把重复的 `a=rtpmap` 泄漏进 SDP**。RTX 在末尾时不触发（无位移），因此仓库现有测试（`Test_RTPTransceiver_UnattachedRTX`，其未挂接 RTX 位于用户 preferences 里、且走的是新建切片的分支）未覆盖此路径。

---

## 7. 异常与冲突配置的错误可见性

### 7.1 可见（有返回值）

| 场景 | 错误 | 出处 |
|---|---|---|
| `m=` 行列出的 PT 在本段无 rtpmap | `errSDPPayloadTypeNotFound: payload type N` | `sdp.go:1148` |
| `apt` 非数字 / 越界 | `strconv.ParseUint: ...`（裸错误，未包装成 typed error） | `mediaengine.go:480-483` |
| 同 PT 不同 MIME/时钟/声道进入 negotiated 列表 | `ErrCodecAlreadyRegistered`（可被 `errors.Join` 聚合） | `mediaengine.go:248`, `588-603` |
| `SetCodecPreferences` 传了 MediaEngine 里没有的编码器 | `errRTPTransceiverCodecUnsupported` | `rtptransceiver.go:62-70` |
| `SetCodecPreferences` 传了 RTX 但它的 apt 主编码器不在 MediaEngine，或 fmtp 无法匹配 | `errRTPTransceiverCodecUnsupported: video/rtx`（**先于** `filterUnattachedRTX` 拦截） | `rtptransceiver.go:62-73`；探针验证 |
| 轨道编码器与协商结果无交集 | `ErrUnsupportedCodec`（`SetLocalDescription` 阶段） | `track_local_static.go:111`, `rtpsender.go:352-361` |
| 该 kind 一个编码器都没协商到，却要建 sender | `ErrNoCodecsAvailable` / `ErrSenderWithNoCodecs` | `peerconnection.go:2596-2600`, `sdp.go:583-586` |

### 7.2 静默（无返回值、无日志）

| 场景 | 实际行为 | 验证 |
|---|---|---|
| 时钟率/声道数不兼容（同 MIME） | 该段无匹配；kind 标志已置 `true`；后续表现为「无可用编码器」 | 探针 |
| 该段有精确匹配时，部分匹配被整体丢弃 | 部分匹配编码器消失 | 探针 |
| 同 PT 同 MIME 不同 fmtp（注册或协商） | 保留先到者 | 探针 |
| 音频 fmtp 取值冲突（如 `minptime`） | 走部分匹配，接受远端值 | 探针 |
| RTX fmtp 非规范写法（附加参数/大写键） | SDP 通告 RTX + FID，但 `PayloadTypeRetransmission=0` | 探针 + interceptor 源码 |
| RTX 无 apt | 以空 fmtp 进入 negotiated 列表，`findRTXPayloadType` 返回 0 | 探针 |
| 同一 apt 匹配多个 RTX | `findRTXPayloadType` 只返回列表里**第一个**（探针：`apt=96` 有两个 RTX 97/98，只有 97 被认） | 探针 |
| 跨 kind 复用 PT | 接收侧永远解析为 video | 探针 |
| `SetCodecPreferences` 去掉了 RTX，但 MediaEngine 仍启用 RTX | answer 不写 `a=rtpmap:97`，却仍写 `a=ssrc-group:FID base rtx`；`streamInfo.rtxPT=97` | 探针（§6.1 + §5.2 叠加） |

### 7.3 错误发生时状态已提交，且无法用 rollback 恢复

`PeerConnection.SetRemoteDescription` 的顺序是（`peerconnection.go:1241-1265`）：

```go
if err := pc.setDescription(&desc, stateChangeOpSetRemote); err != nil { return err }  // 先改信令状态为 have-remote-offer
if err := pc.api.mediaEngine.updateFromRemoteDescription(*desc.parsed); err != nil { return err }  // 再解析编码器
```

因此编码器层面的错误（apt 非法、PT 冲突）会让 PeerConnection **停在 `have-remote-offer`，pending remote description 已保存，MediaEngine 已部分更新**（前面已处理的媒体段的效果保留）。探针验证：

```
SRD err=payload type not found ... 42   signaling=have-remote-offer  pendingRemote=true  meVideo=true
SRD(重试修正后的 offer)     err=invalid proposed signaling state transition: have-remote-offer->SetRemote(offer)
SetLocal(rollback, SDP="")  err=invalid SDP type supplied to SetLocalDescription(): rollback
SetLocal(rollback, SDP!=0)  err=invalid proposed signaling state transition: have-remote-offer->SetLocal(rollback)->stable
SetRemote(rollback, SDP!=0) err=invalid proposed signaling state transition: have-remote-offer->SetRemote(rollback)->stable
```

`checkNextSignalingState`（`signalingstate.go:120-190`）对 `SDPTypeRollback` 只做了一处处理：`stable` 状态直接拒绝（`errSignalingStateCannotRollback`）；其余分支按 op 只接受 `offer/answer/pranswer`，**没有接受 rollback 的分支**。因此本实现中 rollback **在任何状态下都不生效**（探测：`stable`、`have-local-offer`、`have-remote-offer` 全部返回 `InvalidModificationError`）。`SetLocalDescription` 虽在 `peerconnection.go:1068` 有 `SDPTypeRollback` 分支，但当 `desc.SDP == ""` 时会先在 JSEP 5.4 的空 SDP 守卫处（`peerconnection.go:1149-1157`）返回「invalid SDP type」，带 SDP 时则死在状态机里。

也就是说：编码器校验失败后，调用方既不能重发修正后的 offer，也不能 rollback，**只能把这次协商当作「已接受」继续走完 answer/pranswer**，再重新协商。对「非法 apt / PT 冲突」这类可修复的输入错误，恢复成本明显偏高。

对照：协议一致性校验（如 Cryptex）被特意放在 `setDescription` **之前**（`peerconnection.go:1248-1260`，注释引用 RFC 9429 §5.10 要求「拒绝的描述应保持调用前状态」）。编码器校验没有享受同等待遇。

### 7.4 一个给出假安心的测试

`mediaengine_test.go:276`「Does not match when fmtpline is set and does not match」：

```go
m=video 60323 UDP/TLS/RTP/SAVPF 96 98     // 列了 98
a=rtpmap:96 H264/90000                     // 但只定义了 96
a=fmtp:96 ...profile-level-id=640c1f
...
assert.Error(t, mediaEngine.updateFromRemoteDescription(...))
```

错误实际来自 `PT 98 无 rtpmap`（`errSDPPayloadTypeNotFound`），**不是** fmtp 不匹配。探针用同一份 SDP 复现得到 `payload type not found in media section: payload type 98`；把 `m=` 行改成只列 `96` 后，`updateFromRemoteDescription` 返回 **nil**，H264 通过部分匹配被接受，negotiated fmtp 为远端的 `640c1f`。也就是说这条测试名义上保护的「fmtp 不兼容应失败」语义，在当前实现里并不存在。

---

## 8. 旧优先级行为与兼容边界

### 8.1 `DisableMediaEngineMultipleCodecs`

- 默认**启用**多编码器协商：`pc.api.mediaEngine.setMultiCodecNegotiation(!se.disableMediaEngineMultipleCodecs)`（`peerconnection.go:170`）。
- 关闭后（`multiCodecNegotiation()==false`），`updateFromRemoteDescription` 在 kind 已协商时**跳过编码器解析**（但仍先更新头扩展，`mediaengine.go:626-636`）。效果：只有**第一个** audio 段和**第一个** video 段参与协商。
- `settingengine.go:530-538` 注释明确：这是为兼容旧行为保留的开关，计划在 4.2.0 之后移除；`settingengine_test.go:617` 覆盖了 flag 的存储。

兼容边界（探针验证，两个 video 段：第一段 VP9@96+RTX97，第二段 VP8@100+RTX101）：

```
multi=false -> negotiated = [VP9@96, rtx@97]
transceiver mid=0 codecs = [VP9@96, rtx@97]
transceiver mid=1 codecs = [VP9@96, rtx@97]      <-- 第二段也回写第一段的 PT
answer: 两个 m=video 段都写 a=rtpmap:96 VP9 / :97 rtx
```

在 multi=true 时同一输入得到 `mid=1 = [VP8@100, rtx@101]`，answer 正确回显 100/101。因此**旧行为在「多段同 kind 且各段编码器不同」的场景下会生成协议上错误的 answer**（第二段声明了 offer 未在该段提供的 PT）。这是开关的真实语义边界，`TestMultiCodecNegotiation`（`mediaengine_test.go:774`）只断言 negotiated 列表长度为 1 vs 2，没有断言 answer 内容，因此该偏差未被覆盖。

### 8.2 旧优先级行为的另一个可观测点：预建 transceiver 的段级泄漏

即使 multi=true，**预建的 transceiver**（用户在 `SetRemoteDescription` 之前 `AddTransceiverFromKind`）在 `getCodecs()` 里拿到的是 kind 级 negotiated 列表的**并集**，而不是「本段」的编码器。探针（offer 两段：96/97 VP9、100/101 VP8；answerer 预建 1 个 recvonly video transceiver）：

```
answer mid=0: a=rtpmap:96 VP9 + :97 rtx + :100 VP8 + :101 rtx
```

即第一段错误地通告了 offer 在该段没给的 100/101（`TestProbePreCreatedTransceiverSectionLeak` 的 preAdd=true 输出）。而由远端 SDP 自动创建 transceiver 的路径（`setCodecPreferencesFromRemoteDescription`，`peerconnection.go:1338-1342`，`rtptransceiver.go:106-186`）会按段把 PT 映射好，因此不会泄漏。二者的行为差异是隐式的，取决于「用户是否预建 transceiver」。

### 8.3 `setCodecPreferencesFromRemoteDescription` 的等价负载保护

`matchCodecWithPayloadIdentity`（`rtptransceiver.go:188-215`）处理「同段内多个等价的 H265 层级」：若精确命中的本地 PT 恰好也被该段提供，但远端在该段用的是另一个 PT，则优先保留与远端 PT 同号的本地条目，避免把两个等价格式映射到同一个 PT。`TestRTPTransceiverH265AnswerPreservesPayloadTypes` 与 `TestRTPTransceiverEquivalentCodecSelection`（`rtptransceiver_test.go:286,342`）已覆盖五种情形，包括「必须重映射」和「沿用前一段的规范映射」的取舍。

---

## 9. 「不能只按名字相同判定互通」的完整判据链

要判定「这条轨道真的能发给对端」，需依次成立（任一步失败都不互通）：

1. **种类一致**：m= 行的 media 类型决定 kind（不是 rtpmap 名字），且 PT 在 kind 内未被另一种 kind 占用（§2）。
2. **格式参数兼容**：按 MIME 规则精确匹配，或（无任何精确匹配时）至少 MIME+时钟率+声道数一致（§3）。
3. **PT 真的被声明在该段**：multi-codec 关闭或预建 transceiver 时，SDP 里的 PT 集合可能是 kind 并集而非段内交集（§8.1、§8.2）。
4. **发送侧能反查到同一 PT**：`Bind` 用的是 kind 级列表，用户自定义 PT 会与之错位（§6.2）。
5. **接收侧能按该 PT 解析**：`getCodecByPayload` 只看 kind 级列表且 video 优先（§2、§6.3）。
6. **RTX 若要可用**：发送侧 fmtp 必须是规范 `apt=N` 单独成串（§5.2）；接收侧必须有 `a=ssrc-group:FID`（§5.3）；并且 `isRTXEnabled` 这类 kind 级开关与 transceiver 的声明需要同时成立（否则会出现 §7.2 最后一行那种「answer 不写 rtpmap 却写 FID」的不一致）。

---

## 10. 判据清单：已证实 / 已有测试 / 仍需补充

标记说明：**S** = 源码已证实（读码即可确定）；**P** = 本次探针额外复现；**T** = 仓库已有测试覆盖；**N** = 无覆盖或覆盖无效，建议补充。

### 10.1 匹配判定

| # | 判据 | S | P | T | N | 证据 / 建议 |
|---|---|---|---|---|---|---|
| 1 | 精确匹配按 MIME 规则，非字符串相等 | ✔ | ✔ | ✔ | | `internal/fmtp/*`；`fmtp_test.go:153` |
| 2 | H264 忽略 level 字节、要求 pk-mode 存在且相等 | ✔ | | ✔ | | `h264.go`，`fmtp_test.go`「h264 different profile level ids version」 |
| 3 | H265 忽略 level-id，默认值 1/0/SRST | ✔ | ✔ | ✔ | | `h265.go`；`peerconnection_go_test.go:1753` |
| 4 | 部分匹配回退到「MIME+clock+channels」 | ✔ | ✔ | ✔ | | `rtpcodec.go:131-142`；`mediaengine_test.go:390` |
| 5 | 存在精确匹配时整段部分匹配被丢弃 | ✔ | ✔ | ✔ | | `mediaengine.go:694-703`；`mediaengine_test.go:250` |
| 6 | 部分匹配时 answer 回显远端（不兼容）fmtp，发送侧也按该 fmtp 建流 | ✔ | ✔ | | **N** | 建议：H264 pk-mode=0/1 不匹配的 offer，断言 answer `a=fmtp` 与 `streamInfo.SDPFmtpLine`，并说明是否期望拒绝 |
| 7 | 音频 fmtp 取值冲突被部分匹配静默接受 | ✔ | ✔ | | **N** | 建议：opus `minptime` 冲突应断言接受或拒绝其中之一 |
| 8 | generic FMTP 为「共有键相等」的子集语义 | ✔ | ✔ | ✔ | | `fmtp.go:91-105`；`fmtp_test.go`「generic one extra param」 |
| 9 | `m=video` 里写 opus/xxx 会被当作 `video/opus` 且静默不协商 | ✔ | ✔ | | **N** | 建议：断言 negotiated 为空但不报错（或明确要求报错） |
| 10 | 名字相同但时钟率不同 → 不匹配、不报错 | ✔ | ✔ | | **N** | 建议：opus 48000 vs 16000，断言 `SetRemoteDescription` 成功且随后 `ErrNoCodecsAvailable` |
| 11 | 名字相同但声道数不同 → 不匹配（不落入部分匹配） | ✔ | ✔ | | **N** | 同上，devices/1 vs /2 |

### 10.2 PT 与重复声明

| # | 判据 | S | P | T | N | 证据 / 建议 |
|---|---|---|---|---|---|---|
| 12 | 同 PT 不同 MIME/时钟/声道 → `ErrCodecAlreadyRegistered` | ✔ | ✔ | ✔ | | `mediaengine.go:239-253`；`mediaengine_test.go:516` |
| 13 | 同 PT 同 MIME 不同 fmtp → 静默保留首条（注册与协商两个入口） | ✔ | ✔ | | **N** | 建议：注册两个不同 pk-mode 的同 PT H264，断言无错误且列表长度 1 |
| 14 | 跨段同 PT 冲突（VP8 vs VP9）→ `SetRemoteDescription` 报错 | ✔ | ✔ | | **N** | 建议：两段同 PT 不同编码器 |
| 15 | 跨 kind 复用同一 PT 时，接收侧固定解析为 video | ✔ | ✔ | | **N** | 建议：audio/video 都用 PT 96，断言 `getCodecByPayload(96)` 与 `TrackRemote.Codec()` |
| 16 | 重协商变更同 PT 的编码器 → 报错 | ✔ | ✔ | | **N** | 建议：两次 `SetRemoteDescription` 用同 PT 不同编码器 |
| 17 | 静态 PT（0/8/9）无需 rtpmap 即可解析；非 0 未声明 PT 报错 | ✔ | ✔ | | 部分 | `sdp.GetCodecMap` 静态表；`sdp_test.go:1332` 仅覆盖普通 rtpmap 情形 |
| 18 | 测试「fmtp 不匹配应报错」实际因 PT 98 缺少 rtpmap 而通过 | ✔ | ✔ | 误导 | **N** | `mediaengine_test.go:276`；建议改为单 PT 的 m= 行并断言真实期望 |

### 10.3 `apt` 与 RTX

| # | 判据 | S | P | T | N | 证据 / 建议 |
|---|---|---|---|---|---|---|
| 19 | `findRTXPayloadType` 要求整串 `apt=N` | ✔ | ✔ | | **N** | 建议：`apt=96;rtx-time=2000`、`APT=96` 各一条 |
| 20 | `filterUnattachedRTX` 用解析式（支持附加参数、大小写不敏感） | ✔ | ✔ | ✔ | | `rtpcodec.go:210-251`；`rtpcodec_test.go:12`（含「substring」「incomplete fmtp」「越界」） |
| 21 | 非规范 apt → 通告 RTX + FID 但 `PayloadTypeRetransmission=0` | ✔ | ✔ | | **N** | 建议：断言 answer 含 `rtx` 的同时断言 `streamInfo.PayloadTypeRetransmission` |
| 22 | 无 apt 的 RTX 会以空 fmtp 进入 negotiated 列表 | ✔ | ✔ | | **N** | 建议：只写 `a=rtpmap:97 rtx/90000` |
| 23 | 同一 apt 的多个 RTX，`findRTXPayloadType` 只返回列表首个 | ✔ | ✔ | | **N** | 建议：两个 `apt=96` |
| 23b | apt 指向另一个 RTX（链式）时该 RTX 静默丢弃（apt 只能指向主编码器） | ✔ | ✔ | | **N** | 建议：`96 97 98 99`，97/98 `apt=96`、99 `apt=97` |
| 24 | RTX 的 apt 目标必须在本段匹配成功（跨段无效） | ✔ | ✔ | | **N** | 建议：RTX 段与主编码器段分开 |
| 25 | RTX 写在主编码器之前仍可匹配（两遍扫描） | ✔ | ✔ | | **N** | 建议：`m=... 95 94` |
| 26 | 协商让 RTX 降级为部分匹配（主编码器部分匹配时） | ✔ | ✔ | ✔ | | `mediaengine.go:522-527`；`mediaengine_test.go:390` |
| 27 | `isRTXEnabled` 只看 MIME，与 transceiver 的 codec 集合无关 | ✔ | ✔ | 部分 | **N** | `mediaengine_test.go:507` 只测「注册了 RTX」；未测「prefs 去掉 RTX 后 answer 仍写 FID」 |
| 28 | answer 省略 RTX rtpmap 但仍输出 `a=ssrc-group:FID` | ✔ | ✔ | | **N** | 建议：`SetCodecPreferences` 只留主编码器 |
| 29 | 接收侧 RTX 关联只靠 `a=ssrc-group:FID`，不看 apt | ✔ | | ✔ | | `sdp.go:79-190`；`Test_SSRC_Groups`（`sdp_test.go:1370`） |

### 10.4 发送/接收集合与 SDP 声明

| # | 判据 | S | P | T | N | 证据 / 建议 |
|---|---|---|---|---|---|---|
| 30 | 发送 PT 来自 `getRTPParametersByKind`，不是 transceiver `getCodecs` | ✔ | ✔ | | **N** | 建议：answerer 侧 `SetCodecPreferences(PT 54)`，断言 answer 为 54 而 track binding 为 96（当前行为） |
| 31 | answerer 侧自定义 PT 后，接收侧无法反查该 PT | ✔ | ✔ | | **N** | 建议：断言 `getRTPParametersByPayloadType(54)` 报 `ErrCodecNotFound` |
| 32 | 轨道编码器不在协商结果内 → `SetLocalDescription` 返回 `ErrUnsupportedCodec`，且无 binding | ✔ | ✔ | 部分 | | `rtpsender.go:352-361`；`peerconnection_media_test.go:1553` 只测「完全无编码器」的 `ErrSenderWithNoCodecs` |
| 33 | 无该 kind 编码器时 `AddTransceiverFromKind` → `ErrNoCodecsAvailable`；已有 sender 时 answer 为 `m=... 0` | ✔ | ✔ | | **N** | 建议：时钟率不兼容的 offer |
| 34 | 已协商但列表为空会导致 `getCodecsByKind` 返回空（不是回落到本地列表） | ✔ | ✔ | | **N** | `mediaengine.go:716-735` |
| 35 | `getCodecs()` 就地改写 MediaEngine 切片 → SDP 出现重复 `a=rtpmap` | ✔ | ✔ | | **N** | **建议优先修复+补测**：未挂接 RTX 在中段，连续两次 `CreateOffer` |
| 36 | 预建 transceiver 会把 kind 并集写进单个媒体段 | ✔ | ✔ | | **N** | 建议：两段不同编码器 + 预建 recvonly transceiver |
| 37 | 旧模式（`multi=false`）后续段复用第一段的 PT/编码器 | ✔ | ✔ | 部分 | **N** | `TestMultiCodecNegotiation` 仅断言列表长度；建议断言 answer 文本 |
| 38 | 旧模式仍会先更新头扩展再 `continue` | ✔ | | | | `mediaengine.go:626-636` |

### 10.5 错误可见性与状态一致性

| # | 判据 | S | P | T | N | 证据 / 建议 |
|---|---|---|---|---|---|---|
| 39 | 编码器错误发生在信令状态提交之后，PC 停在 `have-remote-offer` | ✔ | ✔ | | **N** | `peerconnection.go:1261-1265`；建议：断言 `SignalingState()` 与 `PendingRemoteDescription()` |
| 40 | 本实现的 rollback 在任何信令状态都不生效：`stable` 报 `errSignalingStateCannotRollback`，`have-local-offer`/`have-remote-offer` 报状态转换非法；空 SDP 时先被 JSEP 5.4 守卫拦下 | ✔ | ✔ | | **N** | `signalingstate.go:120-190`，`peerconnection.go:1068,1149-1157`；建议补一条「编码器错误后无法恢复」的显式用例 |
| 41 | 前一媒体段的效果在报错后保留（MediaEngine 部分更新） | ✔ | ✔ | | **N** | 建议：三段 offer，第三段放非法 PT，断言前两段已进 negotiated |
| 42 | `apt` 解析错误是裸 `strconv` 错误，未包装为 typed error | ✔ | ✔ | | **N** | 建议：断言 `errors.Is` 行为与错误类型 |
| 43 | `SetCodecPreferences` 对「无主 RTX」返回 `errRTPTransceiverCodecUnsupported`（先于 `filterUnattachedRTX`） | ✔ | ✔ | | **N** | 建议：prefs 只含 `rtx apt=50`，MediaEngine 无 PT 50 |
| 44 | `SetCodecPreferences` 的校验与 `getCodecs` 的过滤使用不同判定 | ✔ | | ✔ | | `rtptransceiver_test.go:152`（用户 preferences 分支） |

---

## 附录 A：验证方式（可复现的最小构造）

所有探针都是「`package webrtc` 内部测试 + 手写 SDP 文本 + 纯本地 API」，不建真实对等端：

1. **MediaEngine 级**：`me.RegisterDefaultCodecs()` 或按需 `RegisterCodec`，然后 `me.updateFromRemoteDescription(parsedSDP)`，断言 `negotiatedVideoCodecs`/`negotiatedAudioCodecs` 或 `getCodecByPayload`。
   - 这是最省事的判据入口，且能直接暴露「静默降级」。
2. **PeerConnection 级**：`NewAPI(WithMediaEngine(&me))` → `NewPeerConnection` → `SetRemoteDescription(offer)` → `CreateAnswer` / `SetLocalDescription`，断言 SDP 文本（`a=rtpmap`/`a=fmtp`/`a=ssrc-group`/`m=... 0`）与信令状态。
   - SDP 必须包含 `a=ice-ufrag`/`a=ice-pwd`/`a=setup`/`a=fingerprint`，否则会在 ICE 校验处提前失败。
3. **发送侧**：`SetLocalDescription` 之后读 `tr.Sender().trackEncodings[0].streamInfo.PayloadType/PayloadTypeRetransmission/SSRC…`，以及 `TrackLocalStaticRTP` 的 `bindings`（内部字段），即可区分「SDP 声明」与「线上 PT」。
4. **接收侧**：`getRTPParametersByPayloadType(pt)` 或直接调用 `TrackRemote.checkAndUpdateTrack(pkt)`。
5. 曾尝试用 `newPair()` + `signalPair()` 做 loopback 校验「answer 声明的 PT 是否真的上线」，但**对照组（不改任何 PT）同样收不到 `OnTrack`**（裸写 RTP 包缺少正确的 SRTP/序号环境），因此该实验结论不可靠，**未纳入上面的结论**；§10 #30/#31 的判据仅由 1、3、4 三类入口证实。

## 附录 B：本次分析不涉及

- 未启动真实对等端、未连接任何外部服务、未跑 E2E/`vnet` 用例。
- 未评估 `pion/sdp` 与 `pion/interceptor` 的版本兼容性（只在解释 RTX 发包条件时引用了 `interceptor@v0.1.51` 的分支逻辑）。
- 未涉及 simulcast/Plan-B 的编码器选择细节，仅在 §8.2 提到它与 transceiver 创建路径的交互。
