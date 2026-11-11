# 方案 D：出进程 + 缓冲区 fd 直传（D⁺）

> 状态：**被平台闸门卡住（2026-11-11 实测）**。第三方应用要运行 HAP 里的任何可执行文件，
> 该文件必须带**二进制证书**的代码签名，而二进制证书需要企业账号。两条路都撞在同一处：
>
>     libs/ 里放 ELF     → CheckCertHasBinaryCertExtension: Binary cert extension not found
>                          ParseNativeLibSignInfo: Libs signature not found: signMap_ size:4, signMapPreSize:1
>     HNP 里放 ELF       → hnp/CODE_SIGN: Libs signature not found: signMap_ size:0, signMapPreSize:0
>                          ProcessBundleInstallNative: Native package installation failed with error code: 8393475
>
> 换谁签 HAP（hvigor / hda / hap-sign-tool -signCode 1）都不改变这两条。
> **所以本文描述的是"如果拿到二进制证书"之后的实现准绳；没有证书时它不可交付。**
> 见 ../devlog/2026-11-11.md 的第三、四轮。
>
> **当天深夜更正：已经跑通了。** 上面那堵墙只在两种情况出现——把 ELF 放进 `libs/`，
> 或自己用 hap-sign-tool 签一个带 HNP 的 HAP。**换成 `hda install` 就行**：它会把 HNP
> 装到沙箱里的 `/data/app/`，实测应用进程能 `execve` 它（exit=0）。见第五轮。

## 1. 职责边界

| | 谁 | 内容 |
|---|---|---|
| **控制面** | 宿主 `.so`（UI 进程，C，约 200 行） | NAPI 注册、`fork+execve` 拉起引擎、`RequestBuffer`/`FlushBuffer`、输入转发、IME 桥、前后台与窗口生命周期、看门狗 |
| **数据面** | Go 引擎（独立进程，`CGO_ENABLED=0`） | `mmap` 缓冲区、`internal/raster` 光栅、damage 计算、`FRAME_READY` |

**宿主里永远不出现像素。** 它一旦开始画，架构就退化成两个渲染器。

## 2. 为什么是 fd，不是 parcel

| | 传什么 | 需要 IPC Kit | 跨进程引用计数 |
|---|---|---|---|
| E（`OH_NativeBuffer_WriteToParcel`） | `OHNativeWindow` 句柄 | 是 | 两端都要 Reference/Unreference |
| **D⁺** | 缓冲区 **fd**（SCM_RIGHTS） | 否 | 无 |

官方《图形缓冲区常见稳定性问题》：超过 90% 的问题来自引用计数增减不匹配。
fd 不参与引用计数，谁 mmap 谁 munmap，close 一次就完。

## 3. 协议

本机验证版见 `experiments/ohos-frame`。线上版在框架的 `internal/platform/ohos/host`。

```
帧 = u8 type | u32 len | payload
  1 HELLO        { u32 proto_version }
  2 SURFACE      { u32 w, h; u32 stride; u32 size; u32 format }   ← 附一个 fd
  3 FRAME_READY  { u32 seq; u32 n; n × { i32 x, y; u32 w, h } }   ← 脏矩形，左下原点
  4 INPUT        { u8 kind; u32 len; bytes }                      ← touch/mouse/key/ime
  5 RESIZE       { u32 w, h; u32 stride; u32 size }               ← 尺寸变了，重新 SURFACE
  6 QUIT
```

约束：
- **Region 以 buffer 左下角为原点**（`FlushBuffer` 的契约），`ui` 的 `image.Rectangle`
  是左上原点，转换在宿主做：`y' = height - (y + h)`。
- `n == 0` 表示整屏脏。
- 一帧一个 fd。SCM_RIGHTS 约 1–5µs，不值得为省它做 fd 缓存。真要省，
  让宿主在 `SURFACE` 里把 N 个 buffer 的 fd 一次发完，之后只发序号。

## 4. 帧时序

```
宿主                              引擎
 │ RequestBuffer(win,&buf,&fence)
 │ (fence ≥ 0 就等它，然后 close)     │
 │ send SURFACE{fd,stride,size} ────▶│
 │                                   │ mmap(fd)
 │                                   │ raster.Render(scene) → damage
 │                                   │ 写入映射内存
 │ ◀──────────── FRAME_READY{seq,rects}
 │ FlushBuffer(win,buf,-1,region)    │
 │ (看门狗：100ms 没 READY → AbortBuffer + 重建)
```

**为什么用 `RequestBuffer`+`FlushBuffer` 而不是 `LockBuffer`+`UnlockAndFlushBuffer`：**
后者 `UnlockAndFlushBuffer(window)` 只接 window、不接 region——脏区只能在**锁的时候**
给定，而 damage 要渲染完才知道；前者 `FlushBuffer(win, buf, fence, region)` 在提交时
给 region，且能带 fence（将来换离屏 GL 时需要）。

## 5. 失败模式与恢复

| 症状 | 原因 | 处理 |
|---|---|---|
| 画面卡死 | 引擎崩在渲染中，buffer 没还 | 看门狗超时 → `AbortBuffer` → 重新 `RequestBuffer` |
| 花屏/撕裂 | 引擎在宿主读的时候还在写 | 宿主 `FlushBuffer` 之后才发下一帧；引擎等 ack |
| 全黑 | usage 没开 CPU 写，mmap 写不进去 | `NativeWindowHandleOpt(SET_USAGE, CPU_WRITE|CPU_READ_OFTEN|…)` |
| 尺寸错 | resize 时没重设 geometry | `SET_BUFFER_GEOMETRY` + 重新 SURFACE，33ms 去抖 |
| 引擎崩溃循环 | 启动参数/环境不对 | 宿主退避重启（100ms/500ms/2s），三次后上报 UI |

## 6. 验收（Phase 0 的退出条件）

1. **本机**（不需要设备）：`experiments/ohos-frame` 跑通 fd 传递 + mmap + READY + damage 校验；
2. **设备**：宿主 100 行 + 引擎 80 行，XComponent 上出现纯色；
3. **设备**：跳动的方块，肉眼 60fps，脏矩形生效（`hilog` 打印每次的 rect 数）；
4. **设备**：杀掉引擎，宿主 3 秒内重启它，画面恢复。

## 7. 风险与未验证项

| 项 | 影响 | 何时知道 |
|---|---|---|
| 应用进程里自己 `fork+execve` 被拦 | 架构不成立 | Phase 0 第一天 |
| 引擎 ELF 的**代码签名** | 装不上（`verify code signature failed`） | 已验：self-sign 不够，要应用证书 |
| 图形 buffer 的 fd 能否跨进程 mmap | 决定 D⁺ 还是退回 D | Phase 0 第二天 |
| 输入链路多两跳的跟手性 | 体验（滑块、滚动） | 有画面之后立刻测 |
| 文本引擎（`internal/text/engine_ohos.go`） | 排期 | 第一阶段之后 |

最后一条不是"风险"而是工作量：OHOS 的文字栈要么走 `OH_Drawing`（`libnative_drawing.so`，
设备上确认存在），要么 purego 加载 `libfreetype.so`。两者都要产出同样的东西——
字形位图 + 度量，而不是"帮我画一段字"。


## 8. 引擎放在哪：三条路，实测状态（2026-11-11）

| 位置 | 安装期检查 | 应用能否 exec | 结论 |
|---|---|---|---|
| libs/{abi}/ + executableBinaryPaths | 逐个文件校验代码签名，要**二进制证书** | — | 卡在安装，个人开发者不可用 |
| HNP（hnpPackages + hda install） | 由 hda 处理 | 能，exit=0 | 可行，但依赖 hda 的安装流程 |
| rawfile → 应用 filesDir → chmod 0755 → exec | 无 | 需 `CUSTOM_SANDBOX` | 放弃 |

第三条曾经是首选：ELF 放 resources/rawfile，启动时复制到
`/data/storage/el2/base/haps/entry/files/`，chmod，然后 fork+execve。
它确实绕开了二进制证书和安装期检查——**但从 filesDir 里 exec 需要
`ohos.permission.CUSTOM_SANDBOX`（自定义沙盒），这个权限要人工审批，
个人开发者基本拿不到。**

**所以当前唯一可交付的是 HNP + `hda install`。**
代价：依赖 hda 的安装流程；HNP 按版本号缓存，换二进制要改目录名（`mygo_1.0.1`）
重新装，否则跑的是旧的那份。

三条路各自的闸门：`libs/` 要二进制证书（企业账号），rawfile 要 `CUSTOM_SANDBOX`
（人工审批），HNP 是剩下唯一不需要额外审批的。

## 9. 框架侧的落点（对照 `mygo v0.8.4-0.20261107` 核对）

| 事实 | 位置 | 对 D⁺ 的意义 |
|---|---|---|
| `WindowOptions.Surface` 已存在 | `internal/platform/platform.go:477` | 不用发明"自绘窗口"概念 |
| `Surface.PresentPixels` / `DamageSurface.PresentDamage` | `internal/platform/surface.go:29,64` | 帧的出口就是这个接口 |
| `ui` 的软件路径自己算 damage 并调 `PresentDamage` | `ui/window.go:494-500` | **`ui` 的帧路径一行不用改** |
| `ui` 已有 headless 宿主 | `ui/headless.go:19,101` | ohos 宿主照它的形状写 |
| `SurfaceEvent` 覆盖指针/键/IME/滚动/捏合 | `internal/platform/surface.go:378` | 输入协议有现成目标类型 |
| `linux && arm64` 编译进 `pango_linux.go` | `internal/text/pango_linux.go:1` | **设备上没有 Pango** |
| 其余平台落到 `stubEngine` | `internal/text/engine_other.go` | 兜底是"没有字" |

关键的一条：`ui/window.go` 的软件路径已经是

```go
h.path = "drawn in memory"
damage := h.soft.Render(s)
m := &h.soft.Image
if d, ok := h.conn.Surface.(platform.DamageSurface); ok {
    d.PresentDamage(m.Pix, m.Stride, m.W, m.H, damage)
}
```

也就是说 **damage 已经算好了，只是没人把它交给 `FlushBuffer`**。D⁺ 要做的是让
`PresentDamage` 写进共享 buffer 并把 rect 编成协议，而不是自己去算脏区。
这比原计划里"做 damage"小得多。

`ui` 自己只通过 `h.conn`（`Surface`/`Post`/`Ready`/`RightToLeft`）接触后端，
没有直接引用 `platform.Backend`；后端的选择点在顶层包里。所以新增一个
`internal/platform/ohos` 不需要改 `ui` 内部——需要改的是选后端那一处。

文本是唯一的例外：`newEngine()` 按构建标签选实现，`linux && arm64` 会选中
Pango 那一份。目标三元组不变（要跑在 OHOS 的 linux 内核上），所以要靠额外的
标签（`mygo_ohos`）把它排除掉，再给 ohos 自己的实现。**这一条必须动框架仓库。**

