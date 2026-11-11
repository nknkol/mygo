# HarmonyOS 后端设计

MyGo Music 要把原生 Go 后端带到 HarmonyOS 2in1 上。这里记录**为什么**这么设计、
**放弃了什么**，以及每个决定的证据来源。

## 约束（按优先级）

1. **不用 web。** 用 MyGo 的理由就是不用 WebView（`mygo.config.ts` 的 `linux.webview: false`、
   整个 `ui` 包）。ArkWeb + wasm 的方案因此出局。
2. **复用 `ui`。** 视图层重写成 ArkTS 等于放弃 MyGo。所以"Go 只做引擎、ArkTS 画"出局。
3. **Go 代码不依赖 cgo 编译机制。** 这是 MyGo 的理念，不是"不许有 C"：
   原生库由构建系统单独编译、由 purego 加载（SQLite/Zig、Terminal/Ghostty 都是这么做的）。
   推论：框架仓库里不能出现 `import "C"`。
4. **一行像素不能白拷。** 满屏帧 1512×982×4 ≈ 5.9MB，60fps 就是 355MB/s，不能靠 memcpy 硬扛。

## 结论

**方案 D（D⁺：出进程 + 缓冲区 fd 直传）** 是唯一同时满足四条约束的形态：

```
HAP（ArkTS + 极薄宿主 .so）
  ├─ 创建 UIAbility / XComponent
  ├─ RequestBuffer() 拿图形 Buffer
  ├─ Unix Socket + SCM_RIGHTS 传 Buffer fd / stride / size
  ▼
MyGo ELF（CGO_ENABLED=0）
  ├─ mmap(fd)
  ├─ internal/raster 软件光栅（或离屏 GL，见下）
  ├─ 直接写入映射内存
  └─ FRAME_READY + 脏矩形
  ▼
HAP
  └─ FlushBuffer(win, buf, -1, region)
```

它不是 E（parcel 传 `OHNativeWindow`）：E 需要 IPC Kit 端点 + 跨进程引用计数，
而官方《图形缓冲区常见稳定性问题》写着"超过 90% 的问题来自引用计数不匹配"。
D⁺ 传的是 **fd**，所有权天然清晰，没有 parcel、没有 stub/proxy、没有跨进程句柄。

## 实测状态（2026-11-11）

**被实测过的只有一个方案。** 其余是设计，不是结论。

| 方案 | 形态 | 设备上验证到哪一步 | 证据 |
|---|---|---|---|
| **D⁺** | 引擎 `-buildmode=exe` + `CGO_ENABLED=0`，SCM_RIGHTS 传 fd | **端到端跑通**：ArkTS → 宿主 `.so` → 引擎 `mmap` → `FlushBuffer` → 屏幕上有动的画面 | devlog 第五～八轮 |
| **C** | `-buildmode=c-shared`，Go runtime 装在被加载的 `.so` 里 | **零**。工具链、装载、装载顺序、信号、musl，一条都没跑过 | 只有 [c-in-process-cgo.md](c-in-process-cgo.md) |
| **C′** | 同上，桥放应用侧 | **零** | 同上 §C′-b |

D⁺ 已经证明的，恰好是 C 需要的两头：**ArkTS 能 `dlopen` 一个 NAPI `.so`**（`libmygo_host.so`），
**Go 进程能拿到图形 buffer 的 fd、`mmap` 后画上去**。C 缺的是中间那一步——
让**被加载的那个 `.so` 里面装着 Go runtime**。

D⁺ 自身也还没证明的：**GL**。现在写进 buffer 的是 CPU 算出来的纯色带，
`ui` 桌面默认走 `internal/gpu/gl`；要在没有窗口的进程里跑 GL，得走
`EGL_NATIVE_BUFFER_OHOS` + `eglCreateImageKHR` 离屏渲染，这条未验。
输入、IME、resize、崩溃恢复也都未做。

## 文档

| 文件 | 内容 |
|---|---|
| [c-in-process-cgo.md](c-in-process-cgo.md) | 方案 C：Go 作为 NAPI 模块活在 ArkTS 进程里。**当前不选**，但记下翻案条件 |
| [d-out-of-process.md](d-out-of-process.md) | 方案 D/D⁺：进程模型、协议、时序、失败恢复、验收 |
| [../devlog/](../devlog/) | 每次动手的记录：读到了什么、验了什么、改了什么 |

## 已排除的路线（别再绕回来）

| 路线 | 被谁堵死 |
|---|---|
| Go 当应用进程的 `main` | `module.json5` 的 `srcEntry` 只能是 `.ets` |
| 用 surfaceId 跨进程重建 surface | 官方："不能跨进程获取 surface" |
| Go 当 `startNativeChildProcess` 入口 | 入口必须是 `.so` 的符号（`libentry.so:Main`） |
| ArkWeb + wasm | 约束 1 |
| Go 引擎 + ArkTS 界面 | 约束 2 |
| 框架里放 `import "C"` | 约束 3 |
