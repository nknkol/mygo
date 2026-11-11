// The pure-NDK host half of the MyGo HarmonyOS backend:
//   - Multiplexes multiple windows (winId 1 = main UIAbility window, 2+ =
//     WindowStage subwindows), each mounting an ARKUI_NODE_XCOMPONENT into
//     ArkTS's ContentSlot with its own OHNativeWindow & OH_NativeVSync loop.
//   - Captures touch, mouse, stylus, hover, wheel, touchpad scroll, pinch,
//     coasting inertia, keyboard, and focus directly on each NDK node.
//   - Bridges SetTextInput to HarmonyOS IME C API (libohinputmethod.so) and
//     SetCursor to MultimodalInput C API (libohinput.so).
//   - Forwards window creation/config/actions and native popup menus to ArkTS
//     via a single typed N-API threadsafe function, and injects package,
//     locale/region, and multi-display metadata both into execve environment
//     variables (safe before Init) and live kEnvInfo updates.
#include <napi/native_api.h>
#include <hilog/log.h>
#include <arkui/native_interface.h>
#include <arkui/native_interface_focus.h>
#include <arkui/native_node.h>
#include <arkui/native_node_napi.h>
#include <arkui/ui_input_event.h>
#include <arkui/native_key_event.h>
#include <ace/xcomponent/native_interface_xcomponent.h>
#include <inputmethod/inputmethod_controller_capi.h>
#include <multimodalinput/oh_input_manager.h>
#include <multimodalinput/oh_pointer_style.h>
#include <native_window/external_window.h>
#include <native_window/buffer_handle.h>
#include <native_buffer/native_buffer.h>
#include <native_vsync/native_vsync.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <climits>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>
#include <poll.h>
#include <semaphore.h>
#include <signal.h>
#include <time.h>
#include <unistd.h>
#include <fcntl.h>
#include <linux/dma-buf.h>
#include <sys/ioctl.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>

#undef LOG_DOMAIN
#undef LOG_TAG
#define LOG_DOMAIN 0x0000
#define LOG_TAG "mygo-host"

namespace {

constexpr const char *kEngine = "/data/app/bin/mygo-engine";

// Wire message kinds (must match mygo/internal/ohos/proto.go).
constexpr uint8_t kSurface     = 1;  // {u32 winId, u32 w, u32 h, u32 scale×1000, u32 milliHz}
constexpr uint8_t kFrame       = 2;  // {u32 winId, u32 seq, u32 id, u32 w, u32 h, u32 stride, u32 size} + fd
constexpr uint8_t kInput       = 3;  // {u32 winId, u8 subkind, u8 op, ...}
constexpr uint8_t kQuit        = 4;  // {}
constexpr uint8_t kRequest     = 5;  // {u32 winId}
constexpr uint8_t kPresent     = 6;  // {u32 winId, u32 seq, u32 n, n × {i32 x, i32 y, u32 w, u32 h}}
constexpr uint8_t kTextInput   = 7;  // {u32 winId, u8 flags, 3×pad, 4×f32 caret, i32 selStart, i32 selEnd, u32 n, utf8[n]}
constexpr uint8_t kCursor      = 8;  // {u32 winId, u8 cursor, 3×pad}
constexpr uint8_t kAbort       = 9;  // {u32 winId, u32 seq}
constexpr uint8_t kFrameRate   = 10; // {u32 winId, u32 fps}
constexpr uint8_t kEnvInfo     = 11; // host -> engine: package, locale/region, displays snapshot
constexpr uint8_t kPopupResult = 12; // host -> engine: {u32 reqId, i32 chosenId}
constexpr uint8_t kWinCreate   = 13; // engine -> host: initial window config
constexpr uint8_t kWinConfig   = 14; // engine -> host: masked window config update
constexpr uint8_t kWinAction   = 15; // engine -> host: {u32 winId, u8 action, 3×pad}
constexpr uint8_t kPopupMenu   = 16; // engine -> host: popup menu tree

// Input subkinds.
constexpr uint8_t kInputMouse  = 1;
constexpr uint8_t kInputTouch  = 2;
constexpr uint8_t kInputScroll = 3;
constexpr uint8_t kInputZoom   = 4;
constexpr uint8_t kInputKey    = 5;
constexpr uint8_t kInputWindow = 6;
constexpr uint8_t kInputIME    = 7;

// Mouse ops.
constexpr uint8_t kMouseMove   = 1;
constexpr uint8_t kMouseDown   = 2;
constexpr uint8_t kMouseUp     = 3;
constexpr uint8_t kMouseCancel = 4;
constexpr uint8_t kMouseLeave  = 5;

// Touch ops.
constexpr uint8_t kTouchDown   = 1;
constexpr uint8_t kTouchMove   = 2;
constexpr uint8_t kTouchUp     = 3;
constexpr uint8_t kTouchCancel = 4;

// Scroll flags.
constexpr uint8_t kScrollPrecise  = 1 << 0;
constexpr uint8_t kScrollMomentum = 1 << 1;

// Zoom ops.
constexpr uint8_t kZoomBegin  = 1;
constexpr uint8_t kZoomUpdate = 2;
constexpr uint8_t kZoomEnd    = 3;

// Key ops.
constexpr uint8_t kKeyDown = 1;
constexpr uint8_t kKeyUp   = 2;

// Window ops (must match proto.go winFocus..winTitleBar).
constexpr uint8_t kWinFocus           = 1;
constexpr uint8_t kWinBlur            = 2;
constexpr uint8_t kWinShow            = 3;
constexpr uint8_t kWinHide            = 4;
constexpr uint8_t kWinThemeLight      = 5;
constexpr uint8_t kWinThemeDark       = 6;
constexpr uint8_t kWinMinimize        = 7;
constexpr uint8_t kWinRestore         = 8;
constexpr uint8_t kWinMaximize        = 9;
constexpr uint8_t kWinUnmaximize      = 10;
constexpr uint8_t kWinEnterFullScreen = 11;
constexpr uint8_t kWinLeaveFullScreen = 12;
constexpr uint8_t kWinCloseRequest    = 13;
constexpr uint8_t kWinClosed          = 14;
constexpr uint8_t kWinRect            = 15;
constexpr uint8_t kWinTitleBar        = 16;

// IME ops.
constexpr uint8_t kImeCommit            = 1;
constexpr uint8_t kImePreedit           = 2;
constexpr uint8_t kImePreeditEnd        = 3;
constexpr uint8_t kImeDeleteSurrounding = 4;
constexpr uint8_t kImeSetSelection      = 5;
constexpr uint8_t kImeCommand           = 6;

// Modifiers matching mygo platform.Modifiers.
constexpr uint32_t kModShift = 1 << 0;
constexpr uint32_t kModCtrl  = 1 << 1;
constexpr uint32_t kModAlt   = 1 << 2;
constexpr uint32_t kModSuper = 1 << 3;

int64_t monoNs() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<int64_t>(ts.tv_sec) * 1000000000LL + ts.tv_nsec;
}

class LatencyRing {
public:
    static constexpr size_t kCap = 512;

    void add(double ms) {
        if (samples_.size() < kCap) {
            samples_.push_back(ms);
        } else {
            samples_[head_] = ms;
            head_ = (head_ + 1) % kCap;
        }
        totalCount_++;
    }

    size_t size() const { return samples_.size(); }
    uint64_t totalCount() const { return totalCount_; }

    void summary(double *p50, double *p90, double *p99, double *maxVal) const {
        if (samples_.empty()) {
            *p50 = *p90 = *p99 = *maxVal = 0;
            return;
        }
        std::vector<double> sorted = samples_;
        std::sort(sorted.begin(), sorted.end());
        auto pick = [&](double q) {
            size_t idx = static_cast<size_t>(std::ceil(q * sorted.size()));
            if (idx > 0) {
                idx--;
            }
            if (idx >= sorted.size()) {
                idx = sorted.size() - 1;
            }
            return sorted[idx];
        };
        *p50 = pick(0.50);
        *p90 = pick(0.90);
        *p99 = pick(0.99);
        *maxVal = sorted.back();
    }

private:
    std::vector<double> samples_;
    size_t head_ = 0;
    uint64_t totalCount_ = 0;
};

// UTF-8 <-> UTF-16 conversion without deprecated <codecvt>.
std::string utf16ToUtf8(const char16_t *src, size_t len) {
    std::string out;
    if (src == nullptr || len == 0) {
        return out;
    }
    out.reserve(len * 3);
    for (size_t i = 0; i < len; ++i) {
        uint32_t cp = src[i];
        if (cp >= 0xD800 && cp <= 0xDBFF) {
            if (i + 1 < len && src[i + 1] >= 0xDC00 && src[i + 1] <= 0xDFFF) {
                cp = 0x10000 + (((cp - 0xD800) << 10) | (src[i + 1] - 0xDC00));
                ++i;
            } else {
                cp = 0xFFFD;
            }
        } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
            cp = 0xFFFD;
        }
        if (cp == 0) {
            continue;
        }
        if (cp < 0x80) {
            out.push_back(static_cast<char>(cp));
        } else if (cp < 0x800) {
            out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
            out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
        } else if (cp < 0x10000) {
            out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
            out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
        } else {
            out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
            out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
            out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
        }
    }
    return out;
}

std::u16string utf8ToUtf16(const std::string &src) {
    std::u16string out;
    out.reserve(src.size());
    size_t i = 0;
    const size_t n = src.size();
    while (i < n) {
        uint8_t c = static_cast<uint8_t>(src[i]);
        uint32_t cp = 0;
        size_t extra = 0;
        if (c < 0x80) {
            cp = c;
            extra = 0;
        } else if ((c & 0xE0) == 0xC0) {
            cp = c & 0x1F;
            extra = 1;
        } else if ((c & 0xF0) == 0xE0) {
            cp = c & 0x0F;
            extra = 2;
        } else if ((c & 0xF8) == 0xF0) {
            cp = c & 0x07;
            extra = 3;
        } else {
            out.push_back(0xFFFD);
            ++i;
            continue;
        }
        if (i + extra >= n) {
            out.push_back(0xFFFD);
            break;
        }
        bool ok = true;
        for (size_t k = 1; k <= extra; ++k) {
            uint8_t cc = static_cast<uint8_t>(src[i + k]);
            if ((cc & 0xC0) != 0x80) {
                ok = false;
                break;
            }
            cp = (cp << 6) | (cc & 0x3F);
        }
        i += extra + 1;
        if (!ok) {
            out.push_back(0xFFFD);
            continue;
        }
        if (cp < 0x10000) {
            out.push_back(static_cast<char16_t>(cp));
        } else if (cp <= 0x10FFFF) {
            cp -= 0x10000;
            out.push_back(static_cast<char16_t>(0xD800 | (cp >> 10)));
            out.push_back(static_cast<char16_t>(0xDC00 | (cp & 0x3FF)));
        } else {
            out.push_back(0xFFFD);
        }
    }
    return out;
}

// ---------- Wire Helpers ----------

void appendU32(std::vector<uint8_t> &b, uint32_t v) {
    uint8_t tmp[4];
    memcpy(tmp, &v, 4);
    b.insert(b.end(), tmp, tmp + 4);
}

void appendStr(std::vector<uint8_t> &b, const std::string &s) {
    appendU32(b, static_cast<uint32_t>(s.size()));
    if (!s.empty()) {
        b.insert(b.end(), reinterpret_cast<const uint8_t *>(s.data()),
                 reinterpret_cast<const uint8_t *>(s.data()) + s.size());
    }
}

bool readU32(const std::vector<uint8_t> &b, size_t &off, uint32_t *out) {
    if (off + 4 > b.size()) {
        return false;
    }
    memcpy(out, b.data() + off, 4);
    off += 4;
    return true;
}

bool readStr(const std::vector<uint8_t> &b, size_t &off, std::string *out) {
    uint32_t n = 0;
    if (!readU32(b, off, &n) || off + n > b.size()) {
        return false;
    }
    out->assign(reinterpret_cast<const char *>(b.data() + off), n);
    off += n;
    return true;
}

// ---------- Shared Engine Socket & Environment State ----------

ArkUI_NativeNodeAPI_1 *nodeAPI = nullptr;

std::mutex sendMu;
int sock = -1;
pid_t enginePid = -1;
std::atomic<bool> gSupervisorRunning{false};
std::atomic<bool> gEngineCleanQuit{false};
std::atomic<bool> gFrameTimeoutRestart{false};
std::thread gSupervisorThread;
bool currentDark = false;

int engineLog[2] = {-1, -1};
std::thread logReader;

struct CachedEnv {
    std::string pkgName;
    std::string pkgVer;
    std::string pkgId;
    std::string locale;
    int32_t firstWeekday = -1;
    int32_t hourCycle = 0;
    std::string langsCsv;
    std::string displaysSpec;
    std::vector<uint8_t> wirePayload;
};

std::mutex gEnvMu;
CachedEnv gCachedEnv;

// ---------- Per-Window Session ----------

enum class BufReply {
    None,
    Present,
    Abort,
    Reset,
};

struct WinSession {
    uint32_t winId = 0;

    // UI-thread state.
    ArkUI_ContextHandle uiContext = nullptr;
    ArkUI_NodeContentHandle nodeContent = nullptr;
    ArkUI_NodeHandle xcNode = nullptr;
    OH_ArkUI_SurfaceHolder *surfaceHolder = nullptr;
    OH_ArkUI_SurfaceCallback *surfaceCallback = nullptr;
    int32_t arkWindowId = 0;
    bool nodeFocused = false;
    bool metaLeftDown = false;
    bool metaRightDown = false;
    float lastPointerX = 0.0f;
    float lastPointerY = 0.0f;
    int32_t appliedPointerStyle = -1;

    // Cursor & input latency atomics.
    std::atomic<uint8_t> desiredCursor{0};
    std::atomic<int64_t> lastInputNs{0};
    std::atomic<bool> inputSincePresent{false};

    // Render thread state (guarded by mu).
    std::mutex mu;
    std::condition_variable cv;
    std::thread renderThread;
    bool renderRunning = false;
    bool stopRender = false;
    OHNativeWindow *nativeWindow = nullptr;
    OH_NativeVSync *vsync = nullptr;
    sem_t vsyncSem{};
    bool vsyncInited = false;
    int32_t curW = 0;
    int32_t curH = 0;
    int32_t targetW = 0;
    int32_t targetH = 0;
    bool forceResendSurface = false;
    double scale = 1.0;
    double hz = 120.0;
    bool wantFrame = false;
    BufReply reply = BufReply::None;
    uint32_t replySeq = 0;
    uint32_t replyRectCount = 0;
    uint32_t requestedFps = 0;
    bool fpsDirty = true;

    void waitVsync();
    void applyExpectedFrameRateLocked(uint32_t fps);
    void renderLoop();
};

std::mutex gWinMu;
std::unordered_map<uint32_t, std::shared_ptr<WinSession>> gWindows;
// UI-thread lookup maps.
std::unordered_map<ArkUI_NodeHandle, uint32_t> gNodeToWinId;
std::unordered_map<OH_ArkUI_SurfaceHolder *, uint32_t> gHolderToWinId;
bool pointerHidden = false;

std::shared_ptr<WinSession> findSession(uint32_t winId) {
    std::lock_guard<std::mutex> lk(gWinMu);
    auto it = gWindows.find(winId);
    return it != gWindows.end() ? it->second : nullptr;
}

std::shared_ptr<WinSession> getOrCreateSession(uint32_t winId) {
    std::lock_guard<std::mutex> lk(gWinMu);
    auto &sp = gWindows[winId];
    if (!sp) {
        sp = std::make_shared<WinSession>();
        sp->winId = winId;
    }
    return sp;
}

// ---------- Bridge to ArkTS (Single Threadsafe Function) ----------

enum class BridgeEventKind {
    WinCreate,
    WinConfig,
    WinAction,
    PopupMenu,
    EngineQuit,
    EngineRestart,
};

struct WinConfigData {
    uint32_t winId = 0;
    uint32_t parentOrMask = 0;
    int32_t x = 0, y = 0, w = 0, h = 0;
    int32_t minW = 0, minH = 0;
    int32_t maxW = 0, maxH = 0;
    uint32_t flags = 0;
    int32_t titleBarHeight = 0;
    uint32_t bgRGBA = 0;
    std::string title;
};

struct PopupItemData {
    int32_t id = 0;
    uint8_t kind = 0;
    bool enabled = false;
    bool checked = false;
    std::string label;
    std::string accel;
    std::vector<PopupItemData> items;
};

struct BridgeEvent {
    BridgeEventKind kind = BridgeEventKind::WinAction;
    WinConfigData winCfg;
    uint32_t winId = 0;
    uint8_t action = 0;
    uint32_t popupReqId = 0;
    int32_t popupX = 0;
    int32_t popupY = 0;
    bool popupRtl = false;
    std::vector<PopupItemData> popupItems;
};

std::mutex gBridgeMu;
napi_threadsafe_function gBridgeTsfn = nullptr;
napi_ref gBridgeObjRef = nullptr;
std::vector<std::unique_ptr<BridgeEvent>> gQueuedBridgeEvents;

napi_value makeJsWinConfig(napi_env env, const WinConfigData &c) {
    napi_value obj = nullptr;
    napi_create_object(env, &obj);
    auto setI32 = [&](const char *k, int32_t v) {
        napi_value nv = nullptr;
        napi_create_int32(env, v, &nv);
        napi_set_named_property(env, obj, k, nv);
    };
    auto setU32 = [&](const char *k, uint32_t v) {
        napi_value nv = nullptr;
        napi_create_uint32(env, v, &nv);
        napi_set_named_property(env, obj, k, nv);
    };
    setU32("winId", c.winId);
    setU32("parentOrMask", c.parentOrMask);
    setI32("x", c.x);
    setI32("y", c.y);
    setI32("w", c.w);
    setI32("h", c.h);
    setI32("minW", c.minW);
    setI32("minH", c.minH);
    setI32("maxW", c.maxW);
    setI32("maxH", c.maxH);
    setU32("flags", c.flags);
    setI32("titleBarHeight", c.titleBarHeight);
    setU32("bgRGBA", c.bgRGBA);
    napi_value titleVal = nullptr;
    napi_create_string_utf8(env, c.title.c_str(), c.title.size(), &titleVal);
    napi_set_named_property(env, obj, "title", titleVal);
    return obj;
}

napi_value makeJsMenuItems(napi_env env, const std::vector<PopupItemData> &items) {
    napi_value arr = nullptr;
    napi_create_array_with_length(env, items.size(), &arr);
    for (size_t i = 0; i < items.size(); ++i) {
        const PopupItemData &it = items[i];
        napi_value obj = nullptr;
        napi_create_object(env, &obj);

        napi_value vId = nullptr, vKind = nullptr, vEn = nullptr, vCh = nullptr, vLbl = nullptr, vAcc = nullptr;
        napi_create_int32(env, it.id, &vId);
        napi_create_uint32(env, it.kind, &vKind);
        napi_get_boolean(env, it.enabled, &vEn);
        napi_get_boolean(env, it.checked, &vCh);
        napi_create_string_utf8(env, it.label.c_str(), it.label.size(), &vLbl);
        napi_create_string_utf8(env, it.accel.c_str(), it.accel.size(), &vAcc);
        napi_value vSub = makeJsMenuItems(env, it.items);

        napi_set_named_property(env, obj, "id", vId);
        napi_set_named_property(env, obj, "kind", vKind);
        napi_set_named_property(env, obj, "enabled", vEn);
        napi_set_named_property(env, obj, "checked", vCh);
        napi_set_named_property(env, obj, "label", vLbl);
        napi_set_named_property(env, obj, "accel", vAcc);
        napi_set_named_property(env, obj, "items", vSub);

        napi_set_element(env, arr, static_cast<uint32_t>(i), obj);
    }
    return arr;
}

void callBridgeMethod(napi_env env, napi_value cbObj, const char *name, size_t argc, napi_value *argv) {
    napi_value fn = nullptr;
    if (napi_get_named_property(env, cbObj, name, &fn) != napi_ok || fn == nullptr) {
        return;
    }
    napi_valuetype vt = napi_undefined;
    if (napi_typeof(env, fn, &vt) != napi_ok || vt != napi_function) {
        return;
    }
    napi_value ignored = nullptr;
    napi_call_function(env, cbObj, fn, argc, argv, &ignored);
}

void BridgeCallJs(napi_env env, napi_value, void *, void *data) {
    std::unique_ptr<BridgeEvent> ev(static_cast<BridgeEvent *>(data));
    if (env == nullptr || !ev) {
        return;
    }
    napi_value cbObj = nullptr;
    {
        std::lock_guard<std::mutex> lk(gBridgeMu);
        if (gBridgeObjRef == nullptr) {
            return;
        }
        napi_get_reference_value(env, gBridgeObjRef, &cbObj);
    }
    if (cbObj == nullptr) {
        return;
    }

    switch (ev->kind) {
    case BridgeEventKind::WinCreate: {
        napi_value arg = makeJsWinConfig(env, ev->winCfg);
        callBridgeMethod(env, cbObj, "onWinCreate", 1, &arg);
        break;
    }
    case BridgeEventKind::WinConfig: {
        napi_value arg = makeJsWinConfig(env, ev->winCfg);
        callBridgeMethod(env, cbObj, "onWinConfig", 1, &arg);
        break;
    }
    case BridgeEventKind::WinAction: {
        napi_value args[2] = {nullptr, nullptr};
        napi_create_uint32(env, ev->winId, &args[0]);
        napi_create_uint32(env, ev->action, &args[1]);
        callBridgeMethod(env, cbObj, "onWinAction", 2, args);
        break;
    }
    case BridgeEventKind::PopupMenu: {
        napi_value args[6] = {nullptr, nullptr, nullptr, nullptr, nullptr, nullptr};
        napi_create_uint32(env, ev->popupReqId, &args[0]);
        napi_create_uint32(env, ev->winId, &args[1]);
        napi_create_int32(env, ev->popupX, &args[2]);
        napi_create_int32(env, ev->popupY, &args[3]);
        napi_get_boolean(env, ev->popupRtl, &args[4]);
        args[5] = makeJsMenuItems(env, ev->popupItems);
        callBridgeMethod(env, cbObj, "onPopupMenu", 6, args);
        break;
    }
    case BridgeEventKind::EngineQuit:
        callBridgeMethod(env, cbObj, "onEngineQuit", 0, nullptr);
        break;
    case BridgeEventKind::EngineRestart:
        callBridgeMethod(env, cbObj, "onEngineRestart", 0, nullptr);
        break;
    }
}

void postBridgeEvent(std::unique_ptr<BridgeEvent> ev) {
    std::lock_guard<std::mutex> lk(gBridgeMu);
    if (gBridgeTsfn == nullptr) {
        if (gQueuedBridgeEvents.size() < 64) {
            gQueuedBridgeEvents.push_back(std::move(ev));
        }
        return;
    }
    BridgeEvent *raw = ev.release();
    if (napi_call_threadsafe_function(gBridgeTsfn, raw, napi_tsfn_nonblocking) != napi_ok) {
        delete raw;
    }
}

// ---------- Socket Read/Write Primitives ----------

bool readFull(int fd, void *p, size_t n) {
    char *c = static_cast<char *>(p);
    while (n > 0) {
        ssize_t r = read(fd, c, n);
        if (r < 0) {
            if (errno == EINTR) {
                continue;
            }
            return false;
        }
        if (r == 0) {
            return false;
        }
        c += r;
        n -= static_cast<size_t>(r);
    }
    return true;
}

bool sendMessage(uint8_t kind, const void *payload, uint32_t len, int fd) {
    std::lock_guard<std::mutex> lk(sendMu);
    if (sock < 0) {
        return false;
    }
    uint8_t hdr[5];
    hdr[0] = kind;
    memcpy(hdr + 1, &len, 4);

    struct msghdr mh;
    memset(&mh, 0, sizeof(mh));
    struct iovec iov[2];
    iov[0].iov_base = hdr;
    iov[0].iov_len = sizeof(hdr);
    iov[1].iov_base = const_cast<void *>(payload);
    iov[1].iov_len = len;
    mh.msg_iov = iov;
    mh.msg_iovlen = 2;

    char cbuf[CMSG_SPACE(sizeof(int))];
    memset(cbuf, 0, sizeof(cbuf));
    if (fd >= 0) {
        mh.msg_control = cbuf;
        mh.msg_controllen = sizeof(cbuf);
        struct cmsghdr *c = CMSG_FIRSTHDR(&mh);
        c->cmsg_level = SOL_SOCKET;
        c->cmsg_type = SCM_RIGHTS;
        c->cmsg_len = CMSG_LEN(sizeof(int));
        memcpy(CMSG_DATA(c), &fd, sizeof(int));
    }
    for (;;) {
        ssize_t sent = sendmsg(sock, &mh, MSG_NOSIGNAL);
        if (sent < 0 && errno == EINTR) {
            continue;
        }
        return sent == static_cast<ssize_t>(sizeof(hdr) + len);
    }
}

bool sendSurface(uint32_t winId, int32_t w, int32_t h, double scale, double hz) {
    uint32_t p[5] = {
        winId,
        static_cast<uint32_t>(w),
        static_cast<uint32_t>(h),
        static_cast<uint32_t>(std::lround(scale * 1000.0)),
        static_cast<uint32_t>(std::lround(hz * 1000.0)),
    };
    return sendMessage(kSurface, p, sizeof(p), -1);
}

bool sendFrame(uint32_t winId, uint32_t seq, uint32_t id, int32_t w, int32_t h,
               int32_t stride, uint32_t size, int fd) {
    uint32_t p[7] = {
        winId, seq, id,
        static_cast<uint32_t>(w),
        static_cast<uint32_t>(h),
        static_cast<uint32_t>(stride),
        size,
    };
    return sendMessage(kFrame, p, sizeof(p), fd);
}

void markInputArrival(WinSession *ws) {
    if (ws != nullptr) {
        ws->lastInputNs.store(monoNs(), std::memory_order_relaxed);
        ws->inputSincePresent.store(true, std::memory_order_relaxed);
    }
}

void sendMouseInput(WinSession *ws, uint8_t op, uint8_t button, uint8_t pointerKind,
                    uint32_t mods, uint32_t timeMs, float x, float y, float pressure) {
    if (ws == nullptr) {
        return;
    }
    markInputArrival(ws);
    uint8_t p[28] = {0};
    memcpy(p + 0, &ws->winId, 4);
    p[4] = kInputMouse;
    p[5] = op;
    p[6] = button;
    p[7] = pointerKind;
    memcpy(p + 8, &mods, 4);
    memcpy(p + 12, &timeMs, 4);
    memcpy(p + 16, &x, 4);
    memcpy(p + 20, &y, 4);
    memcpy(p + 24, &pressure, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

struct WireTouchPoint {
    int32_t id;
    float x;
    float y;
    float pressure;
};

void sendTouchInput(WinSession *ws, uint8_t op, bool firstNew, uint32_t mods,
                    uint32_t timeMs, const WireTouchPoint *pts, uint32_t count) {
    if (ws == nullptr) {
        return;
    }
    markInputArrival(ws);
    std::vector<uint8_t> p(20 + count * 16, 0);
    memcpy(p.data() + 0, &ws->winId, 4);
    p[4] = kInputTouch;
    p[5] = op;
    p[6] = firstNew ? 1 : 0;
    memcpy(p.data() + 8, &mods, 4);
    memcpy(p.data() + 12, &timeMs, 4);
    memcpy(p.data() + 16, &count, 4);
    for (uint32_t i = 0; i < count; ++i) {
        uint8_t *at = p.data() + 20 + i * 16;
        memcpy(at + 0, &pts[i].id, 4);
        memcpy(at + 4, &pts[i].x, 4);
        memcpy(at + 8, &pts[i].y, 4);
        memcpy(at + 12, &pts[i].pressure, 4);
    }
    sendMessage(kInput, p.data(), static_cast<uint32_t>(p.size()), -1);
}

void sendScrollInput(WinSession *ws, uint8_t flags, uint32_t mods, float x, float y, float dx, float dy) {
    if (ws == nullptr) {
        return;
    }
    markInputArrival(ws);
    uint8_t p[28] = {0};
    memcpy(p + 0, &ws->winId, 4);
    p[4] = kInputScroll;
    p[5] = flags;
    memcpy(p + 8, &mods, 4);
    memcpy(p + 12, &x, 4);
    memcpy(p + 16, &y, 4);
    memcpy(p + 20, &dx, 4);
    memcpy(p + 24, &dy, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendZoomInput(WinSession *ws, uint8_t op, uint32_t mods, float x, float y, float scale) {
    if (ws == nullptr) {
        return;
    }
    markInputArrival(ws);
    uint8_t p[24] = {0};
    memcpy(p + 0, &ws->winId, 4);
    p[4] = kInputZoom;
    p[5] = op;
    memcpy(p + 8, &mods, 4);
    memcpy(p + 12, &x, 4);
    memcpy(p + 16, &y, 4);
    memcpy(p + 20, &scale, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendKeyInput(uint32_t winId, uint8_t op, uint32_t mods, int32_t keyCode, uint32_t unicode) {
    auto ws = findSession(winId);
    markInputArrival(ws.get());
    uint8_t p[20] = {0};
    memcpy(p + 0, &winId, 4);
    p[4] = kInputKey;
    p[5] = op;
    memcpy(p + 8, &mods, 4);
    memcpy(p + 12, &keyCode, 4);
    memcpy(p + 16, &unicode, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendWindowInput(uint32_t winId, uint8_t op) {
    uint8_t p[8] = {0};
    memcpy(p + 0, &winId, 4);
    p[4] = kInputWindow;
    p[5] = op;
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendWindowRectInput(uint32_t winId, int32_t x, int32_t y, int32_t w, int32_t h) {
    uint8_t p[24] = {0};
    memcpy(p + 0, &winId, 4);
    p[4] = kInputWindow;
    p[5] = kWinRect;
    memcpy(p + 8, &x, 4);
    memcpy(p + 12, &y, 4);
    memcpy(p + 16, &w, 4);
    memcpy(p + 20, &h, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendWindowTitleBarInput(uint32_t winId, int32_t height, int32_t left, int32_t right) {
    uint8_t p[20] = {0};
    memcpy(p + 0, &winId, 4);
    p[4] = kInputWindow;
    p[5] = kWinTitleBar;
    memcpy(p + 8, &height, 4);
    memcpy(p + 12, &left, 4);
    memcpy(p + 16, &right, 4);
    sendMessage(kInput, p, sizeof(p), -1);
}

void sendIMEInput(uint32_t winId, uint8_t op, int32_t arg0, int32_t arg1, const std::string &text) {
    auto ws = findSession(winId);
    markInputArrival(ws.get());
    std::vector<uint8_t> p(16 + text.size(), 0);
    memcpy(p.data() + 0, &winId, 4);
    p[4] = kInputIME;
    p[5] = op;
    memcpy(p.data() + 8, &arg0, 4);
    memcpy(p.data() + 12, &arg1, 4);
    if (!text.empty()) {
        memcpy(p.data() + 16, text.data(), text.size());
    }
    sendMessage(kInput, p.data(), static_cast<uint32_t>(p.size()), -1);
}

void sendPopupResult(uint32_t reqId, int32_t chosenId) {
    uint8_t p[8] = {0};
    memcpy(p + 0, &reqId, 4);
    memcpy(p + 4, &chosenId, 4);
    sendMessage(kPopupResult, p, sizeof(p), -1);
}

// ---------- Native Cursor (libohinput.so) ----------

int32_t mapCursorStyle(uint8_t mygoCursor) {
    switch (mygoCursor) {
    case 0:  return DEFAULT;
    case 1:  return HAND_POINTING;
    case 2:  return TEXT_CURSOR;
    case 3:  return MOVE;
    case 4:  return WEST_EAST;
    case 5:  return NORTH_SOUTH;
    case 6:  return NORTH_WEST_SOUTH_EAST;
    case 7:  return NORTH_EAST_SOUTH_WEST;
    case 8:  return CURSOR_FORBID;
    case 9:  return CROSS;
    case 10: return HAND_OPEN;
    case 11: return HAND_GRABBING;
    case 12: return NORTH;
    case 13: return EAST;
    case 14: return SOUTH;
    case 15: return WEST;
    case 16: return RESIZE_LEFT_RIGHT;
    case 17: return RESIZE_UP_DOWN;
    case 18: return HORIZONTAL_TEXT_CURSOR;
    case 19: return CURSOR_COPY;
    case 20: return CURSOR_COPY;
    case 21: return DEFAULT;
    case 23: return RUNNING;
    case 24: return LOADING;
    case 25: return HELP;
    case 26: return CROSS;
    case 27: return ZOOM_IN;
    case 28: return ZOOM_OUT;
    case 29: return MOVE;
    case 30: return CURSOR_FORBID;
    default: return DEFAULT;
    }
}

void applyCursorOnUI(void *userData) {
    uint32_t winId = static_cast<uint32_t>(reinterpret_cast<uintptr_t>(userData));
    auto ws = findSession(winId);
    if (!ws || ws->uiContext == nullptr) {
        return;
    }
    uint8_t c = ws->desiredCursor.load(std::memory_order_relaxed);
    if (c == 22) { // CursorNone
        if (!pointerHidden) {
            OH_Input_SetPointerVisible(false);
            pointerHidden = true;
        }
        return;
    }
    if (pointerHidden) {
        OH_Input_SetPointerVisible(true);
        pointerHidden = false;
    }
    int32_t style = mapCursorStyle(c);
    if (style != ws->appliedPointerStyle && ws->arkWindowId >= 0) {
        Input_Result res = OH_Input_SetPointerStyle(ws->arkWindowId, style);
        if (res == INPUT_SUCCESS) {
            ws->appliedPointerStyle = style;
        }
    }
}

// ---------- Native Custom-Editor IME (libohinputmethod.so) ----------

struct TextInputSnapshot {
    uint32_t winId = 1;
    bool active = false;
    bool noInputMethod = false;
    float caretX = 0;
    float caretY = 0;
    float caretW = 1;
    float caretH = 18;
    int32_t selStart = 0;
    int32_t selEnd = 0;
    std::u16string text;
};

std::mutex imeStateMu;
TextInputSnapshot latestImeState;

// UI-thread-only IME handles.
InputMethod_TextEditorProxy *textEditorProxy = nullptr;
InputMethod_AttachOptions *attachOptions = nullptr;
InputMethod_InputMethodProxy *imeProxy = nullptr;
InputMethod_CursorInfo *imeCursorInfo = nullptr;
bool imeAttached = false;
uint32_t attachedImeWinId = 0;
std::u16string syncedImeText;
int32_t syncedSelStart = -1;
int32_t syncedSelEnd = -1;

uint32_t activeImeWinId() {
    std::lock_guard<std::mutex> lk(imeStateMu);
    return latestImeState.winId > 0 ? latestImeState.winId : 1;
}

void currentScreenCaretRect(const TextInputSnapshot &st, double *left, double *top, double *w, double *h) {
    ArkUI_IntOffset screenOff{0, 0};
    double sc = 1.0;
    auto ws = findSession(st.winId);
    if (ws) {
        if (ws->xcNode != nullptr) {
            OH_ArkUI_NodeUtils_GetLayoutPositionInScreen(ws->xcNode, &screenOff);
        }
        if (ws->scale > 0) {
            sc = ws->scale;
        }
    }
    *left = static_cast<double>(screenOff.x) + static_cast<double>(st.caretX) * sc;
    *top = static_cast<double>(screenOff.y) + static_cast<double>(st.caretY) * sc;
    *w = std::max(1.0, static_cast<double>(st.caretW) * sc);
    *h = std::max(1.0, static_cast<double>(st.caretH) * sc);
}

void ImeGetTextConfig(InputMethod_TextEditorProxy *, InputMethod_TextConfig *config) {
    TextInputSnapshot st;
    {
        std::lock_guard<std::mutex> lk(imeStateMu);
        st = latestImeState;
    }
    OH_TextConfig_SetInputType(config, IME_TEXT_INPUT_TYPE_TEXT);
    OH_TextConfig_SetEnterKeyType(config, IME_ENTER_KEY_DONE);
    OH_TextConfig_SetPreviewTextSupport(config, true);
    auto ws = findSession(st.winId);
    if (ws && ws->arkWindowId > 0) {
        OH_TextConfig_SetWindowId(config, ws->arkWindowId);
    }
    OH_TextConfig_SetSelection(config, st.selStart, st.selEnd);
    InputMethod_CursorInfo *ci = nullptr;
    if (OH_TextConfig_GetCursorInfo(config, &ci) == IME_ERR_OK && ci != nullptr) {
        double l = 0, t = 0, w = 2, h = 20;
        currentScreenCaretRect(st, &l, &t, &w, &h);
        OH_CursorInfo_SetRect(ci, l, t, w, h);
    }
}

void ImeInsertText(InputMethod_TextEditorProxy *, const char16_t *text, size_t length) {
    std::string utf8 = utf16ToUtf8(text, length);
    if (!utf8.empty()) {
        sendIMEInput(activeImeWinId(), kImeCommit, 0, 0, utf8);
    }
}

int32_t ImeSetPreviewText(InputMethod_TextEditorProxy *, const char16_t text[], size_t length,
                          int32_t start, int32_t end) {
    std::string utf8 = utf16ToUtf8(text, length);
    sendIMEInput(activeImeWinId(), kImePreedit, start, end, utf8);
    return 0;
}

void ImeFinishTextPreview(InputMethod_TextEditorProxy *) {
    sendIMEInput(activeImeWinId(), kImePreeditEnd, 0, 0, "");
}

void ImeDeleteBackward(InputMethod_TextEditorProxy *, int32_t length) {
    if (length > 0) {
        sendIMEInput(activeImeWinId(), kImeDeleteSurrounding, -length, length, "");
    }
}

void ImeDeleteForward(InputMethod_TextEditorProxy *, int32_t length) {
    if (length > 0) {
        sendIMEInput(activeImeWinId(), kImeDeleteSurrounding, 0, length, "");
    }
}

void ImeSendKeyboardStatus(InputMethod_TextEditorProxy *, InputMethod_KeyboardStatus) {}

void ImeSendEnterKey(InputMethod_TextEditorProxy *, InputMethod_EnterKeyType) {
    uint32_t wid = activeImeWinId();
    sendKeyInput(wid, kKeyDown, 0, ARKUI_KEYCODE_ENTER, '\r');
    sendKeyInput(wid, kKeyUp, 0, ARKUI_KEYCODE_ENTER, '\r');
}

void ImeMoveCursor(InputMethod_TextEditorProxy *, InputMethod_Direction direction) {
    int32_t code = 0;
    switch (direction) {
    case IME_DIRECTION_UP:    code = ARKUI_KEYCODE_DPAD_UP; break;
    case IME_DIRECTION_DOWN:  code = ARKUI_KEYCODE_DPAD_DOWN; break;
    case IME_DIRECTION_LEFT:  code = ARKUI_KEYCODE_DPAD_LEFT; break;
    case IME_DIRECTION_RIGHT: code = ARKUI_KEYCODE_DPAD_RIGHT; break;
    default: return;
    }
    uint32_t wid = activeImeWinId();
    sendKeyInput(wid, kKeyDown, 0, code, 0);
    sendKeyInput(wid, kKeyUp, 0, code, 0);
}

void ImeHandleSetSelection(InputMethod_TextEditorProxy *, int32_t start, int32_t end) {
    sendIMEInput(activeImeWinId(), kImeSetSelection, start, end, "");
}

void ImeHandleExtendAction(InputMethod_TextEditorProxy *, InputMethod_ExtendAction action) {
    const char *cmd = nullptr;
    switch (action) {
    case IME_EXTEND_ACTION_SELECT_ALL: cmd = "selectAll"; break;
    case IME_EXTEND_ACTION_CUT:        cmd = "cut"; break;
    case IME_EXTEND_ACTION_COPY:       cmd = "copy"; break;
    case IME_EXTEND_ACTION_PASTE:      cmd = "paste"; break;
    default: return;
    }
    sendIMEInput(activeImeWinId(), kImeCommand, 0, 0, cmd);
}

void ImeGetLeftTextOfCursor(InputMethod_TextEditorProxy *, int32_t number, char16_t text[], size_t *length) {
    if (length == nullptr) {
        return;
    }
    if (number <= 0 || text == nullptr) {
        *length = 0;
        return;
    }
    std::lock_guard<std::mutex> lk(imeStateMu);
    const std::u16string &buf = latestImeState.text;
    size_t caret = static_cast<size_t>(std::max<int32_t>(0, std::min<int32_t>(latestImeState.selEnd, static_cast<int32_t>(buf.size()))));
    size_t want = static_cast<size_t>(number);
    size_t take = std::min(want, caret);
    if (take > 0) {
        memcpy(text, buf.data() + (caret - take), take * sizeof(char16_t));
    }
    *length = take;
}

void ImeGetRightTextOfCursor(InputMethod_TextEditorProxy *, int32_t number, char16_t text[], size_t *length) {
    if (length == nullptr) {
        return;
    }
    if (number <= 0 || text == nullptr) {
        *length = 0;
        return;
    }
    std::lock_guard<std::mutex> lk(imeStateMu);
    const std::u16string &buf = latestImeState.text;
    size_t caret = static_cast<size_t>(std::max<int32_t>(0, std::min<int32_t>(latestImeState.selEnd, static_cast<int32_t>(buf.size()))));
    size_t avail = buf.size() > caret ? buf.size() - caret : 0;
    size_t take = std::min(static_cast<size_t>(number), avail);
    if (take > 0) {
        memcpy(text, buf.data() + caret, take * sizeof(char16_t));
    }
    *length = take;
}

int32_t ImeGetTextIndexAtCursor(InputMethod_TextEditorProxy *) {
    std::lock_guard<std::mutex> lk(imeStateMu);
    return std::max<int32_t>(0, latestImeState.selEnd);
}

int32_t ImeReceivePrivateCommand(InputMethod_TextEditorProxy *, InputMethod_PrivateCommand *[], size_t) {
    return 0;
}

void ensureTextEditorProxy() {
    if (textEditorProxy != nullptr) {
        return;
    }
    textEditorProxy = OH_TextEditorProxy_Create();
    if (textEditorProxy == nullptr) {
        OH_LOG_ERROR(LOG_APP, "OH_TextEditorProxy_Create failed");
        return;
    }
    OH_TextEditorProxy_SetCallbackInMainThread(textEditorProxy, true);
    OH_TextEditorProxy_SetGetTextConfigFunc(textEditorProxy, ImeGetTextConfig);
    OH_TextEditorProxy_SetInsertTextFunc(textEditorProxy, ImeInsertText);
    OH_TextEditorProxy_SetDeleteForwardFunc(textEditorProxy, ImeDeleteForward);
    OH_TextEditorProxy_SetDeleteBackwardFunc(textEditorProxy, ImeDeleteBackward);
    OH_TextEditorProxy_SetSendKeyboardStatusFunc(textEditorProxy, ImeSendKeyboardStatus);
    OH_TextEditorProxy_SetSendEnterKeyFunc(textEditorProxy, ImeSendEnterKey);
    OH_TextEditorProxy_SetMoveCursorFunc(textEditorProxy, ImeMoveCursor);
    OH_TextEditorProxy_SetHandleSetSelectionFunc(textEditorProxy, ImeHandleSetSelection);
    OH_TextEditorProxy_SetHandleExtendActionFunc(textEditorProxy, ImeHandleExtendAction);
    OH_TextEditorProxy_SetGetLeftTextOfCursorFunc(textEditorProxy, ImeGetLeftTextOfCursor);
    OH_TextEditorProxy_SetGetRightTextOfCursorFunc(textEditorProxy, ImeGetRightTextOfCursor);
    OH_TextEditorProxy_SetGetTextIndexAtCursorFunc(textEditorProxy, ImeGetTextIndexAtCursor);
    OH_TextEditorProxy_SetReceivePrivateCommandFunc(textEditorProxy, ImeReceivePrivateCommand);
    OH_TextEditorProxy_SetSetPreviewTextFunc(textEditorProxy, ImeSetPreviewText);
    OH_TextEditorProxy_SetFinishTextPreviewFunc(textEditorProxy, ImeFinishTextPreview);
}

void detachIME() {
    if (imeAttached && imeProxy != nullptr) {
        OH_InputMethodProxy_HideKeyboard(imeProxy);
        OH_InputMethodController_Detach(imeProxy);
    }
    imeProxy = nullptr;
    imeAttached = false;
    attachedImeWinId = 0;
    if (attachOptions != nullptr) {
        OH_AttachOptions_Destroy(attachOptions);
        attachOptions = nullptr;
    }
    syncedImeText.clear();
    syncedSelStart = -1;
    syncedSelEnd = -1;
}

void destroyIME() {
    detachIME();
    if (imeCursorInfo != nullptr) {
        OH_CursorInfo_Destroy(imeCursorInfo);
        imeCursorInfo = nullptr;
    }
    if (textEditorProxy != nullptr) {
        OH_TextEditorProxy_Destroy(textEditorProxy);
        textEditorProxy = nullptr;
    }
}

void syncIMEOnUI(void *) {
    TextInputSnapshot st;
    {
        std::lock_guard<std::mutex> lk(imeStateMu);
        st = latestImeState;
    }
    auto ws = findSession(st.winId);
    if (!ws || ws->uiContext == nullptr) {
        if (imeAttached) {
            detachIME();
        }
        return;
    }
    if (st.active && !st.noInputMethod && !ws->nodeFocused && ws->xcNode != nullptr) {
        OH_ArkUI_FocusRequest(ws->xcNode);
    }
    const bool wantAttach = st.active && !st.noInputMethod;
    if (!wantAttach) {
        if (imeAttached) {
            detachIME();
        }
        return;
    }
    if (imeAttached && attachedImeWinId != st.winId) {
        detachIME();
    }

    ensureTextEditorProxy();
    if (textEditorProxy == nullptr) {
        return;
    }

    double left = 0, top = 0, w = 2, h = 20;
    currentScreenCaretRect(st, &left, &top, &w, &h);

    if (!imeAttached) {
        if (attachOptions != nullptr) {
            OH_AttachOptions_Destroy(attachOptions);
        }
        attachOptions = OH_AttachOptions_Create(true);
        if (attachOptions == nullptr) {
            return;
        }
        InputMethod_ErrorCode ret = OH_InputMethodController_AttachWithUIContext(
            ws->uiContext, textEditorProxy, attachOptions, &imeProxy);
        if (ret != IME_ERR_OK || imeProxy == nullptr) {
            ret = OH_InputMethodController_Attach(textEditorProxy, attachOptions, &imeProxy);
        }
        if (ret != IME_ERR_OK || imeProxy == nullptr) {
            OH_AttachOptions_Destroy(attachOptions);
            attachOptions = nullptr;
            imeProxy = nullptr;
            return;
        }
        imeAttached = true;
        attachedImeWinId = st.winId;
        syncedSelStart = -1;
        syncedSelEnd = -1;
    }

    if (imeCursorInfo == nullptr) {
        imeCursorInfo = OH_CursorInfo_Create(left, top, w, h);
    } else {
        OH_CursorInfo_SetRect(imeCursorInfo, left, top, w, h);
    }
    if (imeCursorInfo != nullptr) {
        OH_InputMethodProxy_NotifyCursorUpdate(imeProxy, imeCursorInfo);
    }

    if (st.text != syncedImeText || st.selStart != syncedSelStart || st.selEnd != syncedSelEnd) {
        char16_t emptyZero = 0;
        char16_t *ptr = st.text.empty() ? &emptyZero : const_cast<char16_t *>(st.text.data());
        OH_InputMethodProxy_NotifySelectionChange(imeProxy, ptr, st.text.size(), st.selStart, st.selEnd);
        syncedImeText = st.text;
        syncedSelStart = st.selStart;
        syncedSelEnd = st.selEnd;
    }
}

void handleEngineTextInput(const std::vector<uint8_t> &p) {
    if (p.size() < 36) {
        return;
    }
    uint32_t winId = 0;
    uint32_t textLen = 0;
    memcpy(&winId, p.data() + 0, 4);
    memcpy(&textLen, p.data() + 32, 4);
    if (36 + static_cast<size_t>(textLen) > p.size()) {
        return;
    }
    TextInputSnapshot st;
    st.winId = winId > 0 ? winId : 1;
    st.active = (p[4] & 1) != 0;
    st.noInputMethod = (p[4] & 2) != 0;
    memcpy(&st.caretX, p.data() + 8, 4);
    memcpy(&st.caretY, p.data() + 12, 4);
    memcpy(&st.caretW, p.data() + 16, 4);
    memcpy(&st.caretH, p.data() + 20, 4);
    memcpy(&st.selStart, p.data() + 24, 4);
    memcpy(&st.selEnd, p.data() + 28, 4);
    if (textLen > 0) {
        std::string utf8(reinterpret_cast<const char *>(p.data() + 36), textLen);
        st.text = utf8ToUtf16(utf8);
    }
    auto ws = getOrCreateSession(st.winId);
    {
        std::lock_guard<std::mutex> lk(imeStateMu);
        if (st.active || latestImeState.winId == st.winId) {
            latestImeState = std::move(st);
        }
    }
    if (ws && ws->uiContext != nullptr) {
        OH_ArkUI_PostUITask(ws->uiContext, nullptr, syncIMEOnUI);
    }
}

// ---------- NDK Input Event Extraction ----------

uint32_t extractMods(const WinSession *ws, const ArkUI_UIInputEvent *ev) {
    uint64_t keys = 0;
    uint32_t mods = 0;
    if (ev != nullptr && OH_ArkUI_UIInputEvent_GetModifierKeyStates(ev, &keys) == ARKUI_ERROR_CODE_NO_ERROR) {
        if (keys & ARKUI_MODIFIER_KEY_SHIFT) {
            mods |= kModShift;
        }
        if (keys & ARKUI_MODIFIER_KEY_CTRL) {
            mods |= kModCtrl;
        }
        if (keys & ARKUI_MODIFIER_KEY_ALT) {
            mods |= kModAlt;
        }
    }
    if (ws != nullptr && (ws->metaLeftDown || ws->metaRightDown)) {
        mods |= kModSuper;
    }
    return mods;
}

uint32_t eventMs(const ArkUI_UIInputEvent *ev) {
    if (ev == nullptr) {
        return static_cast<uint32_t>(monoNs() / 1000000LL);
    }
    int64_t ns = OH_ArkUI_UIInputEvent_GetEventTime(ev);
    if (ns <= 0) {
        return static_cast<uint32_t>(monoNs() / 1000000LL);
    }
    return static_cast<uint32_t>(ns / 1000000LL);
}

float toDip(float vp) {
    return vp;
}

uint8_t mapMouseButton(int32_t arkBtn) {
    switch (arkBtn) {
    case UI_MOUSE_EVENT_BUTTON_LEFT:    return 0;
    case UI_MOUSE_EVENT_BUTTON_RIGHT:   return 1;
    case UI_MOUSE_EVENT_BUTTON_MIDDLE:  return 2;
    case UI_MOUSE_EVENT_BUTTON_BACK:    return 3;
    case UI_MOUSE_EVENT_BUTTON_FORWARD: return 4;
    default:                            return 255;
    }
}

void OnNodeEvent(ArkUI_NodeEvent *event) {
    if (event == nullptr) {
        return;
    }
    ArkUI_NodeHandle node = OH_ArkUI_NodeEvent_GetNodeHandle(event);
    auto it = gNodeToWinId.find(node);
    if (it == gNodeToWinId.end()) {
        return;
    }
    auto ws = findSession(it->second);
    if (!ws) {
        return;
    }

    ArkUI_NodeEventType type = OH_ArkUI_NodeEvent_GetEventType(event);
    switch (type) {
    case NODE_ON_FOCUS:
        ws->nodeFocused = true;
        sendWindowInput(ws->winId, kWinFocus);
        syncIMEOnUI(nullptr);
        return;
    case NODE_ON_BLUR:
        ws->nodeFocused = false;
        ws->metaLeftDown = false;
        ws->metaRightDown = false;
        if (attachedImeWinId == ws->winId) {
            detachIME();
        }
        sendWindowInput(ws->winId, kWinBlur);
        return;
    case NODE_ON_HOVER_EVENT: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev != nullptr && !OH_ArkUI_HoverEvent_IsHovered(ev)) {
            sendMouseInput(ws.get(), kMouseLeave, 255, 0, extractMods(ws.get(), ev), eventMs(ev), 0, 0, 0);
        }
        return;
    }
    case NODE_ON_MOUSE: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev == nullptr) {
            return;
        }
        int32_t act = OH_ArkUI_MouseEvent_GetMouseAction(ev);
        uint8_t btn = mapMouseButton(OH_ArkUI_MouseEvent_GetMouseButton(ev));
        float x = toDip(OH_ArkUI_PointerEvent_GetX(ev));
        float y = toDip(OH_ArkUI_PointerEvent_GetY(ev));
        ws->lastPointerX = x;
        ws->lastPointerY = y;
        uint32_t mods = extractMods(ws.get(), ev);
        uint32_t ms = eventMs(ev);
        switch (act) {
        case UI_MOUSE_EVENT_ACTION_PRESS:
            if (!ws->nodeFocused && ws->xcNode != nullptr) {
                OH_ArkUI_FocusRequest(ws->xcNode);
            }
            sendMouseInput(ws.get(), kMouseDown, btn, 0, mods, ms, x, y, 0);
            break;
        case UI_MOUSE_EVENT_ACTION_RELEASE:
            sendMouseInput(ws.get(), kMouseUp, btn, 0, mods, ms, x, y, 0);
            break;
        case UI_MOUSE_EVENT_ACTION_MOVE:
            sendMouseInput(ws.get(), kMouseMove, btn, 0, mods, ms, x, y, 0);
            break;
        case UI_MOUSE_EVENT_ACTION_CANCEL:
            sendMouseInput(ws.get(), kMouseCancel, btn, 0, mods, ms, x, y, 0);
            break;
        default:
            break;
        }
        return;
    }
    case NODE_TOUCH_EVENT: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev == nullptr) {
            return;
        }
        int32_t tool = OH_ArkUI_UIInputEvent_GetToolType(ev);
        int32_t act = OH_ArkUI_UIInputEvent_GetAction(ev);
        uint32_t mods = extractMods(ws.get(), ev);
        uint32_t ms = eventMs(ev);

        if (act == UI_TOUCH_EVENT_ACTION_DOWN && !ws->nodeFocused && ws->xcNode != nullptr) {
            OH_ArkUI_FocusRequest(ws->xcNode);
        }
        if (tool == UI_INPUT_EVENT_TOOL_TYPE_PEN) {
            float x = toDip(OH_ArkUI_PointerEvent_GetX(ev));
            float y = toDip(OH_ArkUI_PointerEvent_GetY(ev));
            float pr = OH_ArkUI_PointerEvent_GetPressure(ev, 0);
            switch (act) {
            case UI_TOUCH_EVENT_ACTION_DOWN:
                sendMouseInput(ws.get(), kMouseDown, 0, 1, mods, ms, x, y, pr);
                break;
            case UI_TOUCH_EVENT_ACTION_MOVE:
                sendMouseInput(ws.get(), kMouseMove, 255, 1, mods, ms, x, y, pr);
                break;
            case UI_TOUCH_EVENT_ACTION_UP:
                sendMouseInput(ws.get(), kMouseUp, 0, 1, mods, ms, x, y, pr);
                break;
            case UI_TOUCH_EVENT_ACTION_CANCEL:
                sendMouseInput(ws.get(), kMouseCancel, 0, 1, mods, ms, x, y, pr);
                break;
            default:
                break;
            }
            return;
        }

        uint32_t count = OH_ArkUI_PointerEvent_GetPointerCount(ev);
        if (count == 0) {
            return;
        }
        if (act == UI_TOUCH_EVENT_ACTION_DOWN || act == UI_TOUCH_EVENT_ACTION_UP) {
            uint32_t idx = 0;
            OH_ArkUI_PointerEvent_GetChangedPointerId(ev, &idx);
            if (idx >= count) {
                idx = 0;
            }
            WireTouchPoint pt{
                OH_ArkUI_PointerEvent_GetPointerId(ev, idx),
                toDip(OH_ArkUI_PointerEvent_GetXByIndex(ev, idx)),
                toDip(OH_ArkUI_PointerEvent_GetYByIndex(ev, idx)),
                OH_ArkUI_PointerEvent_GetPressure(ev, idx),
            };
            uint8_t op = (act == UI_TOUCH_EVENT_ACTION_DOWN) ? kTouchDown : kTouchUp;
            bool firstNew = (act == UI_TOUCH_EVENT_ACTION_DOWN && count <= 1);
            sendTouchInput(ws.get(), op, firstNew, mods, ms, &pt, 1);
            return;
        }

        std::vector<WireTouchPoint> pts(count);
        for (uint32_t i = 0; i < count; ++i) {
            pts[i] = WireTouchPoint{
                OH_ArkUI_PointerEvent_GetPointerId(ev, i),
                toDip(OH_ArkUI_PointerEvent_GetXByIndex(ev, i)),
                toDip(OH_ArkUI_PointerEvent_GetYByIndex(ev, i)),
                OH_ArkUI_PointerEvent_GetPressure(ev, i),
            };
        }
        uint8_t op = (act == UI_TOUCH_EVENT_ACTION_MOVE) ? kTouchMove : kTouchCancel;
        sendTouchInput(ws.get(), op, false, mods, ms, pts.data(), count);
        return;
    }
    case NODE_ON_AXIS: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev == nullptr) {
            return;
        }
        int32_t act = OH_ArkUI_AxisEvent_GetAxisAction(ev);
        uint32_t mods = extractMods(ws.get(), ev);
        float x = toDip(OH_ArkUI_PointerEvent_GetX(ev));
        float y = toDip(OH_ArkUI_PointerEvent_GetY(ev));
        ws->lastPointerX = x;
        ws->lastPointerY = y;

        if (OH_ArkUI_AxisEvent_HasAxis(ev, UI_AXIS_TYPE_PINCH_AXIS)) {
            float sc = static_cast<float>(OH_ArkUI_AxisEvent_GetPinchAxisScaleValue(ev));
            uint8_t op = kZoomUpdate;
            if (act == UI_AXIS_EVENT_ACTION_BEGIN) {
                op = kZoomBegin;
            } else if (act == UI_AXIS_EVENT_ACTION_END || act == UI_AXIS_EVENT_ACTION_CANCEL) {
                op = kZoomEnd;
            }
            sendZoomInput(ws.get(), op, mods, x, y, sc);
            return;
        }

        if (act != UI_AXIS_EVENT_ACTION_BEGIN && act != UI_AXIS_EVENT_ACTION_UPDATE) {
            return;
        }
        double vert = OH_ArkUI_AxisEvent_GetVerticalAxisValue(ev);
        double horiz = OH_ArkUI_AxisEvent_GetHorizontalAxisValue(ev);
        int32_t tool = OH_ArkUI_UIInputEvent_GetToolType(ev);
        int32_t step = OH_ArkUI_AxisEvent_GetScrollStep(ev);
        bool isTouchpad = (tool == UI_INPUT_EVENT_TOOL_TYPE_TOUCHPAD) ||
                          (tool == UI_INPUT_EVENT_TOOL_TYPE_FINGER) ||
                          (tool != UI_INPUT_EVENT_TOOL_TYPE_MOUSE && step == 0);
        float dx = 0, dy = 0;
        uint8_t flags = 0;
        if (isTouchpad) {
            const double sc = ws->scale > 0 ? ws->scale : 1.0;
            dx = static_cast<float>(horiz / sc);
            dy = static_cast<float>(vert / sc);
            flags = kScrollPrecise;
        } else {
            constexpr double kDipPerDeg = 50.0 / 45.0;
            dx = static_cast<float>(horiz * kDipPerDeg);
            dy = static_cast<float>(vert * kDipPerDeg);
            flags = 0;
        }
        if (dx != 0 || dy != 0) {
            sendScrollInput(ws.get(), flags, mods, x, y, dx, dy);
        }
        return;
    }
    case NODE_ON_COASTING_AXIS_EVENT: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev == nullptr) {
            return;
        }
        ArkUI_CoastingAxisEvent *c = OH_ArkUI_UIInputEvent_GetCoastingAxisEvent(const_cast<ArkUI_UIInputEvent *>(ev));
        if (c == nullptr) {
            return;
        }
        ArkUI_CoastingAxisEventPhase phase = OH_ArkUI_CoastingAxisEvent_GetPhase(c);
        if (phase != ARKUI_COASTING_AXIS_EVENT_PHASE_BEGIN &&
            phase != ARKUI_COASTING_AXIS_EVENT_PHASE_UPDATE) {
            return;
        }
        const double sc = ws->scale > 0 ? ws->scale : 1.0;
        float dx = static_cast<float>(static_cast<double>(OH_ArkUI_CoastingAxisEvent_GetDeltaX(c)) / sc);
        float dy = static_cast<float>(static_cast<double>(OH_ArkUI_CoastingAxisEvent_GetDeltaY(c)) / sc);
        if (dx != 0 || dy != 0) {
            sendScrollInput(ws.get(), kScrollPrecise | kScrollMomentum,
                            extractMods(ws.get(), ev), ws->lastPointerX, ws->lastPointerY, dx, dy);
        }
        return;
    }
    case NODE_ON_KEY_EVENT: {
        const ArkUI_UIInputEvent *ev = OH_ArkUI_NodeEvent_GetInputEvent(event);
        if (ev == nullptr) {
            return;
        }
        ArkUI_KeyEventType kt = OH_ArkUI_KeyEvent_GetType(ev);
        if (kt != ARKUI_KEY_EVENT_DOWN && kt != ARKUI_KEY_EVENT_UP) {
            return;
        }
        int32_t code = OH_ArkUI_KeyEvent_GetKeyCode(ev);
        if (code == ARKUI_KEYCODE_META_LEFT) {
            ws->metaLeftDown = (kt == ARKUI_KEY_EVENT_DOWN);
        } else if (code == ARKUI_KEYCODE_META_RIGHT) {
            ws->metaRightDown = (kt == ARKUI_KEY_EVENT_DOWN);
        }
        uint32_t unicode = OH_ArkUI_KeyEvent_GetUnicode(ev);
        if (code == ARKUI_KEYCODE_SPACE && unicode == 0) {
            unicode = 0x20;
        }
        if (code >= 100 || code == 2) {
            OH_ArkUI_KeyEvent_SetConsumed(ev, true);
        }
        sendKeyInput(ws->winId, kt == ARKUI_KEY_EVENT_UP ? kKeyUp : kKeyDown,
                     extractMods(ws.get(), ev), code, unicode);
        return;
    }
    default:
        return;
    }
}

// ---------- Per-Window VSync & Swapchain Render Thread ----------

void OnVsyncCallback(long long, void *data) {
    auto *ws = static_cast<WinSession *>(data);
    if (ws != nullptr && ws->vsyncInited) {
        sem_post(&ws->vsyncSem);
    }
}

void WinSession::applyExpectedFrameRateLocked(uint32_t fps) {
    if (vsync == nullptr) {
        return;
    }
    int32_t maxHz = std::max<int32_t>(60, static_cast<int32_t>(std::lround(hz)));
    OH_NativeVSync_ExpectedRateRange range;
    if (fps == 0) {
        range = {0, maxHz, maxHz};
    } else {
        int32_t target = std::clamp<int32_t>(static_cast<int32_t>(fps), 1, maxHz);
        range = {target, maxHz, target};
    }
    OH_NativeVSync_SetExpectedFrameRateRange(vsync, &range);
}

void WinSession::waitVsync() {
    if (vsync == nullptr || !vsyncInited) {
        return;
    }
    OH_NativeVSync_RequestFrame(vsync, OnVsyncCallback, this);
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_nsec += 100 * 1000 * 1000;
    if (ts.tv_nsec >= 1000000000) {
        ts.tv_sec += 1;
        ts.tv_nsec -= 1000000000;
    }
    (void)sem_timedwait(&vsyncSem, &ts);
}

void logLatencyStats(uint32_t winId, const char *reason, const LatencyRing &drawRing,
                     const LatencyRing &inRing, const LatencyRing &intervalRing) {
    if (drawRing.size() == 0) {
        return;
    }
    double dp50, dp90, dp99, dmax;
    double ip50, ip90, ip99, imax;
    double fp50, fp90, fp99, fmax;
    drawRing.summary(&dp50, &dp90, &dp99, &dmax);
    inRing.summary(&ip50, &ip90, &ip99, &imax);
    intervalRing.summary(&fp50, &fp90, &fp99, &fmax);
    OH_LOG_INFO(LOG_APP,
                "win=%{public}u latency[%{public}s] draw(n=%{public}llu p50=%{public}.2fms p90=%{public}.2fms p99=%{public}.2fms max=%{public}.2fms) "
                "in2flush(n=%{public}llu p50=%{public}.2fms p90=%{public}.2fms p99=%{public}.2fms max=%{public}.2fms) "
                "interval(n=%{public}llu p50=%{public}.2fms p90=%{public}.2fms p99=%{public}.2fms max=%{public}.2fms)",
                winId, reason,
                static_cast<unsigned long long>(drawRing.totalCount()), dp50, dp90, dp99, dmax,
                static_cast<unsigned long long>(inRing.totalCount()), ip50, ip90, ip99, imax,
                static_cast<unsigned long long>(intervalRing.totalCount()), fp50, fp90, fp99, fmax);
}

void WinSession::renderLoop() {
    OHNativeWindow *win = nullptr;
    int32_t w = 0, h = 0;
    double sc = 1.0, refresh = 120.0;
    {
        std::lock_guard<std::mutex> lk(mu);
        win = nativeWindow;
        w = targetW;
        h = targetH;
        curW = w;
        curH = h;
        sc = scale;
        refresh = hz;
    }
    if (win == nullptr || w <= 0 || h <= 0) {
        return;
    }

    int32_t rc = OH_NativeWindow_NativeWindowHandleOpt(win, SET_BUFFER_GEOMETRY, w, h);
    uint64_t usage = 0;
    OH_NativeWindow_NativeWindowHandleOpt(win, GET_USAGE, &usage);
    usage &= ~static_cast<uint64_t>(NATIVEBUFFER_USAGE_CPU_READ_OFTEN);
    usage |= NATIVEBUFFER_USAGE_CPU_READ | NATIVEBUFFER_USAGE_CPU_WRITE | NATIVEBUFFER_USAGE_MEM_DMA;
    OH_NativeWindow_NativeWindowHandleOpt(win, SET_USAGE, usage);
    OH_LOG_INFO(LOG_APP, "win=%{public}u surface %{public}dx%{public}d rc=%{public}d scale=%{public}.2f hz=%{public}.0f",
                winId, w, h, rc, sc, refresh);
    sendSurface(winId, w, h, sc, refresh);

    uint32_t seq = 0;
    int frames = 0, damaged = 0, whole = 0, aborted = 0;
    int unloggedFrames = 0;
    int64_t lastFlushNs = 0;
    LatencyRing drawRing, inRing, intervalRing;

    for (;;) {
        {
            std::unique_lock<std::mutex> lk(mu);
            bool woke = cv.wait_for(lk, std::chrono::milliseconds(250), [&] {
                return stopRender || forceResendSurface || targetW != curW || targetH != curH || fpsDirty || wantFrame;
            });
            if (stopRender) {
                break;
            }
            if (!woke) {
                if (unloggedFrames > 0) {
                    logLatencyStats(winId, "settle", drawRing, inRing, intervalRing);
                    unloggedFrames = 0;
                }
                continue;
            }
            if (fpsDirty) {
                fpsDirty = false;
                applyExpectedFrameRateLocked(requestedFps);
            }
            if (targetW > 0 && targetH > 0 && (targetW != curW || targetH != curH)) {
                curW = targetW;
                curH = targetH;
                w = curW;
                h = curH;
                sc = scale;
                refresh = hz;
                lk.unlock();
                rc = OH_NativeWindow_NativeWindowHandleOpt(win, SET_BUFFER_GEOMETRY, w, h);
                OH_LOG_INFO(LOG_APP, "win=%{public}u resize %{public}dx%{public}d rc=%{public}d", winId, w, h, rc);
                sendSurface(winId, w, h, sc, refresh);
                lk.lock();
                forceResendSurface = false;
                if (stopRender) {
                    break;
                }
            } else if (forceResendSurface) {
                forceResendSurface = false;
                sc = scale;
                refresh = hz;
                lk.unlock();
                sendSurface(winId, w, h, sc, refresh);
                lk.lock();
                if (stopRender) {
                    break;
                }
            }
            if (!wantFrame) {
                continue;
            }
            wantFrame = false;
        }

        waitVsync();

        {
            std::unique_lock<std::mutex> lk(mu);
            if (stopRender) {
                break;
            }
            if (targetW > 0 && targetH > 0 && (targetW != curW || targetH != curH)) {
                curW = targetW;
                curH = targetH;
                w = curW;
                h = curH;
                sc = scale;
                refresh = hz;
                lk.unlock();
                OH_NativeWindow_NativeWindowHandleOpt(win, SET_BUFFER_GEOMETRY, w, h);
                sendSurface(winId, w, h, sc, refresh);
                lk.lock();
                if (stopRender) {
                    break;
                }
            }
            reply = BufReply::None;
        }

        OHNativeWindowBuffer *buf = nullptr;
        int fence = -1;
        rc = OH_NativeWindow_NativeWindowRequestBuffer(win, &buf, &fence);
        if (rc != 0 || buf == nullptr) {
            OH_LOG_ERROR(LOG_APP, "win=%{public}u RequestBuffer failed rc=%{public}d", winId, rc);
            usleep(16000);
            continue;
        }
        if (fence >= 0) {
            struct pollfd pfd = {};
            pfd.fd = fence;
            pfd.events = POLLIN;
            int prc = -1;
            do {
                prc = poll(&pfd, 1, 3000);
            } while (prc == -1 && (errno == EINTR || errno == EAGAIN));
            close(fence);
        }
        BufferHandle *bh = OH_NativeWindow_GetBufferHandleFromNative(buf);
        if (bh == nullptr || bh->fd < 0) {
            OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
            continue;
        }
        struct dma_buf_sync syncStart = {DMA_BUF_SYNC_START | DMA_BUF_SYNC_WRITE};
        (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncStart);

        uint32_t bufId = 0;
        OH_NativeBuffer *nb = nullptr;
        if (OH_NativeBuffer_FromNativeWindowBuffer(buf, &nb) == 0 && nb != nullptr) {
            bufId = OH_NativeBuffer_GetSeqNum(nb);
        }
        if (bufId == 0) {
            uintptr_t ptr = reinterpret_cast<uintptr_t>(buf);
            bufId = static_cast<uint32_t>(ptr ^ (ptr >> 32)) | 1u;
        }

        const uint32_t curSeq = seq++;
        const int64_t sendFrameNs = monoNs();
        if (!sendFrame(winId, curSeq, bufId, bh->width, bh->height, bh->stride, bh->size, bh->fd)) {
            struct dma_buf_sync syncEnd = {DMA_BUF_SYNC_END | DMA_BUF_SYNC_WRITE};
            (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncEnd);
            OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
            continue;
        }

        BufReply gotReply = BufReply::None;
        uint32_t rectCount = 0;
        bool timedOut = false;
        {
            std::unique_lock<std::mutex> lk(mu);
            const int waitMs = (frames == 0) ? 15000 : 5000;
            bool ok = cv.wait_for(lk, std::chrono::milliseconds(waitMs), [&] {
                return stopRender || reply != BufReply::None;
            });
            if (!ok) {
                timedOut = true;
            } else {
                gotReply = reply;
                rectCount = replyRectCount;
                reply = BufReply::None;
            }
        }

        struct dma_buf_sync syncEnd = {DMA_BUF_SYNC_END | DMA_BUF_SYNC_WRITE};
        (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncEnd);

        if (timedOut) {
            OH_LOG_ERROR(LOG_APP, "win=%{public}u frame %{public}u timed out", winId, curSeq);
            OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
            gFrameTimeoutRestart.store(true);
            int fd = -1;
            {
                std::lock_guard<std::mutex> lk(sendMu);
                fd = sock;
            }
            if (fd >= 0) {
                shutdown(fd, SHUT_RDWR);
            }
            continue;
        }

        if (gotReply != BufReply::Present) {
            OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
            if (gotReply == BufReply::Abort) {
                aborted++;
            }
            continue;
        }

        Region region = {nullptr, 0};
        rc = OH_NativeWindow_NativeWindowFlushBuffer(win, buf, -1, region);
        if (rc != 0) {
            OH_LOG_ERROR(LOG_APP, "win=%{public}u FlushBuffer failed rc=%{public}d", winId, rc);
            continue;
        }

        int64_t nowNs = monoNs();
        if (frames > 0 && sendFrameNs > 0) {
            drawRing.add(static_cast<double>(nowNs - sendFrameNs) / 1e6);
        }
        if (inputSincePresent.exchange(false, std::memory_order_relaxed)) {
            int64_t inNs = lastInputNs.load(std::memory_order_relaxed);
            if (inNs > 0 && nowNs >= inNs) {
                inRing.add(static_cast<double>(nowNs - inNs) / 1e6);
            }
        }
        if (lastFlushNs > 0) {
            double ivMs = static_cast<double>(nowNs - lastFlushNs) / 1e6;
            if (ivMs > 0.1 && ivMs < 50.0) {
                intervalRing.add(ivMs);
            }
        }
        lastFlushNs = nowNs;

        if (rectCount == 0) {
            whole++;
        } else {
            damaged++;
        }
        frames++;
        unloggedFrames++;
        if (frames <= 3 || frames % 60 == 0) {
            OH_LOG_INFO(LOG_APP, "win=%{public}u frames=%{public}d damaged=%{public}d whole=%{public}d aborted=%{public}d",
                        winId, frames, damaged, whole, aborted);
        }
        if (frames % 60 == 0) {
            logLatencyStats(winId, "periodic", drawRing, inRing, intervalRing);
            unloggedFrames = 0;
        }
    }
    logLatencyStats(winId, "stop", drawRing, inRing, intervalRing);
}

void stopWinRenderThread(const std::shared_ptr<WinSession> &ws) {
    if (!ws) {
        return;
    }
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->stopRender = true;
        if (ws->reply == BufReply::None) {
            ws->reply = BufReply::Reset;
        }
    }
    if (ws->vsyncInited) {
        sem_post(&ws->vsyncSem);
    }
    ws->cv.notify_all();
    if (ws->renderThread.joinable()) {
        ws->renderThread.join();
    }
    ws->renderRunning = false;
    if (ws->vsync != nullptr) {
        OH_NativeVSync_Destroy(ws->vsync);
        ws->vsync = nullptr;
    }
    if (ws->vsyncInited) {
        sem_destroy(&ws->vsyncSem);
        ws->vsyncInited = false;
    }
}

// ---------- Engine Process & Supervisor Loop ----------

void logReaderLoop() {
    std::string line;
    char buf[512];
    for (;;) {
        ssize_t n = read(engineLog[0], buf, sizeof(buf));
        if (n <= 0) {
            break;
        }
        for (ssize_t i = 0; i < n; i++) {
            char c = buf[i];
            if (c != '\n') {
                if (line.size() < 480) {
                    line.push_back(c);
                }
                continue;
            }
            if (!line.empty()) {
                OH_LOG_INFO(LOG_APP, "%{public}s", line.c_str());
                line.clear();
            }
        }
    }
    if (!line.empty()) {
        OH_LOG_INFO(LOG_APP, "%{public}s", line.c_str());
    }
}

std::string packageRoot() {
    char resolved[PATH_MAX];
    if (realpath(kEngine, resolved) != nullptr) {
        std::string p(resolved);
        size_t s1 = p.rfind('/');
        if (s1 != std::string::npos && s1 > 0) {
            size_t s2 = p.rfind('/', s1 - 1);
            if (s2 != std::string::npos && s2 > 0) {
                return p.substr(0, s2);
            }
        }
    }
    return "/data/app";
}

std::vector<std::string> buildEngineEnv(const std::string &pkgRoot) {
    const std::string libDir = pkgRoot + "/lib";
    const std::string fontsDir = pkgRoot + "/etc/fonts";
    const std::string fontsConf = fontsDir + "/fonts.conf";
    const std::string baseDir = "/data/storage/el2/base/haps/entry";
    const std::string filesDir = baseDir + "/files";
    const std::string cacheDir = baseDir + "/cache";
    const std::string tempDir = baseDir + "/temp";

    mkdir(filesDir.c_str(), 0755);
    mkdir(cacheDir.c_str(), 0755);
    mkdir(tempDir.c_str(), 0755);

    std::string ldPath = libDir;
    const char *oldLd = getenv("LD_LIBRARY_PATH");
    if (oldLd != nullptr && oldLd[0] != '\0') {
        ldPath += ":";
        ldPath += oldLd;
    }

    auto overridden = [](const char *entry) {
        static const char *const keys[] = {
            "LD_LIBRARY_PATH=", "FONTCONFIG_FILE=", "FONTCONFIG_PATH=",
            "PANGOCAIRO_BACKEND=", "HOME=", "XDG_CACHE_HOME=",
            "XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "TMPDIR=",
            "MYGO_OHOS_PKG_ID=", "MYGO_OHOS_PKG_NAME=", "MYGO_OHOS_PKG_VER=",
            "MYGO_OHOS_LANGS=", "MYGO_OHOS_LOCALE=", "MYGO_OHOS_HOUR_CYCLE=",
            "MYGO_OHOS_FIRST_WEEKDAY=", "MYGO_OHOS_DISPLAYS=",
        };
        for (const char *k : keys) {
            if (strncmp(entry, k, strlen(k)) == 0) {
                return true;
            }
        }
        return false;
    };

    std::vector<std::string> env;
    if (environ != nullptr) {
        for (char **e = environ; *e != nullptr; ++e) {
            if (!overridden(*e)) {
                env.emplace_back(*e);
            }
        }
    }
    env.push_back("LD_LIBRARY_PATH=" + ldPath);
    env.push_back("FONTCONFIG_FILE=" + fontsConf);
    env.push_back("FONTCONFIG_PATH=" + fontsDir);
    env.push_back("PANGOCAIRO_BACKEND=fc");
    env.push_back("HOME=" + filesDir);
    env.push_back("XDG_CACHE_HOME=" + cacheDir);
    env.push_back("XDG_CONFIG_HOME=" + filesDir + "/.config");
    env.push_back("XDG_DATA_HOME=" + filesDir + "/.local/share");
    env.push_back("TMPDIR=" + tempDir);

    CachedEnv ce;
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        ce = gCachedEnv;
    }
    if (!ce.pkgId.empty()) {
        env.push_back("MYGO_OHOS_PKG_ID=" + ce.pkgId);
    }
    if (!ce.pkgName.empty()) {
        env.push_back("MYGO_OHOS_PKG_NAME=" + ce.pkgName);
    }
    if (!ce.pkgVer.empty()) {
        env.push_back("MYGO_OHOS_PKG_VER=" + ce.pkgVer);
    }
    if (!ce.langsCsv.empty()) {
        env.push_back("MYGO_OHOS_LANGS=" + ce.langsCsv);
    }
    if (!ce.locale.empty()) {
        env.push_back("MYGO_OHOS_LOCALE=" + ce.locale);
    }
    if (ce.hourCycle == 12 || ce.hourCycle == 24) {
        env.push_back("MYGO_OHOS_HOUR_CYCLE=" + std::to_string(ce.hourCycle));
    }
    if (ce.firstWeekday >= -1 && ce.firstWeekday <= 6) {
        env.push_back("MYGO_OHOS_FIRST_WEEKDAY=" + std::to_string(ce.firstWeekday));
    }
    if (!ce.displaysSpec.empty()) {
        env.push_back("MYGO_OHOS_DISPLAYS=" + ce.displaysSpec);
    }
    return env;
}

bool startEngine(int32_t w, int32_t h, double scale) {
    int sv[2];
    if (socketpair(AF_UNIX, SOCK_STREAM, 0, sv) != 0) {
        OH_LOG_ERROR(LOG_APP, "socketpair errno=%{public}d", errno);
        return false;
    }
    char size[64];
    snprintf(size, sizeof(size), "--size=%dx%d", w, h);
    char sc[64];
    snprintf(sc, sizeof(sc), "--scale=%.3f", scale);
    const char *darkArg = currentDark ? "--dark=1" : "--dark=0";

    std::string pkgRoot = packageRoot();
    std::vector<std::string> envStrings = buildEngineEnv(pkgRoot);
    std::vector<char *> envp;
    envp.reserve(envStrings.size() + 1);
    for (auto &s : envStrings) {
        envp.push_back(const_cast<char *>(s.c_str()));
    }
    envp.push_back(nullptr);

    pid_t pid = fork();
    if (pid == 0) {
        prctl(PR_SET_PDEATHSIG, SIGKILL);
        dup2(sv[1], 3);
        close(sv[0]);
        close(sv[1]);
        dup2(engineLog[1], 1);
        dup2(engineLog[1], 2);
        close(engineLog[0]);
        close(engineLog[1]);
        const char *argv[] = {kEngine, "--fd=3", size, sc, darkArg, nullptr};
        execve(kEngine, (char *const *)argv, envp.data());
        char why[160];
        int n = snprintf(why, sizeof(why), "execve %s failed: %s (errno %d)\n",
                         kEngine, strerror(errno), errno);
        if (n > 0) {
            ssize_t ignored = write(2, why, static_cast<size_t>(n));
            (void)ignored;
        }
        _exit(127);
    }
    close(sv[1]);
    if (pid < 0) {
        OH_LOG_ERROR(LOG_APP, "fork errno=%{public}d", errno);
        close(sv[0]);
        return false;
    }
    enginePid = pid;
    gFrameTimeoutRestart.store(false);
    {
        std::lock_guard<std::mutex> lk(sendMu);
        sock = sv[0];
    }
    OH_LOG_INFO(LOG_APP, "engine started pid=%{public}d", pid);
    return true;
}

void killEngine() {
    int fd = -1;
    {
        std::lock_guard<std::mutex> lk(sendMu);
        fd = sock;
        sock = -1;
    }
    if (fd >= 0) {
        shutdown(fd, SHUT_RDWR);
        close(fd);
    }
    if (enginePid > 0) {
        if (!gEngineCleanQuit.load()) {
            kill(enginePid, SIGKILL);
        }
        int status = 0;
        waitpid(enginePid, &status, 0);
        OH_LOG_INFO(LOG_APP, "engine %{public}d ended status=%{public}d", enginePid, status);
        enginePid = -1;
    }
}

int recvMessage(uint8_t *kind, std::vector<uint8_t> *payload, int timeoutMs) {
    int fd = -1;
    {
        std::lock_guard<std::mutex> lk(sendMu);
        fd = sock;
    }
    if (fd < 0) {
        return -1;
    }
    struct pollfd pfd;
    pfd.fd = fd;
    pfd.events = POLLIN;
    pfd.revents = 0;
    int p = poll(&pfd, 1, timeoutMs);
    if (p < 0) {
        return errno == EINTR ? 0 : -1;
    }
    if (p == 0) {
        return 0;
    }
    uint8_t hdr[5];
    if (!readFull(fd, hdr, sizeof(hdr))) {
        return -1;
    }
    uint32_t len = 0;
    memcpy(&len, hdr + 1, 4);
    if (len > (1u << 20)) {
        return -1;
    }
    *kind = hdr[0];
    payload->resize(len);
    if (len > 0 && !readFull(fd, payload->data(), len)) {
        return -1;
    }
    return 1;
}

bool decodeWinConfig(const std::vector<uint8_t> &p, WinConfigData *out) {
    if (p.size() < 56) {
        return false;
    }
    uint32_t titleLen = 0;
    memcpy(&titleLen, p.data() + 52, 4);
    if (56 + static_cast<size_t>(titleLen) > p.size()) {
        return false;
    }
    memcpy(&out->winId, p.data() + 0, 4);
    memcpy(&out->parentOrMask, p.data() + 4, 4);
    memcpy(&out->x, p.data() + 8, 4);
    memcpy(&out->y, p.data() + 12, 4);
    memcpy(&out->w, p.data() + 16, 4);
    memcpy(&out->h, p.data() + 20, 4);
    memcpy(&out->minW, p.data() + 24, 4);
    memcpy(&out->minH, p.data() + 28, 4);
    memcpy(&out->maxW, p.data() + 32, 4);
    memcpy(&out->maxH, p.data() + 36, 4);
    memcpy(&out->flags, p.data() + 40, 4);
    memcpy(&out->titleBarHeight, p.data() + 44, 4);
    memcpy(&out->bgRGBA, p.data() + 48, 4);
    out->title.assign(reinterpret_cast<const char *>(p.data() + 56), titleLen);
    return true;
}

bool decodePopupSlice(const std::vector<uint8_t> &p, size_t &off, std::vector<PopupItemData> *out) {
    uint32_t count = 0;
    if (!readU32(p, off, &count) || count > 512) {
        return false;
    }
    out->clear();
    out->reserve(count);
    for (uint32_t i = 0; i < count; ++i) {
        if (off + 8 > p.size()) {
            return false;
        }
        PopupItemData it;
        memcpy(&it.id, p.data() + off, 4);
        it.kind = p[off + 4];
        uint8_t flags = p[off + 5];
        it.enabled = (flags & 1) != 0;
        it.checked = (flags & 2) != 0;
        off += 8;
        if (!readStr(p, off, &it.label) ||
            !readStr(p, off, &it.accel) ||
            !decodePopupSlice(p, off, &it.items)) {
            return false;
        }
        out->push_back(std::move(it));
    }
    return true;
}

bool decodePopupMenu(const std::vector<uint8_t> &p, BridgeEvent *ev) {
    if (p.size() < 20) {
        return false;
    }
    memcpy(&ev->popupReqId, p.data() + 0, 4);
    memcpy(&ev->winId, p.data() + 4, 4);
    memcpy(&ev->popupX, p.data() + 8, 4);
    memcpy(&ev->popupY, p.data() + 12, 4);
    ev->popupRtl = p[16] != 0;
    size_t off = 20;
    return decodePopupSlice(p, off, &ev->popupItems) && off == p.size();
}

void resetAllPendingBuffers() {
    std::vector<std::shared_ptr<WinSession>> list;
    {
        std::lock_guard<std::mutex> lk(gWinMu);
        for (auto &kv : gWindows) {
            if (kv.second) {
                list.push_back(kv.second);
            }
        }
    }
    for (auto &ws : list) {
        {
            std::lock_guard<std::mutex> lk(ws->mu);
            ws->reply = BufReply::Reset;
            ws->wantFrame = false;
        }
        ws->cv.notify_all();
    }
}

void sendInitialStateToEngine() {
    std::vector<uint8_t> envWire;
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        envWire = gCachedEnv.wirePayload;
    }
    if (!envWire.empty()) {
        sendMessage(kEnvInfo, envWire.data(), static_cast<uint32_t>(envWire.size()), -1);
    }
    sendWindowInput(1, currentDark ? kWinThemeDark : kWinThemeLight);
    auto ws1 = findSession(1);
    if (ws1) {
        {
            std::lock_guard<std::mutex> lk(ws1->mu);
            ws1->forceResendSurface = true;
        }
        ws1->cv.notify_all();
    }
}

bool pumpEngineMessages() {
    sendInitialStateToEngine();
    std::vector<uint8_t> payload;

    while (gSupervisorRunning.load()) {
        if (gFrameTimeoutRestart.exchange(false)) {
            return false;
        }
        uint8_t kind = 0;
        int got = recvMessage(&kind, &payload, 250);
        if (got == 0) {
            continue;
        }
        if (got < 0) {
            return !gSupervisorRunning.load() || gEngineCleanQuit.load();
        }

        switch (kind) {
        case kQuit: {
            gEngineCleanQuit.store(true);
            resetAllPendingBuffers();
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::EngineQuit;
            postBridgeEvent(std::move(ev));
            return true;
        }
        case kRequest: {
            if (payload.size() >= 4) {
                uint32_t wid = 0;
                memcpy(&wid, payload.data(), 4);
                auto ws = getOrCreateSession(wid > 0 ? wid : 1);
                {
                    std::lock_guard<std::mutex> lk(ws->mu);
                    ws->wantFrame = true;
                }
                ws->cv.notify_all();
            }
            break;
        }
        case kPresent: {
            if (payload.size() >= 12) {
                uint32_t wid = 0, seq = 0, rects = 0;
                memcpy(&wid, payload.data() + 0, 4);
                memcpy(&seq, payload.data() + 4, 4);
                memcpy(&rects, payload.data() + 8, 4);
                auto ws = findSession(wid > 0 ? wid : 1);
                if (ws) {
                    {
                        std::lock_guard<std::mutex> lk(ws->mu);
                        ws->reply = BufReply::Present;
                        ws->replySeq = seq;
                        ws->replyRectCount = rects;
                    }
                    ws->cv.notify_all();
                }
            }
            break;
        }
        case kAbort: {
            if (payload.size() >= 8) {
                uint32_t wid = 0, seq = 0;
                memcpy(&wid, payload.data() + 0, 4);
                memcpy(&seq, payload.data() + 4, 4);
                auto ws = findSession(wid > 0 ? wid : 1);
                if (ws) {
                    {
                        std::lock_guard<std::mutex> lk(ws->mu);
                        ws->reply = BufReply::Abort;
                        ws->replySeq = seq;
                    }
                    ws->cv.notify_all();
                }
            }
            break;
        }
        case kCursor: {
            if (payload.size() >= 5) {
                uint32_t wid = 0;
                memcpy(&wid, payload.data(), 4);
                auto ws = getOrCreateSession(wid > 0 ? wid : 1);
                ws->desiredCursor.store(payload[4], std::memory_order_relaxed);
                if (ws->uiContext != nullptr) {
                    OH_ArkUI_PostUITask(ws->uiContext,
                                        reinterpret_cast<void *>(static_cast<uintptr_t>(ws->winId)),
                                        applyCursorOnUI);
                }
            }
            break;
        }
        case kFrameRate: {
            if (payload.size() >= 8) {
                uint32_t wid = 0, fps = 0;
                memcpy(&wid, payload.data() + 0, 4);
                memcpy(&fps, payload.data() + 4, 4);
                auto ws = getOrCreateSession(wid > 0 ? wid : 1);
                {
                    std::lock_guard<std::mutex> lk(ws->mu);
                    if (ws->requestedFps != fps) {
                        ws->requestedFps = fps;
                        ws->fpsDirty = true;
                        ws->cv.notify_all();
                    }
                }
            }
            break;
        }
        case kTextInput:
            handleEngineTextInput(payload);
            break;
        case kWinCreate:
        case kWinConfig: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = (kind == kWinCreate) ? BridgeEventKind::WinCreate : BridgeEventKind::WinConfig;
            if (decodeWinConfig(payload, &ev->winCfg)) {
                (void)getOrCreateSession(ev->winCfg.winId);
                postBridgeEvent(std::move(ev));
            }
            break;
        }
        case kWinAction: {
            if (payload.size() >= 5) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::WinAction;
                memcpy(&ev->winId, payload.data() + 0, 4);
                ev->action = payload[4];
                postBridgeEvent(std::move(ev));
            }
            break;
        }
        case kPopupMenu: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::PopupMenu;
            if (decodePopupMenu(payload, ev.get())) {
                postBridgeEvent(std::move(ev));
            } else if (payload.size() >= 4) {
                uint32_t reqId = 0;
                memcpy(&reqId, payload.data(), 4);
                sendPopupResult(reqId, 0);
            }
            break;
        }
        default:
            break;
        }
    }
    return true;
}

void engineSupervisorLoop(int32_t initW, int32_t initH, double initScale) {
    int restarts = 0;
    while (gSupervisorRunning.load()) {
        if (pumpEngineMessages()) {
            break;
        }
        resetAllPendingBuffers();
        if (!gSupervisorRunning.load() || gEngineCleanQuit.load()) {
            break;
        }
        if (restarts >= 3) {
            OH_LOG_ERROR(LOG_APP, "engine failed %{public}d times, giving up", restarts);
            break;
        }
        restarts++;
        auto ev = std::make_unique<BridgeEvent>();
        ev->kind = BridgeEventKind::EngineRestart;
        postBridgeEvent(std::move(ev));

        int backoff = restarts == 1 ? 100 : (restarts == 2 ? 500 : 2000);
        OH_LOG_INFO(LOG_APP, "restarting engine in %{public}dms", backoff);
        killEngine();
        usleep(backoff * 1000);

        int32_t w = initW, h = initH;
        double sc = initScale;
        if (auto ws1 = findSession(1)) {
            std::lock_guard<std::mutex> lk(ws1->mu);
            if (ws1->curW > 0 && ws1->curH > 0) {
                w = ws1->curW;
                h = ws1->curH;
                sc = ws1->scale;
            }
        }
        if (!gSupervisorRunning.load() || !startEngine(w, h, sc)) {
            break;
        }
        OH_LOG_INFO(LOG_APP, "engine restarted (attempt %{public}d)", restarts);
    }
    OH_LOG_INFO(LOG_APP, "engine supervisor ended");
}

void ensureEngineStarted(int32_t w, int32_t h, double scale) {
    if (gSupervisorRunning.load()) {
        return;
    }
    if (gSupervisorThread.joinable()) {
        gSupervisorThread.join();
    }
    gEngineCleanQuit.store(false);
    if (pipe(engineLog) == 0) {
        logReader = std::thread(logReaderLoop);
    }
    if (!startEngine(w, h, scale)) {
        if (engineLog[1] >= 0) {
            close(engineLog[1]);
            engineLog[1] = -1;
        }
        if (logReader.joinable()) {
            logReader.join();
        }
        if (engineLog[0] >= 0) {
            close(engineLog[0]);
            engineLog[0] = -1;
        }
        return;
    }
    gSupervisorRunning.store(true);
    gSupervisorThread = std::thread(engineSupervisorLoop, w, h, scale);
}

void stopEngineSupervisor() {
    gSupervisorRunning.store(false);
    int fd = -1;
    {
        std::lock_guard<std::mutex> lk(sendMu);
        fd = sock;
    }
    if (fd >= 0) {
        shutdown(fd, SHUT_RDWR);
    }
    resetAllPendingBuffers();
    if (gSupervisorThread.joinable()) {
        gSupervisorThread.join();
    }
    killEngine();
    if (engineLog[1] >= 0) {
        close(engineLog[1]);
        engineLog[1] = -1;
    }
    if (logReader.joinable()) {
        logReader.join();
    }
    if (engineLog[0] >= 0) {
        close(engineLog[0]);
        engineLog[0] = -1;
    }
}

// ---------- SurfaceHolder Callbacks (UI Thread) ----------

void OnSurfaceCreated(OH_ArkUI_SurfaceHolder *holder) {
    auto it = gHolderToWinId.find(holder);
    if (it == gHolderToWinId.end()) {
        return;
    }
    auto ws = findSession(it->second);
    if (!ws) {
        return;
    }
    OHNativeWindow *win = OH_ArkUI_XComponent_GetNativeWindow(holder);
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->nativeWindow = win;
    }
    OH_LOG_INFO(LOG_APP, "win=%{public}u surface created nativeWin=%{public}p", ws->winId, win);
}

void OnSurfaceChanged(OH_ArkUI_SurfaceHolder *holder, uint64_t width, uint64_t height) {
    auto it = gHolderToWinId.find(holder);
    if (it == gHolderToWinId.end()) {
        return;
    }
    auto ws = findSession(it->second);
    if (!ws) {
        return;
    }
    int32_t w = static_cast<int32_t>(width);
    int32_t h = static_cast<int32_t>(height);
    OHNativeWindow *win = OH_ArkUI_XComponent_GetNativeWindow(holder);
    if (w <= 0 || h <= 0 || win == nullptr) {
        return;
    }

    if (ws->winId == 1) {
        ensureEngineStarted(w, h, ws->scale);
    }

    {
        std::lock_guard<std::mutex> lk(ws->mu);
        if (ws->renderRunning && ws->nativeWindow == win) {
            ws->targetW = w;
            ws->targetH = h;
            ws->cv.notify_all();
            return;
        }
    }

    stopWinRenderThread(ws);
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->nativeWindow = win;
        ws->curW = w;
        ws->curH = h;
        ws->targetW = w;
        ws->targetH = h;
        ws->stopRender = false;
        ws->reply = BufReply::None;
        ws->fpsDirty = true;
    }
    if (sem_init(&ws->vsyncSem, 0, 0) == 0) {
        ws->vsyncInited = true;
    }
    if (ws->arkWindowId > 0) {
        ws->vsync = OH_NativeVSync_Create_ForAssociatedWindow(static_cast<uint64_t>(ws->arkWindowId), "mygo", 4);
    }
    if (ws->vsync == nullptr) {
        ws->vsync = OH_NativeVSync_Create("mygo", 4);
    }
    ws->renderRunning = true;
    ws->renderThread = std::thread(&WinSession::renderLoop, ws.get());
}

void OnSurfaceDestroyed(OH_ArkUI_SurfaceHolder *holder) {
    auto it = gHolderToWinId.find(holder);
    if (it == gHolderToWinId.end()) {
        return;
    }
    auto ws = findSession(it->second);
    if (!ws) {
        return;
    }
    OH_LOG_INFO(LOG_APP, "win=%{public}u surface destroyed", ws->winId);
    if (attachedImeWinId == ws->winId) {
        detachIME();
    }
    stopWinRenderThread(ws);
    std::lock_guard<std::mutex> lk(ws->mu);
    ws->nativeWindow = nullptr;
}

void OnSurfaceShow(OH_ArkUI_SurfaceHolder *holder) {
    auto it = gHolderToWinId.find(holder);
    if (it != gHolderToWinId.end()) {
        sendWindowInput(it->second, kWinShow);
    }
}

void OnSurfaceHide(OH_ArkUI_SurfaceHolder *holder) {
    auto it = gHolderToWinId.find(holder);
    if (it != gHolderToWinId.end()) {
        sendWindowInput(it->second, kWinHide);
    }
}

void teardownWindowNode(uint32_t winId, bool eraseSession) {
    auto ws = findSession(winId);
    if (!ws) {
        return;
    }
    if (attachedImeWinId == winId) {
        detachIME();
    }
    stopWinRenderThread(ws);

    if (ws->xcNode != nullptr) {
        gNodeToWinId.erase(ws->xcNode);
        if (nodeAPI != nullptr) {
            if (ws->nodeContent != nullptr) {
                OH_ArkUI_NodeContent_RemoveNode(ws->nodeContent, ws->xcNode);
            }
            static const ArkUI_NodeEventType kEvents[] = {
                NODE_TOUCH_EVENT, NODE_ON_MOUSE, NODE_ON_HOVER_EVENT,
                NODE_ON_AXIS, NODE_ON_COASTING_AXIS_EVENT, NODE_ON_KEY_EVENT,
                NODE_ON_FOCUS, NODE_ON_BLUR,
            };
            for (ArkUI_NodeEventType ev : kEvents) {
                nodeAPI->unregisterNodeEvent(ws->xcNode, ev);
            }
            nodeAPI->removeNodeEventReceiver(ws->xcNode, OnNodeEvent);
        }
    }
    if (ws->surfaceHolder != nullptr) {
        gHolderToWinId.erase(ws->surfaceHolder);
        if (ws->surfaceCallback != nullptr) {
            OH_ArkUI_SurfaceHolder_RemoveSurfaceCallback(ws->surfaceHolder, ws->surfaceCallback);
        }
        OH_ArkUI_SurfaceHolder_Dispose(ws->surfaceHolder);
        ws->surfaceHolder = nullptr;
    }
    if (ws->surfaceCallback != nullptr) {
        OH_ArkUI_SurfaceCallback_Dispose(ws->surfaceCallback);
        ws->surfaceCallback = nullptr;
    }
    if (ws->xcNode != nullptr && nodeAPI != nullptr) {
        nodeAPI->disposeNode(ws->xcNode);
        ws->xcNode = nullptr;
    }
    ws->nodeContent = nullptr;
    ws->uiContext = nullptr;

    if (eraseSession) {
        std::lock_guard<std::mutex> lk(gWinMu);
        gWindows.erase(winId);
    }
    if (winId == 1) {
        destroyIME();
        if (pointerHidden) {
            OH_Input_SetPointerVisible(true);
            pointerHidden = false;
        }
        stopEngineSupervisor();
    }
}

// ---------- N-API Helpers & Entrypoints ----------

std::string getNapiString(napi_env env, napi_value val) {
    size_t len = 0;
    if (napi_get_value_string_utf8(env, val, nullptr, 0, &len) != napi_ok || len == 0) {
        return "";
    }
    std::string out(len, '\0');
    napi_get_value_string_utf8(env, val, out.data(), len + 1, &len);
    return out;
}

int32_t getPropInt32(napi_env env, napi_value obj, const char *key, int32_t defVal = 0) {
    napi_value v = nullptr;
    int32_t out = defVal;
    if (napi_get_named_property(env, obj, key, &v) == napi_ok && v != nullptr) {
        napi_get_value_int32(env, v, &out);
    }
    return out;
}

int64_t getPropInt64(napi_env env, napi_value obj, const char *key, int64_t defVal = 0) {
    napi_value v = nullptr;
    int64_t out = defVal;
    if (napi_get_named_property(env, obj, key, &v) == napi_ok && v != nullptr) {
        napi_get_value_int64(env, v, &out);
    }
    return out;
}

double getPropDouble(napi_env env, napi_value obj, const char *key, double defVal = 1.0) {
    napi_value v = nullptr;
    double out = defVal;
    if (napi_get_named_property(env, obj, key, &v) == napi_ok && v != nullptr) {
        napi_get_value_double(env, v, &out);
    }
    return out;
}

bool getPropBool(napi_env env, napi_value obj, const char *key, bool defVal = false) {
    napi_value v = nullptr;
    bool out = defVal;
    if (napi_get_named_property(env, obj, key, &v) == napi_ok && v != nullptr) {
        napi_get_value_bool(env, v, &out);
    }
    return out;
}

std::string getPropString(napi_env env, napi_value obj, const char *key) {
    napi_value v = nullptr;
    if (napi_get_named_property(env, obj, key, &v) == napi_ok && v != nullptr) {
        return getNapiString(env, v);
    }
    return "";
}

napi_value SetBridgeCallbacks(napi_env env, napi_callback_info info) {
    size_t argc = 1;
    napi_value argv[1] = {nullptr};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 1 || argv[0] == nullptr) {
        return nullptr;
    }

    std::vector<std::unique_ptr<BridgeEvent>> pending;
    napi_threadsafe_function tsfn = nullptr;
    {
        std::lock_guard<std::mutex> lk(gBridgeMu);
        if (gBridgeObjRef != nullptr) {
            napi_delete_reference(env, gBridgeObjRef);
            gBridgeObjRef = nullptr;
        }
        if (gBridgeTsfn != nullptr) {
            napi_release_threadsafe_function(gBridgeTsfn, napi_tsfn_release);
            gBridgeTsfn = nullptr;
        }
        napi_create_reference(env, argv[0], 1, &gBridgeObjRef);

        napi_value resName = nullptr;
        napi_create_string_utf8(env, "mygo_bridge", NAPI_AUTO_LENGTH, &resName);
        napi_value dummyFn = nullptr;
        napi_get_named_property(env, argv[0], "onWinCreate", &dummyFn);
        if (napi_create_threadsafe_function(env, dummyFn, nullptr, resName, 0, 1,
                                            nullptr, nullptr, nullptr, BridgeCallJs, &tsfn) == napi_ok) {
            napi_unref_threadsafe_function(env, tsfn);
            gBridgeTsfn = tsfn;
            pending.swap(gQueuedBridgeEvents);
        }
    }
    for (auto &ev : pending) {
        postBridgeEvent(std::move(ev));
    }
    return nullptr;
}

napi_value UpdateEnvInfo(napi_env env, napi_callback_info info) {
    size_t argc = 8;
    napi_value argv[8] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 8) {
        return nullptr;
    }

    CachedEnv ce;
    ce.pkgName = getNapiString(env, argv[0]);
    ce.pkgVer = getNapiString(env, argv[1]);
    ce.pkgId = getNapiString(env, argv[2]);
    ce.locale = getNapiString(env, argv[3]);
    napi_get_value_int32(env, argv[4], &ce.firstWeekday);
    napi_get_value_int32(env, argv[5], &ce.hourCycle);

    std::vector<std::string> langs;
    uint32_t langLen = 0;
    if (napi_get_array_length(env, argv[6], &langLen) == napi_ok) {
        for (uint32_t i = 0; i < langLen && i < 64; ++i) {
            napi_value elem = nullptr;
            if (napi_get_element(env, argv[6], i, &elem) == napi_ok) {
                std::string tag = getNapiString(env, elem);
                if (!tag.empty()) {
                    if (!ce.langsCsv.empty()) {
                        ce.langsCsv.push_back(',');
                    }
                    ce.langsCsv += tag;
                    langs.push_back(std::move(tag));
                }
            }
        }
    }

    struct DispItem {
        int64_t id;
        std::string label;
        int32_t x, y, w, h;
        int32_t wx, wy, ww, wh;
        uint32_t scaleMilli;
        int16_t rot;
        uint16_t flags;
    };
    std::vector<DispItem> disps;
    uint32_t dispLen = 0;
    if (napi_get_array_length(env, argv[7], &dispLen) == napi_ok) {
        for (uint32_t i = 0; i < dispLen && i < 32; ++i) {
            napi_value dObj = nullptr;
            if (napi_get_element(env, argv[7], i, &dObj) != napi_ok || dObj == nullptr) {
                continue;
            }
            DispItem d{};
            d.id = getPropInt64(env, dObj, "id", static_cast<int64_t>(i));
            d.label = getPropString(env, dObj, "label");
            d.x = getPropInt32(env, dObj, "x", 0);
            d.y = getPropInt32(env, dObj, "y", 0);
            d.w = getPropInt32(env, dObj, "w", 0);
            d.h = getPropInt32(env, dObj, "h", 0);
            d.wx = getPropInt32(env, dObj, "wx", d.x);
            d.wy = getPropInt32(env, dObj, "wy", d.y);
            d.ww = getPropInt32(env, dObj, "ww", d.w);
            d.wh = getPropInt32(env, dObj, "wh", d.h);
            double sc = getPropDouble(env, dObj, "scale", 1.0);
            d.scaleMilli = static_cast<uint32_t>(std::max(100L, std::lround(sc * 1000.0)));
            d.rot = static_cast<int16_t>(getPropInt32(env, dObj, "rotation", 0));
            d.flags = static_cast<uint16_t>((getPropBool(env, dObj, "primary") ? 1 : 0) |
                                            (getPropBool(env, dObj, "internal") ? 2 : 0));
            if (d.w <= 0 || d.h <= 0) {
                continue;
            }

            std::string cleanLabel = d.label;
            for (char &ch : cleanLabel) {
                if (ch == ';' || ch == ':') {
                    ch = ' ';
                }
            }
            char spec[256];
            snprintf(spec, sizeof(spec), "%lld:%d,%d,%d,%d:%d,%d,%d,%d:%u:%d:%u:",
                     static_cast<long long>(d.id),
                     d.x, d.y, d.w, d.h,
                     d.wx, d.wy, d.ww, d.wh,
                     d.scaleMilli, static_cast<int>(d.rot), static_cast<unsigned>(d.flags));
            if (!ce.displaysSpec.empty()) {
                ce.displaysSpec.push_back(';');
            }
            ce.displaysSpec += spec;
            ce.displaysSpec += cleanLabel;
            disps.push_back(std::move(d));
        }
    }

    // Encode binary kEnvInfo matching proto.go encodeEnvInfoMessage.
    std::vector<uint8_t> wire(12, 0);
    if (!ce.pkgId.empty() || !ce.pkgName.empty()) {
        wire[0] = 1;
    }
    memcpy(wire.data() + 4, &ce.firstWeekday, 4);
    memcpy(wire.data() + 8, &ce.hourCycle, 4);
    appendStr(wire, ce.pkgName);
    appendStr(wire, ce.pkgVer);
    appendStr(wire, ce.pkgId);
    appendStr(wire, ce.locale);
    appendU32(wire, static_cast<uint32_t>(langs.size()));
    for (const auto &l : langs) {
        appendStr(wire, l);
    }
    appendU32(wire, static_cast<uint32_t>(disps.size()));
    for (const auto &d : disps) {
        uint8_t db[48] = {0};
        memcpy(db + 0, &d.id, 8);
        memcpy(db + 8, &d.x, 4);
        memcpy(db + 12, &d.y, 4);
        memcpy(db + 16, &d.w, 4);
        memcpy(db + 20, &d.h, 4);
        memcpy(db + 24, &d.wx, 4);
        memcpy(db + 28, &d.wy, 4);
        memcpy(db + 32, &d.ww, 4);
        memcpy(db + 36, &d.wh, 4);
        memcpy(db + 40, &d.scaleMilli, 4);
        memcpy(db + 44, &d.rot, 2);
        memcpy(db + 46, &d.flags, 2);
        wire.insert(wire.end(), db, db + 48);
        appendStr(wire, d.label);
    }
    ce.wirePayload = wire;

    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        gCachedEnv = std::move(ce);
    }
    sendMessage(kEnvInfo, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value BindWindow(napi_env env, napi_callback_info info) {
    size_t argc = 7;
    napi_value argv[7] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 7) {
        return nullptr;
    }

    uint32_t winId = 1;
    napi_get_value_uint32(env, argv[0], &winId);
    if (winId == 0) {
        winId = 1;
    }

    teardownWindowNode(winId, false);
    auto ws = getOrCreateSession(winId);

    OH_ArkUI_GetContextFromNapiValue(env, argv[1], &ws->uiContext);
    if (OH_ArkUI_GetNodeContentFromNapiValue(env, argv[2], &ws->nodeContent) != ARKUI_ERROR_CODE_NO_ERROR ||
        ws->nodeContent == nullptr) {
        OH_LOG_ERROR(LOG_APP, "win=%{public}u GetNodeContentFromNapiValue failed", winId);
        return nullptr;
    }
    napi_get_value_int32(env, argv[3], &ws->arkWindowId);
    double scale = 1.0;
    napi_get_value_double(env, argv[4], &scale);
    double hz = 120.0;
    napi_get_value_double(env, argv[5], &hz);
    bool dark = false;
    if (napi_get_value_bool(env, argv[6], &dark) == napi_ok && winId == 1) {
        currentDark = dark;
    }
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->scale = scale > 0 ? scale : 1.0;
        ws->hz = hz > 0 ? hz : 120.0;
    }
    ws->appliedPointerStyle = -1;

    if (nodeAPI == nullptr) {
        nodeAPI = reinterpret_cast<ArkUI_NativeNodeAPI_1 *>(
            OH_ArkUI_QueryModuleInterfaceByName(ARKUI_NATIVE_NODE, "ArkUI_NativeNodeAPI_1"));
    }
    if (nodeAPI == nullptr) {
        OH_LOG_ERROR(LOG_APP, "ArkUI_NativeNodeAPI_1 unavailable");
        return nullptr;
    }

    ws->xcNode = nodeAPI->createNode(ARKUI_NODE_XCOMPONENT);
    if (ws->xcNode == nullptr) {
        return nullptr;
    }

    ArkUI_NumberValue fullPct[] = {{.f32 = 1.0f}};
    ArkUI_AttributeItem pctItem = {fullPct, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_WIDTH_PERCENT, &pctItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_HEIGHT_PERCENT, &pctItem);

    ArkUI_NumberValue oneVal[] = {{.i32 = 1}};
    ArkUI_AttributeItem oneItem = {oneVal, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_FOCUSABLE, &oneItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_DEFAULT_FOCUS, &oneItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_FOCUS_ON_TOUCH, &oneItem);
    OH_ArkUI_XComponent_SetNeedSoftKeyboard(ws->xcNode, true);

    ws->surfaceHolder = OH_ArkUI_SurfaceHolder_Create(ws->xcNode);
    ws->surfaceCallback = OH_ArkUI_SurfaceCallback_Create();
    if (ws->surfaceHolder == nullptr || ws->surfaceCallback == nullptr) {
        teardownWindowNode(winId, winId != 1);
        return nullptr;
    }

    ArkUI_XComponentSurfaceConfig *cfg = OH_ArkUI_XComponentSurfaceConfig_Create();
    if (cfg != nullptr) {
        OH_ArkUI_XComponentSurfaceConfig_SetIsOpaque(cfg, true);
        OH_ArkUI_SurfaceHolder_SetSurfaceConfig(ws->surfaceHolder, cfg);
        OH_ArkUI_XComponentSurfaceConfig_Dispose(cfg);
    }

    gHolderToWinId[ws->surfaceHolder] = winId;
    gNodeToWinId[ws->xcNode] = winId;

    OH_ArkUI_SurfaceCallback_SetSurfaceCreatedEvent(ws->surfaceCallback, OnSurfaceCreated);
    OH_ArkUI_SurfaceCallback_SetSurfaceChangedEvent(ws->surfaceCallback, OnSurfaceChanged);
    OH_ArkUI_SurfaceCallback_SetSurfaceDestroyedEvent(ws->surfaceCallback, OnSurfaceDestroyed);
    OH_ArkUI_SurfaceCallback_SetSurfaceShowEvent(ws->surfaceCallback, OnSurfaceShow);
    OH_ArkUI_SurfaceCallback_SetSurfaceHideEvent(ws->surfaceCallback, OnSurfaceHide);
    OH_ArkUI_SurfaceHolder_AddSurfaceCallback(ws->surfaceHolder, ws->surfaceCallback);

    nodeAPI->addNodeEventReceiver(ws->xcNode, OnNodeEvent);
    static const ArkUI_NodeEventType kEvents[] = {
        NODE_TOUCH_EVENT, NODE_ON_MOUSE, NODE_ON_HOVER_EVENT,
        NODE_ON_AXIS, NODE_ON_COASTING_AXIS_EVENT, NODE_ON_KEY_EVENT,
        NODE_ON_FOCUS, NODE_ON_BLUR,
    };
    for (ArkUI_NodeEventType ev : kEvents) {
        nodeAPI->registerNodeEvent(ws->xcNode, ev, static_cast<int32_t>(ev), nullptr);
    }

    OH_ArkUI_NodeContent_AddNode(ws->nodeContent, ws->xcNode);
    if (ws->uiContext != nullptr) {
        OH_ArkUI_FocusSetKeyProcessingMode(ws->uiContext, ARKUI_KEY_PROCESSING_MODE_FOCUS_ANCESTOR_EVENT);
        OH_ArkUI_FocusActivate(ws->uiContext, false, true);
    }
    OH_ArkUI_FocusRequest(ws->xcNode);
    if (ws->uiContext != nullptr) {
        OH_ArkUI_PostUITask(ws->uiContext,
                            reinterpret_cast<void *>(static_cast<uintptr_t>(winId)),
                            applyCursorOnUI);
    }
    OH_LOG_INFO(LOG_APP, "bound win=%{public}u arkWin=%{public}d scale=%{public}.2f hz=%{public}.0f",
                winId, ws->arkWindowId, ws->scale, ws->hz);
    return nullptr;
}

napi_value UnbindWindow(napi_env env, napi_callback_info info) {
    size_t argc = 1;
    napi_value argv[1] = {nullptr};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    uint32_t winId = 1;
    if (argc >= 1 && argv[0] != nullptr) {
        napi_get_value_uint32(env, argv[0], &winId);
    }
    teardownWindowNode(winId > 0 ? winId : 1, true);
    return nullptr;
}

napi_value NotifyWindowEvent(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {nullptr, nullptr};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    uint32_t winId = 1;
    int32_t op = 0;
    napi_get_value_uint32(env, argv[0], &winId);
    napi_get_value_int32(env, argv[1], &op);
    if (op >= kWinFocus && op <= kWinClosed) {
        if (op == kWinBlur || op == kWinHide || op == kWinMinimize || op == kWinClosed) {
            if (auto ws = findSession(winId)) {
                ws->metaLeftDown = false;
                ws->metaRightDown = false;
            }
            if (attachedImeWinId == winId) {
                detachIME();
            }
        }
        sendWindowInput(winId, static_cast<uint8_t>(op));
    }
    return nullptr;
}

napi_value NotifyWindowRect(napi_env env, napi_callback_info info) {
    size_t argc = 5;
    napi_value argv[5] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 5) {
        return nullptr;
    }
    uint32_t winId = 1;
    int32_t x = 0, y = 0, w = 0, h = 0;
    napi_get_value_uint32(env, argv[0], &winId);
    napi_get_value_int32(env, argv[1], &x);
    napi_get_value_int32(env, argv[2], &y);
    napi_get_value_int32(env, argv[3], &w);
    napi_get_value_int32(env, argv[4], &h);
    sendWindowRectInput(winId, x, y, w, h);
    return nullptr;
}

napi_value NotifyTitleBar(napi_env env, napi_callback_info info) {
    size_t argc = 4;
    napi_value argv[4] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 4) {
        return nullptr;
    }
    uint32_t winId = 1;
    int32_t height = 0, left = 0, right = 0;
    napi_get_value_uint32(env, argv[0], &winId);
    napi_get_value_int32(env, argv[1], &height);
    napi_get_value_int32(env, argv[2], &left);
    napi_get_value_int32(env, argv[3], &right);
    sendWindowTitleBarInput(winId, height, left, right);
    return nullptr;
}

napi_value ResolvePopupMenu(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    uint32_t reqId = 0;
    int32_t chosenId = 0;
    napi_get_value_uint32(env, argv[0], &reqId);
    napi_get_value_int32(env, argv[1], &chosenId);
    sendPopupResult(reqId, chosenId);
    return nullptr;
}

napi_value SetDarkMode(napi_env env, napi_callback_info info) {
    size_t argc = 1;
    napi_value argv[1] = {nullptr};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 1) {
        return nullptr;
    }
    bool dark = false;
    if (napi_get_value_bool(env, argv[0], &dark) == napi_ok) {
        currentDark = dark;
        sendWindowInput(1, dark ? kWinThemeDark : kWinThemeLight);
    }
    return nullptr;
}

napi_value Crash(napi_env, napi_callback_info) {
    if (enginePid > 0) {
        OH_LOG_INFO(LOG_APP, "killing engine %{public}d on purpose", enginePid);
        kill(enginePid, SIGKILL);
    }
    return nullptr;
}

} // namespace

static napi_value Init(napi_env env, napi_value exports) {
    napi_property_descriptor desc[] = {
        {"setBridgeCallbacks", nullptr, SetBridgeCallbacks, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"updateEnvInfo", nullptr, UpdateEnvInfo, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"bindWindow", nullptr, BindWindow, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"unbindWindow", nullptr, UnbindWindow, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyWindowEvent", nullptr, NotifyWindowEvent, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyWindowRect", nullptr, NotifyWindowRect, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyTitleBar", nullptr, NotifyTitleBar, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolvePopupMenu", nullptr, ResolvePopupMenu, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"setDarkMode", nullptr, SetDarkMode, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"crash", nullptr, Crash, nullptr, nullptr, nullptr, napi_default, nullptr},
    };
    napi_define_properties(env, exports, sizeof(desc) / sizeof(desc[0]), desc);
    return exports;
}

static napi_module hostModule = {
    .nm_version = 1,
    .nm_flags = 0,
    .nm_filename = nullptr,
    .nm_register_func = Init,
    .nm_modname = "mygo_host",
    .nm_priv = ((void *)0),
    .reserved = {0},
};

extern "C" __attribute__((constructor)) void RegisterMygoHost(void) {
    napi_module_register(&hostModule);
}
