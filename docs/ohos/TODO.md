# 路线图：方案 D⁺

排序原则：**按风险排，不按分层排。** 每个阶段回答一个今天答不出的问题，
且答案有可能改掉后面的计划；不会改计划的阶段不该排在前面。
细节见 [design/d-out-of-process.md](design/d-out-of-process.md)。

## S0 已完成：设备上有真实像素在动

ArkTS `XComponent(SURFACE)` → `libmygo_host.so` → `RequestBuffer` →
fd 走 `SCM_RIGHTS` → Go 引擎 `mmap` 画 → `FlushBuffer`。
证据见 [devlog/2026-11-11.md](devlog/2026-11-11.md) 第五～八轮。
引擎构建：`-buildmode=exe`、`CGO_ENABLED=0`、`GOOS=linux GOARCH=arm64`。

已核对的框架落点（`mygo v0.8.4-0.20261107`）：

| 事实 | 位置 | 对 D⁺ 的意义 |
|---|---|---|
| `WindowOptions.Surface` 已存在 | `internal/platform/platform.go:477` | 不用发明"自绘窗口"概念 |
| `Surface.PresentPixels`、`DamageSurface.PresentDamage` | `internal/platform/surface.go:29,64` | 帧的出口就是这个接口 |
| `ui` 软件路径自己算 damage 并调 `PresentDamage` | `ui/window.go:494-500` | **`ui` 的帧路径一行不用改** |
| 已有 headless 宿主（`raster.Renderer` → `image.RGBA`） | `ui/headless.go:19,101` | ohos 宿主照它的形状写 |
| `SurfaceEvent` 覆盖指针/键/IME/滚动/捏合 | `internal/platform/surface.go:378` | 输入协议有现成目标类型 |
| `linux && arm64` 会编译进 `pango_linux.go` | `internal/text/pango_linux.go:1` | **设备上没有 Pango**，见 S3 |
| 其余平台落到 `stubEngine` | `internal/text/engine_other.go` | 兜底是"没有字"，不是"字难看" |

## S1 闭环：damage / resize / 节拍 —— 已完成

要把每帧真正花多少时间变成可测的。

- [x] 协议换成 `u8 kind | u32 len | payload`：`FRAME`（带 fd）/ `READY`（带 rect）/ `QUIT`
- [x] `FlushBuffer` 的 `Region` 传真脏矩形（左下原点，`y' = h-(y+hh)`）
- [x] **damage 按 buffer 记**：环里每块 buffer 存的是 N 帧前的内容，
      第一次用到必须整帧画。实测 `whole=4`（环深 4），之后每帧 `rects=2`
- [x] `onSurfaceChanged` 重设 `SET_BUFFER_GEOMETRY`，在循环顶部、不持 buffer 时改
- [x] 节拍换成 `OH_NativeVSync`，120Hz 上 p50 落到 8.3–8.6ms，`vsyncMisses=0`
- [x] 看门狗（`poll` 100ms → `AbortBuffer`）、重启退避 100/500/2000ms、三次放弃
- [x] `PR_SET_PDEATHSIG(SIGKILL)`，应用死了引擎不留孤儿
- [x] 引擎 stdout/stderr 接进 hilog（一条 pipe + 读线程）——引擎的报错全靠它

退出判据全部达成，数字见 [devlog 第九轮](docs/devlog/2026-11-11.md)。

剩下的：33ms 去抖（现在窗口拉开时会连改十几次几何，能跑但多余）。

## S1.5 探针：purego 与 OH_Drawing —— 已完成

引擎加了 `--probe`，宿主 attach 时跑一次，输出走同一条 pipe：

- [x] **purego 在 OHOS 的 musl 上能用**：`dlopen` + `RegisterLibFunc` 调 libc 的
      `getpid`，与 `os.Getpid()` 一致
- [x] **`/system/fonts` 可读，263 个字体**：S3 不用自带字体
- [x] `libnative_drawing.so` 能 `dlopen`；探针报的 `OH_Drawing_CreateFont` 不存在
      是我函数名猜错了，不是库缺失——真正的文字 API 是整套 typography

结论：**S3 走 `OH_Drawing`**，和 Pango/CoreText/DirectWrite 同一个位置，不自己接 freetype。

## S2 让 `ui` 在引擎里跑起来 —— 已完成

要回答：**`ui` 的事件循环能被一个"窗口在别处"的后端驱动吗？** 答案是能。

- [x] `internal/ohos`：`Backend` + `Window`（`Surface=true`）+ `Surface`
      （`PresentPixels`/`PresentDamage`/`PacedSurface`/`OccludableSurface`）
- [x] 其余 `Backend` 方法由内嵌的 `internal/unsupported` 兜底
- [x] `Size()` 与 `scale` 走启动参数（`--size`/`--scale`），避免首帧布局抢跑；
      之后的 resize 走 `SURFACE` 消息
- [x] 帧改为引擎驱动：`REQUEST` → `FRAME`(+fd) → `PRESENT`(rects)
- [x] hello-world = 框架自己的 `examples/counter-native`，用 `-tags mygo_ohos` 编
- [x] **上屏确认**：`snapshot_display` 已拍到真机窗口（`0`、`−`/`Reset`/`+`、`↑ and ↓ count too` 完整显示）

退出判据：`ui` 的 widget 树出现在屏幕上；`framePath` 报 `"drawn in memory"`。

## S3 文字（复用 Linux Pango musl 栈）—— 已完成

决策调整：放弃手写 1500+ 行 `OH_Drawing` 后端，利用 OHOS 的 Linux/musl ABI 兼容性，
直接随 HNP 附带 Pango 文字栈的 musl `.so` 闭包，**`pango_linux.go` 一行不改**，保持多平台行为与维护零分叉。

- [x] 还原 `internal/text/pango_linux.go`（零修改），移除临时空桩 `engine_ohos.go`
- [x] `tools/hnp` 用 `debug/elf` 自动解析 8 个根库（`gobject`/`pango`/`glib`/`pangocairo`/`cairo`/`harfbuzz`/`pangoft2`/`fontconfig`）的完整 `DT_NEEDED` 闭包（共 33 个 musl `.so`，压缩后增量 ~5.2MB，保留自带 `.codesign`）打入 `mygo_<ver>/lib/`
- [x] `ohos/fonts.conf` 打入 `mygo_<ver>/etc/fonts/fonts.conf`：直读系统 `/system/fonts`（优先 `HarmonyOS Sans` / `HarmonyOS Sans SC` / `HarmonyOS Sans TC` / `Noto Sans Mono` / `HMOS Color Emoji`），缓存写应用沙箱 `XDG_CACHE_HOME`
- [x] `host.cpp` 在 `execve` 前解析 `/data/app/bin/mygo-engine` 软链定位 HNP 包根，注入 `LD_LIBRARY_PATH`、`FONTCONFIG_FILE` 与沙箱 `HOME`/`XDG_*`/`TMPDIR`
- [x] `ohos.Backend.Theme()` 返回 `UIFont() = "HarmonyOS Sans"` 与 `FontRendering{Antialias: "gray", Hinting: "slight"}`
- [x] 真机验证：`mygo/internal/text` Pango 套件跑通，HAP 内首帧 311ms 上屏显示完整文字与符号

## S4 输入与 IME —— 已完成

要回答：**跟手性够不够，C vs D⁺ 的尾延迟到底差多少。** 答案是够，**保留 D⁺，无需翻案回 C**。

- [x] 纯 NDK 挂载 `ARKUI_NODE_XCOMPONENT`（`ContentSlot` + `OH_ArkUI_SurfaceHolder`），宿主在 C++ 侧直收触摸/鼠标/手写笔/滚轮/触控板捏合与惯性（`NODE_ON_COASTING_AXIS_EVENT`）/键盘/焦点事件并编进 `INPUT`，引擎解成 `SurfaceEvent`（修正了 SDK 英文注释误写 `in px`、实际 `OH_ArkUI_PointerEvent_GetX/Y` 默认返回 `vp` 的坐标单位陷阱，以及 PC 端 `OH_ArkUI_FocusRequest` 焦点激活与离屏惯性指针坐标保持）
- [x] 对齐 `mygo` 原生按需懒刷新与帧调度语义：分离 `Signal()`（跨协程 `Window.Update` 调度 `AppHandler.Dispatch`）与 `Wake()`（不吃掉 `Dispatch`），输入优先于待绘帧并合并连续 `PointerMove`/`Surface`，接入 `Paced: true`、`FrameRateSurface`（`OH_NativeVSync_SetExpectedFrameRateRange`）、`IdleSurface`（2s 静止自动释放闲置 swapchain `mmap`）与系统深浅色切换
- [x] `SetTextInput` → 宿主 → NDK `libohinputmethod.so`（`OH_InputMethodController_AttachWithUIContext` + `InputMethod_TextEditorProxy`，零隐藏 ArkTS `TextInput`），结合 `OH_ArkUI_NodeUtils_GetLayoutPositionInScreen` 物理 `px` 偏移同步光标候选词窗位置
- [x] `SetCursor` → 宿主 → NDK `libohinput.so`（`OH_Input_SetPointerStyle` / `OH_Input_SetPointerVisible`）
- [x] 触屏单指拖拽滚动、松手惯性衰减（VSync 步进 `stepFling`）、双指捏合缩放、`PointerCancel`
- [x] **测帧间隔与输入上屏延迟 p50/p90/p99**（MateBook Pro 2090×1324 @ 120Hz，`examples/gallery` 实测 `n=3527` 帧 / `in2flush n=1327`）：
      - 绘制往返 `draw`：**p50 = 1.53ms / p90 = 5.53ms / p99 = 8.53ms**（纯列表滚动 `n=2309` 时 **p50 = 1.04ms / p90 = 3.31ms / p99 = 7.08ms**）
      - 输入到 `FlushBuffer` 端到端 `in2flush`：**p50 = 3.41ms / p90 = 6.30ms / p99 = 7.97ms**（含重度整窗图表重绘全周期 `n=1407` 时 **p50 = 3.51ms / p90 = 7.03ms / p99 = 14.03ms**）
      - 120Hz 活跃帧间隔 `interval`：**p50 = 8.35–8.38ms / p90 = 9.52–11.59ms / p99 = 12.96–15.94ms**
      - 结论：跨进程 IPC 占比不足 0.05ms，端到端 `in2flush` 中位数仅半个 120Hz VSync（3.41ms），稳态 `p99` 落在 1–1.7 帧内，**D⁺ 架构完全满足 120Hz 桌面跟手性要求**

退出判据全部达成，数字与排障细节见 [devlog 第十三轮](docs/devlog/2026-11-11.md)。

---

> **后续演进策略（框架先行，应用验证）**：优先在独立工作区 `/storage/Users/currentUser/Workspace/mygo`（配合 OHOS 宿主层）分三批补齐 `mygo` 框架能力，尽量通过适配框架而非修改下游业务代码来抹平平台差异；待三批框架能力逐批测试提交后，直接以原版 `mygo-music` 作为端到端验收目标。

## S5 框架补齐 Batch 1：窗口系统、多窗口、沉浸式标题栏、原生菜单与屏幕/应用信息 —— 已完成

- [x] **真多窗口支持**：协议全链路按 `winID` 多路复用（主窗口 `1` 映射 Ability 主窗，`2+` 映射 `windowStage.createSubWindowWithOptions` 独立子窗 + 独立 `XComponent`/`OHNativeWindow`/异步 VSync），支持 `NewWindow`、`Show`/`ShowInactive`/`Hide`、`Close`/`ShouldClose`（`windowWillClose` 拦截）/`Closed`、`Minimize`/`Maximize`（`EXIT_IMMERSIVE`）/`Unmaximize`/`Restore`/`SetFullScreen`（`ENTER_IMMERSIVE`）、`SetTitle`、`SetBounds`/`Bounds`、`SetMinimumSize`/`SetMaximumSize`、`Center`、`Focus`/`Blur`、`SetAlwaysOnTop`
- [x] **沉浸式隐藏标题栏与自定义拖拽**：打通 `TitleBarHidden`/`TitleBarHiddenInset` → `setWindowDecorVisible(false)` + `setWindowDecorHeight` + `setWindowTitleButtonVisible` + `getTitleButtonRect()`/`on('windowTitleButtonRectChange')` → `Window.TitleBar()`/`TitleBarChanged()`，以及 `Window.StartDrag()` → `win.startMoving()`、`TitleBarDoubleClicked()` 缩放切换
- [x] **系统级弹出菜单（`Backend.PopupMenu`）**：支持 `SystemMenus` 默认模式下的右键上下文菜单、编辑框右键菜单与菜单按钮（序列化 `platform.Menu` 树 → 宿主指定窗口 `UIContext` 原生弹出菜单，引擎侧嵌套 `Step()` 阻塞等待并在返回前派发 `MenuItemClicked`）
- [x] **屏幕与打包应用元数据**：实现 `Screen.Displays()` / `CursorPoint()`（接入 `@ohos.display` 多显示器几何、可用工作区 `WorkArea` 与缩放比，监听显示器变更触发 `DisplaysChanged`）以及 `App.PreferredLanguages()` / `App.Region()` / `App.Package()`（接入 `@ohos.i18n` 与 `bundleManager`）
- [x] **退出判据**：`go test -count=5 ./internal/ohos` 全绿，真机验证原生右键弹出菜单、标题同步及屏幕/语言信息查询通过；框架宿主与 HNP 工具统一收归 `mygo` 仓库（`internal/ohos/host`、`internal/ohos/cmd/hnp`）

## S6 框架补齐 Batch 2：桌面通用 I/O（剪贴板、对话框/Picker、Shell、深链与单实例）

- [ ] **剪贴板（`Backend.Clipboard`）**：接入 `@ohos.pasteboard` 读写纯文本、HTML、PNG 图片与格式探测，打通文本框 `Ctrl+C`/`Ctrl+V`/`Ctrl+X` 及应用内复制操作
- [ ] **原生文件/目录选择器与消息框（`Backend.Dialogs`）**：接入 `@ohos.file.picker`（`DocumentViewPicker` 多选文件/目录选择、保存面板，处理 URI 授权到引擎可读路径的桥接）与 `UIContext.showAlertDialog` 消息框
- [ ] **Shell 与系统集成（`Backend.Shell`）**：实现 `OpenExternal`（`context.openLink` / `startAbility` 打开外部浏览器）、`OpenPath`、`ShowItemInFolder`
- [ ] **URL 深链与单实例激活**：在 `EntryAbility` 的 `onCreate` / `onNewWant` 中捕获 `want.uri` 与文件参数，桥接至 `AppHandler.OpenURLs` / `OpenFiles` / `Activated`
- [ ] **退出判据**：单元测试与真机验证剪贴板双向复制粘贴、系统文件/文件夹选择返回可读路径、消息弹窗与 `mygo-music://` 深链唤起通过

## S7 框架补齐 Batch 3：音频输出、媒体播控、通知与后台运行

- [ ] **原生音频输出（`audio/output_ohos.go`）**：用 `purego` 直连 `libohaudio.so`（`OH_AudioStreamBuilder` + 低延迟 PCM 渲染流回调或宿主桥接，对齐 `audio/output_linux.go` 接口，使 `github.com/egoist/mygo/audio` 开箱发声）
- [ ] **系统媒体播控中心（`Backend.NowPlaying`）**：接入 `@ohos.multimedia.avsession`（同步曲名、歌手、专辑、封面、时长、进度与倍速，将系统播控面板与键盘媒体键映射回 `AppHandler.MediaCommand`）
- [ ] **桌面通知与电源/后台保活**：接入 `@ohos.notificationManager`（`ShowNotification`/`NotificationClicked`）以及 `@ohos.resourceschedule.backgroundTaskManager`（`AUDIO_PLAYBACK` 长时任务 + `Power.KeepRunning`）
- [ ] **退出判据**：原生 PCM 音频稳定播放/Seek/暂停无爆音，控制中心与媒体键双向联动，切后台或锁屏继续播放且窗口暂停无谓重绘

## S8 端到端验证与框架化

- [ ] **零业务侵入运行 `mygo-music` 全量验证**：本地目录扫描导入、SQLite（`modernc`/`moejs`）、歌词与封面渲染、子设置窗、无标题栏拖拽、播控中心与深链登录全链路验收
- [ ] 协议与宿主 `.so`/ArkTS 壳搬进 `cmd/mygo/template/ohos`
- [ ] `mygo build -platform ohos/arm64`：交叉编译 + 打包 HNP + 签名 + `hvigorw`
- [ ] `-tags mygo_ohos` 的构建约定写进框架文档

## 不进这个路线图

| 东西 | 为什么不 |
|---|---|
| 离屏 GL（`EGL_NATIVE_BUFFER_OHOS`） | 软件光栅在 `ui` 是一等公民；实测撑不住再做，且只换渲染器不动协议 |
| 页内 WebView（`ArkWeb` 混合合成） | 纯 `ui` 自绘桌面应用无需 WebView；`WebViewAvailable() == false` 已是框架一等契约 |
| 辅助功能、跨应用拖放、系统玻璃 | `platform` 有接口，先保持空实现或 `ErrUnsupported` |
| 方案 C | 见 [c-in-process-cgo.md](docs/design/c-in-process-cgo.md)，翻案条件是 S4 的 p99（实测已通过 D⁺） |
