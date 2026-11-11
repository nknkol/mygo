# 方案 C：进程内 cgo（Go 是被 ArkTS 加载的 NAPI 模块）

> 状态：**搁置，且至今未在设备上验证过任何一行。**
> 下文全部是纸面推理——工具链、装载顺序、信号、musl 兼容性，没有一条经过实测。
> 目前唯一端到端跑通的是 D⁺，见 [d-out-of-process.md](d-out-of-process.md)。

## 形态

```
ArkTS 壳（EntryAbility + XComponent）
      │ import libmygo.so
      ▼
libmygo.so  ← go build -buildmode=c-shared，唯一一处 cgo
  ├─ NAPI 注册（ELF 构造函数 + napi_module + napi_module_register）
  ├─ 其余全部 purego：napi_* / EGL / GLES / OH_NativeWindow / OH_NativeVsync / OH_Pasteboard
  └─ ui → internal/gpu/gl/load_ohos.go → eglSwapBuffers
```

线程模型：ArkUI 拥有主线程，Go 的 `Run` 等在 channel 上，`onMain` 用
`napi_create_threadsafe_function` 回到 UI 线程，`Step` 退化成条件变量。

## 为什么不选

约束 3。方案 C 需要 `import "C"` 才能被 loader 认领：Go 无法在 `CGO_ENABLED=0` 下
导出 C 符号（`-buildmode=c-shared` 强制 cgo），而 NAPI 的入口就是"ELF 构造函数里
调宿主的 `napi_module_register`"。这个文件放进框架仓库，与 MyGo 的构建理念冲突。

**注意区分方向**：MyGo 现有的原生模块机制是 **Go → C**（Go 用 purego 加载 .so）；
方案 C 需要 **C → Go**（.so 被 ArkTS 加载、回调进 Go）。机制表达不了反方向，
不是"不许写 C"，是"没有 cgo 就没有被 loader 认领的动作"。

## 保留的变体：C′-b（cgo 在应用侧）

框架导出 `ohos.Start(env, exports, entry)`（纯 Go），应用自带 25 行
`//go:build linux && arm64 && mygo_ohos && cgo` 的桥。框架仍然 0 处 `import "C"`。

- 优点：性能最好（零进程边界）、代码最少、故障模型与桌面一致。
- 代价：**应用**要为 ohos 目标启用 cgo 和 NDK 的 clang。
- 何时翻案：如果 D 的延迟（输入多两跳、一帧上屏延迟）在真机上不可接受，
  C′-b 是退路，且不需要改 `ui`。

## 已知风险（真要做的话）

| 风险 | 说明 |
|---|---|
| musl 上的 purego | Linux 后端在 glibc 上跑了很久，musl 要重新验 |
| 信号处理器冲突 | Go 会装 SIGSEGV/SIGURG，ArkTS 进程里可能已有别人的 |
| 崩溃域 | Go 崩 = 整个应用崩，与桌面一致但和 D 不同 |
