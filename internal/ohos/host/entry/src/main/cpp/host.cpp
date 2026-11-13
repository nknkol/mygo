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
#include <arkui/drag_and_drop.h>
#include <arkui/native_interface.h>
#include <arkui/native_interface_accessibility.h>
#include <arkui/native_interface_focus.h>
#include <arkui/native_node.h>
#include <arkui/native_node_napi.h>
#include <arkui/ui_input_event.h>
#include <arkui/native_key_event.h>
#include <ace/xcomponent/native_interface_xcomponent.h>
#include <asset/asset_api.h>
#include <asset/asset_type.h>
#include <database/udmf/udmf.h>
#include <database/udmf/uds.h>
#include <inputmethod/inputmethod_controller_capi.h>
#include <multimedia/image_framework/image/pixelmap_native.h>
#include <multimodalinput/oh_input_manager.h>
#include <multimodalinput/oh_pointer_style.h>
#include <native_window/external_window.h>
#include <native_window/buffer_handle.h>
#include <native_buffer/native_buffer.h>
#include <native_vsync/native_vsync.h>
#include <ohaudio/native_audiostreambuilder.h>
#include <ohaudio/native_audiorenderer.h>

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
#include <deque>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>
#include <poll.h>
#include <dlfcn.h>
#include <semaphore.h>
#include <signal.h>
#include <time.h>
#include <unistd.h>
#include <fcntl.h>
#include <linux/dma-buf.h>
#include <linux/futex.h>
#include <linux/sync_file.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
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

constexpr uint8_t kClipChanged  = 17; // host -> engine: {u32 changeCount, u8 external, 3×pad, mimes...}
constexpr uint8_t kClipData     = 18; // host -> engine: {u32 reqId, str err, reps...}
constexpr uint8_t kDialogResult = 19; // host -> engine: {u32 reqId, i32 response, u8 checkbox, 3×pad, str err, paths...}
constexpr uint8_t kShellResult  = 20; // host -> engine: {u32 reqId, str err}
constexpr uint8_t kAppOpen      = 21; // host -> engine: {u8 op, 3×pad, items...}

constexpr uint8_t kClipWrite  = 22; // engine -> host: clipboard write/clear
constexpr uint8_t kClipRead   = 23; // engine -> host: clipboard read
constexpr uint8_t kDialogOpen = 24; // engine -> host: open file/folder dialog
constexpr uint8_t kDialogSave = 25; // engine -> host: save file dialog
constexpr uint8_t kDialogMsg  = 26; // engine -> host: message box
constexpr uint8_t kShellReq   = 27; // engine -> host: {u32 reqId, u8 op, 3×pad, str target}

constexpr uint8_t kMediaCmd     = 28; // host -> engine: {u32 cmd, i64 posMs}
constexpr uint8_t kNotifResult  = 29; // host -> engine: {u32 reqId, u8 denied, 3×pad, str err}
constexpr uint8_t kNotifClicked = 30; // host -> engine: {str id}
constexpr uint8_t kPowerState   = 31; // host -> engine: {u8 onBattery, 3×pad, str event}
constexpr uint8_t kNetState     = 32; // host -> engine: {u8 online, u8 metered, 2×pad}

constexpr uint8_t kAudioOpen   = 33; // engine -> host: {u32 streamId, u32 rate, u32 ch, u32 latMode, u32 shmBytes} + fd
constexpr uint8_t kAudioClose  = 34; // engine -> host: {u32 streamId}
constexpr uint8_t kNowPlaying  = 35; // engine -> host: NowPlaying snapshot
constexpr uint8_t kNotifShow   = 36; // engine -> host: show notification
constexpr uint8_t kNotifRemove = 37; // engine -> host: remove notification(s)
constexpr uint8_t kPowerReq    = 38; // engine -> host: aggregate wake/background lock request
constexpr uint8_t kNetWatch    = 39; // engine -> host: start reporting network changes

constexpr uint8_t kCookieReq       = 40; // engine -> host: cookie/data-store request
constexpr uint8_t kCookieResult    = 41; // host -> engine: cookie/data-store result
constexpr uint8_t kWebReq          = 42; // engine -> host: standalone/embedded ArkWeb command
constexpr uint8_t kWebEvent        = 43; // host -> engine: ArkWeb event
constexpr uint8_t kWebSetup        = 44; // engine -> host: configure standalone or embedded ArkWeb
constexpr uint8_t kPlaceWebViews   = 45; // engine -> host: position embedded WebViews & surface hit regions
constexpr uint8_t kWebAsyncReq     = 46; // engine -> host: CallAsyncFunction / CapturePage / PrintToPDF
constexpr uint8_t kWebAsyncRes     = 47; // host -> engine: async WebView result
constexpr uint8_t kSchemeReq       = 48; // host -> engine: custom protocol request / cancel
constexpr uint8_t kSchemeRes       = 49; // engine -> host: custom protocol response head/body/finish/fail

constexpr uint8_t kSecretReq       = 50; // engine -> host: Asset Store secret op
constexpr uint8_t kSecretRes       = 51; // host -> engine: Asset Store secret result
constexpr uint8_t kHotkeyReq       = 52; // engine -> host: register/unregister global hotkey
constexpr uint8_t kHotkeyRes       = 53; // host -> engine: hotkey registration result
constexpr uint8_t kHotkeyTrig      = 54; // host -> engine: global hotkey triggered
constexpr uint8_t kTrayUpdate      = 55; // engine -> host: system tray upsert/destroy
constexpr uint8_t kTrayEvent       = 56; // host -> engine: system tray click / menu item
constexpr uint8_t kDragSetFormats  = 57; // engine -> host: set accepted drop formats
constexpr uint8_t kDragStart       = 58; // engine -> host: start outgoing data drag
constexpr uint8_t kDragCancel      = 59; // engine -> host: cancel outgoing data drag
constexpr uint8_t kDragEvent       = 60; // host -> engine: drag enter/move/leave/drop/ended
constexpr uint8_t kDragReply       = 61; // engine -> host: drag over/drop reply
constexpr uint8_t kAccessTree      = 62; // engine -> host: surface accessibility tree snapshot
constexpr uint8_t kAccessEvent     = 63; // host -> engine: accessibility enabled / action
constexpr uint8_t kGpuBuffer       = 64; // host -> engine: serialized OH_NativeBuffer swapchain slot + fds

constexpr uint8_t kWinActEnableWebLayer = 17;
constexpr uint8_t kWinActFocusSurface   = 18;
constexpr uint8_t kWinActQuitDeny       = 19;
constexpr uint8_t kWinActPerfHUDOn      = 23;
constexpr uint8_t kWinActPerfHUDOff     = 24;
constexpr uint8_t kWinActPerfHUDToggle  = 25;

constexpr int32_t kArkKeyF12 = 2101;

constexpr uint32_t kMediaPause = 1u << 1;

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
constexpr uint8_t kWinQuitRequest     = 17;

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

void appendBytes(std::vector<uint8_t> &b, const uint8_t *data, size_t len) {
    appendU32(b, static_cast<uint32_t>(len));
    if (len > 0 && data != nullptr) {
        b.insert(b.end(), data, data + len);
    }
}

bool readBytes(const std::vector<uint8_t> &b, size_t &off, std::vector<uint8_t> *out) {
    uint32_t n = 0;
    if (!readU32(b, off, &n) || off + n > b.size()) {
        return false;
    }
    out->assign(b.begin() + static_cast<ptrdiff_t>(off), b.begin() + static_cast<ptrdiff_t>(off + n));
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
    uint32_t textScaleMilli = 1000;
    std::string langsCsv;
    std::string displaysSpec;
    std::vector<uint8_t> wirePayload;
};

std::mutex gEnvMu;
CachedEnv gCachedEnv;
std::vector<uint8_t> gCachedClipWire;
std::vector<std::vector<uint8_t>> gPendingAppOpens;
std::vector<std::vector<uint8_t>> gPendingNotifClicks;
bool gOnBattery = false;
bool gNetOnline = true;
bool gNetMetered = false;
std::atomic<bool> gNowPlayingActive{false};
std::atomic<uint32_t> gNowPlayingCmds{0};
std::atomic<bool> gPerfHudAvailable{false};
std::atomic<bool> gPerfHudEnabled{false};

struct PerfHudData {
    uint32_t winId = 1;
    bool visible = false;
    bool live = false;
    std::string headerLine;
    std::string pacingLine;
    std::string stageLine;
    std::string inputLine;
    std::vector<double> dtMs;
    std::vector<double> uiGlMs;
    std::vector<double> gpuMs;
    std::vector<double> copySwapMs;
};

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
    bool xcIsTexture = false;
    uint32_t bgARGB = 0xFFF4F4F6u;
    OH_ArkUI_SurfaceHolder *surfaceHolder = nullptr;
    OH_ArkUI_SurfaceCallback *surfaceCallback = nullptr;
    ArkUI_AccessibilityProvider *accessProvider = nullptr;
    std::string accessInstId;
    ArkUI_DragAction *activeDragAction = nullptr;
    bool activeDragCanceled = false;
    int32_t arkWindowId = 0;
    bool nodeFocused = false;
    bool metaLeftDown = false;
    bool metaRightDown = false;
    bool f12Down = false;
    float lastPointerX = 0.0f;
    float lastPointerY = 0.0f;
    int32_t appliedPointerStyle = -1;

    // Cursor & input latency atomics.
    std::atomic<uint8_t> desiredCursor{0};
    std::atomic<bool> webLayerEnabled{false};
    std::atomic<int64_t> lastInputNs{0};
    std::atomic<bool> inputSincePresent{false};

    // Performance HUD input cadence & last snapshot (guarded by perfMu).
    std::mutex perfMu;
    int64_t lastScrollInputNs = 0;
    std::deque<double> scrollDtMs;
    PerfHudData lastPerfSnapshot;

    // Render thread state (guarded by mu).
    std::mutex mu;
    std::condition_variable cv;
    std::thread renderThread;
    bool renderRunning = false;
    bool stopRender = false;
    OHNativeWindow *nativeWindow = nullptr;
    OH_NativeVSync *vsync = nullptr;
    uint64_t vsyncSeq = 0;
    int64_t vsyncHwNs = 0;
    int64_t vsyncPeriodHwNs = 0;
    bool vsyncArmed = false;
    bool frameInFlight = false;
    bool inputPrefetch = false;
    int vsyncIdleTicks = 0;
    int64_t lastWantFrameNs = 0;
    int32_t curW = 0;
    int32_t curH = 0;
    int32_t targetW = 0;
    int32_t targetH = 0;
    bool forceResendSurface = false;
    double scale = 1.0;
    double hz = 120.0;
    bool wantFrame = false;
    bool forcePerfPush = false;
    BufReply reply = BufReply::None;
    uint32_t replySeq = 0;
    uint32_t replyRectCount = 0;
    uint32_t replyUiGLUs = 0;
    uint32_t replyGpuFinishUs = 0;
    uint32_t replyCopyUs = 0;
    uint32_t replyPerfMeta = 0;
    int replyFenceFd = -1;
    uint32_t requestedFps = 0;
    bool fpsDirty = true;

    ~WinSession() {
        clearReplyFenceLocked();
    }

    void clearReplyFenceLocked() {
        if (replyFenceFd >= 0) {
            close(replyFenceFd);
            replyFenceFd = -1;
        }
    }

    void ensureVsyncArmedLocked();
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
    ClipWrite,
    ClipRead,
    DialogOpen,
    DialogSave,
    DialogMsg,
    ShellReq,
    NowPlaying,
    NotifShow,
    NotifRemove,
    PowerReq,
    NetWatch,
    CookieReq,
    WebReq,
    WebSetup,
    PlaceWebViews,
    WebAsyncReq,
    SchemeRes,
    TrayUpdate,
    FirstFrame,
    PerfHud,
    EngineQuit,
    EngineRestart,
};

struct ClipWriteData {
    bool clear = false;
    bool concealed = false;
    std::string text;
    std::string html;
    std::string uriList;
    std::vector<uint8_t> png;
};

struct FileFilterData {
    std::string name;
    std::vector<std::string> extensions;
};

struct FileDialogData {
    uint32_t reqId = 0;
    uint32_t winId = 0;
    uint8_t flags = 0;
    std::string title;
    std::string defaultPath;
    std::string buttonLabel;
    std::vector<FileFilterData> filters;
};

struct MessageBoxData {
    uint32_t reqId = 0;
    uint32_t winId = 0;
    int32_t defaultId = 0;
    int32_t cancelId = 0;
    bool checkboxChecked = false;
    std::string boxType;
    std::string title;
    std::string message;
    std::string detail;
    std::string checkboxLabel;
    std::vector<std::string> buttons;
    std::vector<int32_t> destructiveIds;
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

struct NowPlayingData {
    bool active = false;
    uint8_t state = 0;
    bool metaChanged = false;
    uint32_t commands = 0;
    int64_t durationMs = 0;
    int64_t positionMs = 0;
    double rate = 1.0;
    std::string title;
    std::string artist;
    std::string album;
    std::vector<uint8_t> artwork;
};

struct NotifShowData {
    uint32_t reqId = 0;
    bool silent = false;
    std::string id;
    std::string title;
    std::string subtitle;
    std::string body;
    std::string group;
    std::vector<uint8_t> image;
};

struct PowerReqData {
    bool keepSystem = false;
    bool keepDisplay = false;
    bool keepRunning = false;
    std::string reason;
};

struct CookieReqData {
    uint32_t reqId = 0;
    uint8_t op = 0;
    bool incognito = false;
    uint32_t siteDataTypes = 0;
    std::string url;
    std::string headerOrName;
    std::string domain;
    std::string path;
};

struct UserScriptData {
    std::string source;
    bool atDocEnd = false;
    bool allFrames = false;
};

struct WebSetupData {
    uint32_t webId = 0;
    uint32_t hostWinId = 0;
    uint32_t flags = 0;
    uint32_t bgRGBA = 0;
    double zoom = 1.0;
    std::string ua;
    std::vector<std::string> schemes;
    std::vector<UserScriptData> scripts;
};

struct PlacedWebViewData {
    uint32_t webId = 0;
    float frameX = 0, frameY = 0, frameW = 0, frameH = 0;
    float clipX = 0, clipY = 0, clipW = 0, clipH = 0;
    float pageW = 0, pageH = 0;
    float r0 = 0, r1 = 0, r2 = 0, r3 = 0;
};

struct SurfaceRegionData {
    float x = 0, y = 0, w = 0, h = 0;
};

struct PlaceWebViewsData {
    uint32_t winId = 0;
    std::vector<PlacedWebViewData> views;
    std::vector<SurfaceRegionData> surfaceRegions;
};

struct WebAsyncReqData {
    uint32_t reqId = 0;
    uint32_t webId = 0;
    uint8_t op = 0;
    bool landscape = false;
    bool background = false;
    double pageWidth = 0;
    double pageHeight = 0;
    double marginTop = 0;
    double marginRight = 0;
    double marginBottom = 0;
    double marginLeft = 0;
    std::string body;
};

struct SchemeHeaderData {
    std::string name;
    std::string headerValue;
};

struct SchemeResData {
    uint32_t reqId = 0;
    uint8_t op = 0;
    int32_t status = 0;
    std::vector<SchemeHeaderData> headers;
    std::vector<uint8_t> chunk;
    std::string err;
};

struct TrayUpdateData {
    uint32_t trayId = 0;
    uint8_t op = 0;
    bool hasImage = false;
    bool templateImg = false;
    bool hasMenu = false;
    std::string id;
    std::string title;
    std::string tooltip;
    std::vector<uint8_t> png;
    std::vector<PopupItemData> menuItems;
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
    ClipWriteData clipWrite;
    uint32_t reqId = 0;
    std::vector<std::string> strList;
    FileDialogData fileDlg;
    MessageBoxData msgBox;
    uint8_t shellOp = 0;
    std::string shellTarget;
    NowPlayingData nowPlaying;
    NotifShowData notifShow;
    bool notifRemoveAll = false;
    std::string notifRemoveId;
    PowerReqData powerReq;
    CookieReqData cookieReq;
    uint8_t webOp = 0;
    std::string webArg1;
    std::string webArg2;
    WebSetupData webSetup;
    PlaceWebViewsData placeWebViews;
    WebAsyncReqData webAsyncReq;
    SchemeResData schemeRes;
    TrayUpdateData trayUpdate;
    PerfHudData perfHud;
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
    case BridgeEventKind::ClipWrite: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vClear = nullptr, vConc = nullptr, vTxt = nullptr, vHtml = nullptr, vUri = nullptr, vPng = nullptr;
        napi_get_boolean(env, ev->clipWrite.clear, &vClear);
        napi_get_boolean(env, ev->clipWrite.concealed, &vConc);
        napi_create_string_utf8(env, ev->clipWrite.text.c_str(), ev->clipWrite.text.size(), &vTxt);
        napi_create_string_utf8(env, ev->clipWrite.html.c_str(), ev->clipWrite.html.size(), &vHtml);
        napi_create_string_utf8(env, ev->clipWrite.uriList.c_str(), ev->clipWrite.uriList.size(), &vUri);
        void *abData = nullptr;
        napi_create_arraybuffer(env, ev->clipWrite.png.size(), &abData, &vPng);
        if (abData != nullptr && !ev->clipWrite.png.empty()) {
            memcpy(abData, ev->clipWrite.png.data(), ev->clipWrite.png.size());
        }
        napi_set_named_property(env, obj, "clear", vClear);
        napi_set_named_property(env, obj, "concealed", vConc);
        napi_set_named_property(env, obj, "text", vTxt);
        napi_set_named_property(env, obj, "html", vHtml);
        napi_set_named_property(env, obj, "uriList", vUri);
        napi_set_named_property(env, obj, "png", vPng);
        callBridgeMethod(env, cbObj, "onClipWrite", 1, &obj);
        break;
    }
    case BridgeEventKind::ClipRead: {
        napi_value args[2] = {nullptr, nullptr};
        napi_create_uint32(env, ev->reqId, &args[0]);
        napi_create_array_with_length(env, ev->strList.size(), &args[1]);
        for (size_t i = 0; i < ev->strList.size(); ++i) {
            napi_value s = nullptr;
            napi_create_string_utf8(env, ev->strList[i].c_str(), ev->strList[i].size(), &s);
            napi_set_element(env, args[1], static_cast<uint32_t>(i), s);
        }
        callBridgeMethod(env, cbObj, "onClipRead", 2, args);
        break;
    }
    case BridgeEventKind::DialogOpen:
    case BridgeEventKind::DialogSave: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vReq = nullptr, vWin = nullptr, vFlags = nullptr, vTitle = nullptr, vDef = nullptr, vBtn = nullptr, vFlt = nullptr;
        napi_create_uint32(env, ev->fileDlg.reqId, &vReq);
        napi_create_uint32(env, ev->fileDlg.winId, &vWin);
        napi_create_uint32(env, ev->fileDlg.flags, &vFlags);
        napi_create_string_utf8(env, ev->fileDlg.title.c_str(), ev->fileDlg.title.size(), &vTitle);
        napi_create_string_utf8(env, ev->fileDlg.defaultPath.c_str(), ev->fileDlg.defaultPath.size(), &vDef);
        napi_create_string_utf8(env, ev->fileDlg.buttonLabel.c_str(), ev->fileDlg.buttonLabel.size(), &vBtn);
        napi_create_array_with_length(env, ev->fileDlg.filters.size(), &vFlt);
        for (size_t i = 0; i < ev->fileDlg.filters.size(); ++i) {
            const FileFilterData &ff = ev->fileDlg.filters[i];
            napi_value fo = nullptr, fn = nullptr, fe = nullptr;
            napi_create_object(env, &fo);
            napi_create_string_utf8(env, ff.name.c_str(), ff.name.size(), &fn);
            napi_create_array_with_length(env, ff.extensions.size(), &fe);
            for (size_t j = 0; j < ff.extensions.size(); ++j) {
                napi_value es = nullptr;
                napi_create_string_utf8(env, ff.extensions[j].c_str(), ff.extensions[j].size(), &es);
                napi_set_element(env, fe, static_cast<uint32_t>(j), es);
            }
            napi_set_named_property(env, fo, "name", fn);
            napi_set_named_property(env, fo, "extensions", fe);
            napi_set_element(env, vFlt, static_cast<uint32_t>(i), fo);
        }
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "winId", vWin);
        napi_set_named_property(env, obj, "flags", vFlags);
        napi_set_named_property(env, obj, "title", vTitle);
        napi_set_named_property(env, obj, "defaultPath", vDef);
        napi_set_named_property(env, obj, "buttonLabel", vBtn);
        napi_set_named_property(env, obj, "filters", vFlt);
        callBridgeMethod(env, cbObj, ev->kind == BridgeEventKind::DialogOpen ? "onDialogOpen" : "onDialogSave", 1, &obj);
        break;
    }
    case BridgeEventKind::DialogMsg: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        auto setStr = [&](const char *k, const std::string &s) {
            napi_value v = nullptr;
            napi_create_string_utf8(env, s.c_str(), s.size(), &v);
            napi_set_named_property(env, obj, k, v);
        };
        napi_value vReq = nullptr, vWin = nullptr, vDef = nullptr, vCan = nullptr, vChk = nullptr, vBtns = nullptr, vDestr = nullptr;
        napi_create_uint32(env, ev->msgBox.reqId, &vReq);
        napi_create_uint32(env, ev->msgBox.winId, &vWin);
        napi_create_int32(env, ev->msgBox.defaultId, &vDef);
        napi_create_int32(env, ev->msgBox.cancelId, &vCan);
        napi_get_boolean(env, ev->msgBox.checkboxChecked, &vChk);
        napi_create_array_with_length(env, ev->msgBox.buttons.size(), &vBtns);
        for (size_t i = 0; i < ev->msgBox.buttons.size(); ++i) {
            napi_value bs = nullptr;
            napi_create_string_utf8(env, ev->msgBox.buttons[i].c_str(), ev->msgBox.buttons[i].size(), &bs);
            napi_set_element(env, vBtns, static_cast<uint32_t>(i), bs);
        }
        napi_create_array_with_length(env, ev->msgBox.destructiveIds.size(), &vDestr);
        for (size_t i = 0; i < ev->msgBox.destructiveIds.size(); ++i) {
            napi_value di = nullptr;
            napi_create_int32(env, ev->msgBox.destructiveIds[i], &di);
            napi_set_element(env, vDestr, static_cast<uint32_t>(i), di);
        }
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "winId", vWin);
        napi_set_named_property(env, obj, "defaultId", vDef);
        napi_set_named_property(env, obj, "cancelId", vCan);
        napi_set_named_property(env, obj, "checkboxChecked", vChk);
        setStr("boxType", ev->msgBox.boxType);
        setStr("title", ev->msgBox.title);
        setStr("message", ev->msgBox.message);
        setStr("detail", ev->msgBox.detail);
        setStr("checkboxLabel", ev->msgBox.checkboxLabel);
        napi_set_named_property(env, obj, "buttons", vBtns);
        napi_set_named_property(env, obj, "destructiveIds", vDestr);
        callBridgeMethod(env, cbObj, "onDialogMsg", 1, &obj);
        break;
    }
    case BridgeEventKind::ShellReq: {
        napi_value args[3] = {nullptr, nullptr, nullptr};
        napi_create_uint32(env, ev->reqId, &args[0]);
        napi_create_uint32(env, ev->shellOp, &args[1]);
        napi_create_string_utf8(env, ev->shellTarget.c_str(), ev->shellTarget.size(), &args[2]);
        callBridgeMethod(env, cbObj, "onShellReq", 3, args);
        break;
    }
    case BridgeEventKind::NowPlaying: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vAct = nullptr, vSt = nullptr, vMeta = nullptr, vCmds = nullptr;
        napi_value vDur = nullptr, vPos = nullptr, vRate = nullptr;
        napi_value vTitle = nullptr, vArtist = nullptr, vAlbum = nullptr, vArt = nullptr;
        napi_get_boolean(env, ev->nowPlaying.active, &vAct);
        napi_create_uint32(env, ev->nowPlaying.state, &vSt);
        napi_get_boolean(env, ev->nowPlaying.metaChanged, &vMeta);
        napi_create_uint32(env, ev->nowPlaying.commands, &vCmds);
        napi_create_double(env, static_cast<double>(ev->nowPlaying.durationMs), &vDur);
        napi_create_double(env, static_cast<double>(ev->nowPlaying.positionMs), &vPos);
        napi_create_double(env, ev->nowPlaying.rate, &vRate);
        napi_create_string_utf8(env, ev->nowPlaying.title.c_str(), ev->nowPlaying.title.size(), &vTitle);
        napi_create_string_utf8(env, ev->nowPlaying.artist.c_str(), ev->nowPlaying.artist.size(), &vArtist);
        napi_create_string_utf8(env, ev->nowPlaying.album.c_str(), ev->nowPlaying.album.size(), &vAlbum);
        void *abData = nullptr;
        napi_create_arraybuffer(env, ev->nowPlaying.artwork.size(), &abData, &vArt);
        if (abData != nullptr && !ev->nowPlaying.artwork.empty()) {
            memcpy(abData, ev->nowPlaying.artwork.data(), ev->nowPlaying.artwork.size());
        }
        napi_set_named_property(env, obj, "active", vAct);
        napi_set_named_property(env, obj, "state", vSt);
        napi_set_named_property(env, obj, "metaChanged", vMeta);
        napi_set_named_property(env, obj, "commands", vCmds);
        napi_set_named_property(env, obj, "durationMs", vDur);
        napi_set_named_property(env, obj, "positionMs", vPos);
        napi_set_named_property(env, obj, "rate", vRate);
        napi_set_named_property(env, obj, "title", vTitle);
        napi_set_named_property(env, obj, "artist", vArtist);
        napi_set_named_property(env, obj, "album", vAlbum);
        napi_set_named_property(env, obj, "artwork", vArt);
        callBridgeMethod(env, cbObj, "onNowPlaying", 1, &obj);
        break;
    }
    case BridgeEventKind::NotifShow: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vReq = nullptr, vSil = nullptr, vId = nullptr, vTitle = nullptr;
        napi_value vSub = nullptr, vBody = nullptr, vGrp = nullptr, vImg = nullptr;
        napi_create_uint32(env, ev->notifShow.reqId, &vReq);
        napi_get_boolean(env, ev->notifShow.silent, &vSil);
        napi_create_string_utf8(env, ev->notifShow.id.c_str(), ev->notifShow.id.size(), &vId);
        napi_create_string_utf8(env, ev->notifShow.title.c_str(), ev->notifShow.title.size(), &vTitle);
        napi_create_string_utf8(env, ev->notifShow.subtitle.c_str(), ev->notifShow.subtitle.size(), &vSub);
        napi_create_string_utf8(env, ev->notifShow.body.c_str(), ev->notifShow.body.size(), &vBody);
        napi_create_string_utf8(env, ev->notifShow.group.c_str(), ev->notifShow.group.size(), &vGrp);
        void *abData = nullptr;
        napi_create_arraybuffer(env, ev->notifShow.image.size(), &abData, &vImg);
        if (abData != nullptr && !ev->notifShow.image.empty()) {
            memcpy(abData, ev->notifShow.image.data(), ev->notifShow.image.size());
        }
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "silent", vSil);
        napi_set_named_property(env, obj, "id", vId);
        napi_set_named_property(env, obj, "title", vTitle);
        napi_set_named_property(env, obj, "subtitle", vSub);
        napi_set_named_property(env, obj, "body", vBody);
        napi_set_named_property(env, obj, "group", vGrp);
        napi_set_named_property(env, obj, "image", vImg);
        callBridgeMethod(env, cbObj, "onNotifShow", 1, &obj);
        break;
    }
    case BridgeEventKind::NotifRemove: {
        napi_value args[2] = {nullptr, nullptr};
        napi_get_boolean(env, ev->notifRemoveAll, &args[0]);
        napi_create_string_utf8(env, ev->notifRemoveId.c_str(), ev->notifRemoveId.size(), &args[1]);
        callBridgeMethod(env, cbObj, "onNotifRemove", 2, args);
        break;
    }
    case BridgeEventKind::PowerReq: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vSys = nullptr, vDisp = nullptr, vRun = nullptr, vRsn = nullptr;
        napi_get_boolean(env, ev->powerReq.keepSystem, &vSys);
        napi_get_boolean(env, ev->powerReq.keepDisplay, &vDisp);
        napi_get_boolean(env, ev->powerReq.keepRunning, &vRun);
        napi_create_string_utf8(env, ev->powerReq.reason.c_str(), ev->powerReq.reason.size(), &vRsn);
        napi_set_named_property(env, obj, "keepSystem", vSys);
        napi_set_named_property(env, obj, "keepDisplay", vDisp);
        napi_set_named_property(env, obj, "keepRunning", vRun);
        napi_set_named_property(env, obj, "reason", vRsn);
        callBridgeMethod(env, cbObj, "onPowerReq", 1, &obj);
        break;
    }
    case BridgeEventKind::NetWatch:
        callBridgeMethod(env, cbObj, "onNetWatch", 0, nullptr);
        break;
    case BridgeEventKind::CookieReq: {
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vReq = nullptr, vOp = nullptr, vInc = nullptr, vTypes = nullptr;
        napi_value vUrl = nullptr, vHdr = nullptr, vDom = nullptr, vPath = nullptr;
        napi_create_uint32(env, ev->cookieReq.reqId, &vReq);
        napi_create_uint32(env, ev->cookieReq.op, &vOp);
        napi_get_boolean(env, ev->cookieReq.incognito, &vInc);
        napi_create_uint32(env, ev->cookieReq.siteDataTypes, &vTypes);
        napi_create_string_utf8(env, ev->cookieReq.url.c_str(), ev->cookieReq.url.size(), &vUrl);
        napi_create_string_utf8(env, ev->cookieReq.headerOrName.c_str(), ev->cookieReq.headerOrName.size(), &vHdr);
        napi_create_string_utf8(env, ev->cookieReq.domain.c_str(), ev->cookieReq.domain.size(), &vDom);
        napi_create_string_utf8(env, ev->cookieReq.path.c_str(), ev->cookieReq.path.size(), &vPath);
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "op", vOp);
        napi_set_named_property(env, obj, "incognito", vInc);
        napi_set_named_property(env, obj, "siteDataTypes", vTypes);
        napi_set_named_property(env, obj, "url", vUrl);
        napi_set_named_property(env, obj, "headerOrName", vHdr);
        napi_set_named_property(env, obj, "domain", vDom);
        napi_set_named_property(env, obj, "path", vPath);
        callBridgeMethod(env, cbObj, "onCookieReq", 1, &obj);
        break;
    }
    case BridgeEventKind::WebReq: {
        napi_value args[4] = {nullptr, nullptr, nullptr, nullptr};
        napi_create_uint32(env, ev->winId, &args[0]);
        napi_create_uint32(env, ev->webOp, &args[1]);
        napi_create_string_utf8(env, ev->webArg1.c_str(), ev->webArg1.size(), &args[2]);
        napi_create_string_utf8(env, ev->webArg2.c_str(), ev->webArg2.size(), &args[3]);
        callBridgeMethod(env, cbObj, "onWebReq", 4, args);
        break;
    }
    case BridgeEventKind::WebSetup: {
        const auto &ws = ev->webSetup;
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vWeb = nullptr, vHost = nullptr, vFlags = nullptr, vBg = nullptr, vZoom = nullptr, vUa = nullptr;
        napi_create_uint32(env, ws.webId, &vWeb);
        napi_create_uint32(env, ws.hostWinId, &vHost);
        napi_create_uint32(env, ws.flags, &vFlags);
        napi_create_uint32(env, ws.bgRGBA, &vBg);
        napi_create_double(env, ws.zoom, &vZoom);
        napi_create_string_utf8(env, ws.ua.c_str(), ws.ua.size(), &vUa);
        napi_set_named_property(env, obj, "webId", vWeb);
        napi_set_named_property(env, obj, "hostWinId", vHost);
        napi_set_named_property(env, obj, "flags", vFlags);
        napi_set_named_property(env, obj, "bgRGBA", vBg);
        napi_set_named_property(env, obj, "zoom", vZoom);
        napi_set_named_property(env, obj, "ua", vUa);

        napi_value scArr = nullptr;
        napi_create_array_with_length(env, ws.schemes.size(), &scArr);
        for (size_t i = 0; i < ws.schemes.size(); ++i) {
            napi_value sv = nullptr;
            napi_create_string_utf8(env, ws.schemes[i].c_str(), ws.schemes[i].size(), &sv);
            napi_set_element(env, scArr, static_cast<uint32_t>(i), sv);
        }
        napi_set_named_property(env, obj, "schemes", scArr);

        napi_value usArr = nullptr;
        napi_create_array_with_length(env, ws.scripts.size(), &usArr);
        for (size_t i = 0; i < ws.scripts.size(); ++i) {
            napi_value uo = nullptr, vSrc = nullptr, vEnd = nullptr, vAll = nullptr;
            napi_create_object(env, &uo);
            napi_create_string_utf8(env, ws.scripts[i].source.c_str(), ws.scripts[i].source.size(), &vSrc);
            napi_get_boolean(env, ws.scripts[i].atDocEnd, &vEnd);
            napi_get_boolean(env, ws.scripts[i].allFrames, &vAll);
            napi_set_named_property(env, uo, "source", vSrc);
            napi_set_named_property(env, uo, "atDocEnd", vEnd);
            napi_set_named_property(env, uo, "allFrames", vAll);
            napi_set_element(env, usArr, static_cast<uint32_t>(i), uo);
        }
        napi_set_named_property(env, obj, "scripts", usArr);
        callBridgeMethod(env, cbObj, "onWebSetup", 1, &obj);
        break;
    }
    case BridgeEventKind::PlaceWebViews: {
        const auto &pw = ev->placeWebViews;
        napi_value obj = nullptr, vWin = nullptr, vArr = nullptr, rArr = nullptr;
        napi_create_object(env, &obj);
        napi_create_uint32(env, pw.winId, &vWin);
        napi_set_named_property(env, obj, "winId", vWin);

        napi_create_array_with_length(env, pw.views.size(), &vArr);
        for (size_t i = 0; i < pw.views.size(); ++i) {
            const auto &it = pw.views[i];
            napi_value vo = nullptr;
            napi_create_object(env, &vo);
            auto setNum = [&](const char *k, double d) {
                napi_value nv = nullptr;
                napi_create_double(env, d, &nv);
                napi_set_named_property(env, vo, k, nv);
            };
            napi_value vWid = nullptr;
            napi_create_uint32(env, it.webId, &vWid);
            napi_set_named_property(env, vo, "webId", vWid);
            setNum("frameX", it.frameX);
            setNum("frameY", it.frameY);
            setNum("frameW", it.frameW);
            setNum("frameH", it.frameH);
            setNum("clipX", it.clipX);
            setNum("clipY", it.clipY);
            setNum("clipW", it.clipW);
            setNum("clipH", it.clipH);
            setNum("pageW", it.pageW);
            setNum("pageH", it.pageH);
            setNum("r0", it.r0);
            setNum("r1", it.r1);
            setNum("r2", it.r2);
            setNum("r3", it.r3);
            napi_set_element(env, vArr, static_cast<uint32_t>(i), vo);
        }
        napi_set_named_property(env, obj, "views", vArr);

        napi_create_array_with_length(env, pw.surfaceRegions.size(), &rArr);
        for (size_t i = 0; i < pw.surfaceRegions.size(); ++i) {
            const auto &rg = pw.surfaceRegions[i];
            napi_value ro = nullptr, vx = nullptr, vy = nullptr, vw = nullptr, vh = nullptr;
            napi_create_object(env, &ro);
            napi_create_double(env, rg.x, &vx);
            napi_create_double(env, rg.y, &vy);
            napi_create_double(env, rg.w, &vw);
            napi_create_double(env, rg.h, &vh);
            napi_set_named_property(env, ro, "x", vx);
            napi_set_named_property(env, ro, "y", vy);
            napi_set_named_property(env, ro, "w", vw);
            napi_set_named_property(env, ro, "h", vh);
            napi_set_element(env, rArr, static_cast<uint32_t>(i), ro);
        }
        napi_set_named_property(env, obj, "surfaceRegions", rArr);
        callBridgeMethod(env, cbObj, "onPlaceWebViews", 1, &obj);
        break;
    }
    case BridgeEventKind::WebAsyncReq: {
        const auto &ar = ev->webAsyncReq;
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        auto setNum = [&](const char *k, double d) {
            napi_value nv = nullptr;
            napi_create_double(env, d, &nv);
            napi_set_named_property(env, obj, k, nv);
        };
        napi_value vReq = nullptr, vWeb = nullptr, vOp = nullptr, vLand = nullptr, vBg = nullptr, vBody = nullptr;
        napi_create_uint32(env, ar.reqId, &vReq);
        napi_create_uint32(env, ar.webId, &vWeb);
        napi_create_uint32(env, ar.op, &vOp);
        napi_get_boolean(env, ar.landscape, &vLand);
        napi_get_boolean(env, ar.background, &vBg);
        napi_create_string_utf8(env, ar.body.c_str(), ar.body.size(), &vBody);
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "webId", vWeb);
        napi_set_named_property(env, obj, "op", vOp);
        napi_set_named_property(env, obj, "landscape", vLand);
        napi_set_named_property(env, obj, "background", vBg);
        setNum("pageWidth", ar.pageWidth);
        setNum("pageHeight", ar.pageHeight);
        setNum("marginTop", ar.marginTop);
        setNum("marginRight", ar.marginRight);
        setNum("marginBottom", ar.marginBottom);
        setNum("marginLeft", ar.marginLeft);
        napi_set_named_property(env, obj, "body", vBody);
        callBridgeMethod(env, cbObj, "onWebAsyncReq", 1, &obj);
        break;
    }
    case BridgeEventKind::SchemeRes: {
        const auto &sr = ev->schemeRes;
        napi_value obj = nullptr, vReq = nullptr, vOp = nullptr, vSt = nullptr, hArr = nullptr, vChunk = nullptr, vErr = nullptr;
        napi_create_object(env, &obj);
        napi_create_uint32(env, sr.reqId, &vReq);
        napi_create_uint32(env, sr.op, &vOp);
        napi_create_int32(env, sr.status, &vSt);
        napi_create_string_utf8(env, sr.err.c_str(), sr.err.size(), &vErr);
        napi_set_named_property(env, obj, "reqId", vReq);
        napi_set_named_property(env, obj, "op", vOp);
        napi_set_named_property(env, obj, "status", vSt);
        napi_set_named_property(env, obj, "err", vErr);

        napi_create_array_with_length(env, sr.headers.size(), &hArr);
        for (size_t i = 0; i < sr.headers.size(); ++i) {
            napi_value ho = nullptr, vk = nullptr, vv = nullptr;
            napi_create_object(env, &ho);
            napi_create_string_utf8(env, sr.headers[i].name.c_str(), sr.headers[i].name.size(), &vk);
            napi_create_string_utf8(env, sr.headers[i].headerValue.c_str(), sr.headers[i].headerValue.size(), &vv);
            napi_set_named_property(env, ho, "name", vk);
            napi_set_named_property(env, ho, "headerValue", vv);
            napi_set_element(env, hArr, static_cast<uint32_t>(i), ho);
        }
        napi_set_named_property(env, obj, "headers", hArr);

        void *abPtr = nullptr;
        napi_create_arraybuffer(env, sr.chunk.size(), &abPtr, &vChunk);
        if (abPtr != nullptr && !sr.chunk.empty()) {
            memcpy(abPtr, sr.chunk.data(), sr.chunk.size());
        }
        napi_set_named_property(env, obj, "chunk", vChunk);
        callBridgeMethod(env, cbObj, "onSchemeRes", 1, &obj);
        break;
    }
    case BridgeEventKind::TrayUpdate: {
        const auto &tu = ev->trayUpdate;
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        uint32_t flags = (tu.hasImage ? 1u : 0u) |
                         (tu.templateImg ? 2u : 0u) |
                         (tu.hasMenu ? 4u : 0u);
        napi_value vTid = nullptr, vOp = nullptr, vFlags = nullptr;
        napi_value vId = nullptr, vTitle = nullptr, vTip = nullptr, vPng = nullptr;
        napi_create_uint32(env, tu.trayId, &vTid);
        napi_create_uint32(env, tu.op, &vOp);
        napi_create_uint32(env, flags, &vFlags);
        napi_create_string_utf8(env, tu.id.c_str(), tu.id.size(), &vId);
        napi_create_string_utf8(env, tu.title.c_str(), tu.title.size(), &vTitle);
        napi_create_string_utf8(env, tu.tooltip.c_str(), tu.tooltip.size(), &vTip);
        void *abPtr = nullptr;
        napi_create_arraybuffer(env, tu.png.size(), &abPtr, &vPng);
        if (abPtr != nullptr && !tu.png.empty()) {
            memcpy(abPtr, tu.png.data(), tu.png.size());
        }
        napi_value vMenu = makeJsMenuItems(env, tu.menuItems);
        napi_set_named_property(env, obj, "trayId", vTid);
        napi_set_named_property(env, obj, "op", vOp);
        napi_set_named_property(env, obj, "flags", vFlags);
        napi_set_named_property(env, obj, "id", vId);
        napi_set_named_property(env, obj, "title", vTitle);
        napi_set_named_property(env, obj, "tooltip", vTip);
        napi_set_named_property(env, obj, "png", vPng);
        napi_set_named_property(env, obj, "items", vMenu);
        callBridgeMethod(env, cbObj, "onTrayUpdate", 1, &obj);
        break;
    }
    case BridgeEventKind::FirstFrame: {
        napi_value arg = nullptr;
        napi_create_uint32(env, ev->winId, &arg);
        callBridgeMethod(env, cbObj, "onFirstFrame", 1, &arg);
        break;
    }
    case BridgeEventKind::PerfHud: {
        const auto &ph = ev->perfHud;
        napi_value obj = nullptr;
        napi_create_object(env, &obj);
        napi_value vWin = nullptr, vVis = nullptr, vLive = nullptr;
        napi_value vHdr = nullptr, vPac = nullptr, vStg = nullptr, vInp = nullptr;
        napi_create_uint32(env, ph.winId, &vWin);
        napi_get_boolean(env, ph.visible, &vVis);
        napi_get_boolean(env, ph.live, &vLive);
        napi_create_string_utf8(env, ph.headerLine.c_str(), ph.headerLine.size(), &vHdr);
        napi_create_string_utf8(env, ph.pacingLine.c_str(), ph.pacingLine.size(), &vPac);
        napi_create_string_utf8(env, ph.stageLine.c_str(), ph.stageLine.size(), &vStg);
        napi_create_string_utf8(env, ph.inputLine.c_str(), ph.inputLine.size(), &vInp);
        napi_set_named_property(env, obj, "winId", vWin);
        napi_set_named_property(env, obj, "visible", vVis);
        napi_set_named_property(env, obj, "live", vLive);
        napi_set_named_property(env, obj, "headerLine", vHdr);
        napi_set_named_property(env, obj, "pacingLine", vPac);
        napi_set_named_property(env, obj, "stageLine", vStg);
        napi_set_named_property(env, obj, "inputLine", vInp);

        auto setArr = [&](const char *k, const std::vector<double> &vals) {
            napi_value arr = nullptr;
            napi_create_array_with_length(env, vals.size(), &arr);
            for (size_t i = 0; i < vals.size(); ++i) {
                napi_value nv = nullptr;
                napi_create_double(env, vals[i], &nv);
                napi_set_element(env, arr, static_cast<uint32_t>(i), nv);
            }
            napi_set_named_property(env, obj, k, arr);
        };
        setArr("dtMs", ph.dtMs);
        setArr("uiGlMs", ph.uiGlMs);
        setArr("gpuMs", ph.gpuMs);
        setArr("copySwapMs", ph.copySwapMs);
        callBridgeMethod(env, cbObj, "onPerfHud", 1, &obj);
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

bool sendMessageFds(uint8_t kind, const void *payload, uint32_t len, const int *fds, size_t fdCount) {
    if (fdCount == 0 || fds == nullptr) {
        return sendMessage(kind, payload, len, -1);
    }
    if (fdCount > 8) {
        return false;
    }
    std::lock_guard<std::mutex> lk(sendMu);
    if (sock < 0) {
        return false;
    }
    std::vector<uint8_t> msg(5 + len);
    msg[0] = kind;
    memcpy(msg.data() + 1, &len, 4);
    if (len > 0 && payload != nullptr) {
        memcpy(msg.data() + 5, payload, len);
    }

    struct iovec iov;
    iov.iov_base = msg.data();
    iov.iov_len = msg.size();

    const size_t rightsBytes = sizeof(int) * fdCount;
    std::vector<char> cbuf(CMSG_SPACE(rightsBytes), 0);
    struct msghdr mh;
    memset(&mh, 0, sizeof(mh));
    mh.msg_iov = &iov;
    mh.msg_iovlen = 1;
    mh.msg_control = cbuf.data();
    mh.msg_controllen = cbuf.size();
    struct cmsghdr *c = CMSG_FIRSTHDR(&mh);
    c->cmsg_level = SOL_SOCKET;
    c->cmsg_type = SCM_RIGHTS;
    c->cmsg_len = CMSG_LEN(rightsBytes);
    memcpy(CMSG_DATA(c), fds, rightsBytes);

    ssize_t sent = 0;
    for (;;) {
        sent = sendmsg(sock, &mh, MSG_NOSIGNAL);
        if (sent < 0 && errno == EINTR) {
            continue;
        }
        if (sent <= 0) {
            return false;
        }
        break;
    }
    size_t off = static_cast<size_t>(sent);
    while (off < msg.size()) {
        ssize_t n = send(sock, msg.data() + off, msg.size() - off, MSG_NOSIGNAL);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return false;
        }
        if (n == 0) {
            return false;
        }
        off += static_cast<size_t>(n);
    }
    return true;
}

struct OHIPCParcel;

struct IpcParcelApi {
    OHIPCParcel *(*create)() = nullptr;
    void (*destroy)(OHIPCParcel *) = nullptr;
    int (*getDataSize)(const OHIPCParcel *) = nullptr;
    int (*getReadPos)(const OHIPCParcel *) = nullptr;
    int (*rewindReadPos)(OHIPCParcel *, uint32_t) = nullptr;
    int (*readInt32)(OHIPCParcel *, int32_t *) = nullptr;
    int (*readFd)(OHIPCParcel *, int32_t *) = nullptr;
    int32_t (*nbWriteToParcel)(OH_NativeBuffer *, OHIPCParcel *) = nullptr;
    bool ok = false;
};

const IpcParcelApi &getIpcParcelApi() {
    static IpcParcelApi api = []() {
        IpcParcelApi a;
        void *ipcLib = dlopen("libipc_capi.so", RTLD_NOW);
        void *nbLib = dlopen("libnative_buffer.so", RTLD_NOW);
        if (ipcLib != nullptr && nbLib != nullptr) {
            a.create = reinterpret_cast<decltype(a.create)>(dlsym(ipcLib, "OH_IPCParcel_Create"));
            a.destroy = reinterpret_cast<decltype(a.destroy)>(dlsym(ipcLib, "OH_IPCParcel_Destroy"));
            a.getDataSize = reinterpret_cast<decltype(a.getDataSize)>(dlsym(ipcLib, "OH_IPCParcel_GetDataSize"));
            a.getReadPos = reinterpret_cast<decltype(a.getReadPos)>(dlsym(ipcLib, "OH_IPCParcel_GetReadPosition"));
            a.rewindReadPos = reinterpret_cast<decltype(a.rewindReadPos)>(dlsym(ipcLib, "OH_IPCParcel_RewindReadPosition"));
            a.readInt32 = reinterpret_cast<decltype(a.readInt32)>(dlsym(ipcLib, "OH_IPCParcel_ReadInt32"));
            a.readFd = reinterpret_cast<decltype(a.readFd)>(dlsym(ipcLib, "OH_IPCParcel_ReadFileDescriptor"));
            a.nbWriteToParcel = reinterpret_cast<decltype(a.nbWriteToParcel)>(dlsym(nbLib, "OH_NativeBuffer_WriteToParcel"));
            a.ok = a.create && a.destroy && a.getDataSize && a.getReadPos &&
                   a.rewindReadPos && a.readInt32 && a.readFd && a.nbWriteToParcel;
        }
        return a;
    }();
    return api;
}

bool envTruthy(const char *val) {
    if (val == nullptr || val[0] == '\0') {
        return false;
    }
    return strcmp(val, "1") == 0 || strcasecmp(val, "true") == 0 ||
           strcasecmp(val, "yes") == 0 || strcasecmp(val, "on") == 0;
}

bool wantGpuSwapchain() {
    if (envTruthy(getenv("MYGO_SOFTWARE"))) {
        return false;
    }
    const char *gpuEnv = getenv("MYGO_GPU");
    if (gpuEnv != nullptr && (strcmp(gpuEnv, "0") == 0 || strcasecmp(gpuEnv, "false") == 0 || strcasecmp(gpuEnv, "off") == 0)) {
        return false;
    }
    if (access("/system/lib64/libEGL.so", R_OK) != 0 || access("/system/lib64/libGLESv3.so", R_OK) != 0) {
        return false;
    }
    return getIpcParcelApi().ok;
}

bool sendGpuBuffer(uint32_t winId, uint32_t bufId, int32_t w, int32_t h, OH_NativeBuffer *nb) {
    const auto &api = getIpcParcelApi();
    if (!api.ok || nb == nullptr || w <= 0 || h <= 0) {
        return false;
    }
    OHIPCParcel *p = api.create();
    if (p == nullptr) {
        return false;
    }
    if (api.nbWriteToParcel(nb, p) != 0) {
        api.destroy(p);
        return false;
    }
    int dataSize = api.getDataSize(p);
    if (dataSize <= 0 || dataSize > 4096) {
        api.destroy(p);
        return false;
    }
    std::vector<uint8_t> isFd;
    std::vector<int32_t> ints;
    std::vector<int> fds;
    isFd.reserve(96);
    ints.reserve(96);
    while (api.getReadPos(p) < dataSize) {
        int pos = api.getReadPos(p);
        int32_t v = 0;
        if (api.readInt32(p, &v) == 0) {
            isFd.push_back(0);
            ints.push_back(v);
            continue;
        }
        api.rewindReadPos(p, static_cast<uint32_t>(pos));
        int32_t fd = -1;
        if (api.readFd(p, &fd) == 0 && fd >= 0) {
            isFd.push_back(1);
            fds.push_back(static_cast<int>(fd));
            continue;
        }
        for (int cfd : fds) {
            close(cfd);
        }
        api.destroy(p);
        return false;
    }
    api.destroy(p);

    const size_t tc = isFd.size();
    if (tc == 0 || tc > 1024 || fds.size() > 8) {
        for (int cfd : fds) {
            close(cfd);
        }
        return false;
    }
    const size_t maskBytes = (tc + 7) / 8;
    std::vector<uint8_t> wire(20 + maskBytes + ints.size() * 4, 0);
    uint32_t uWin = winId;
    uint32_t uBuf = bufId;
    uint32_t uW = static_cast<uint32_t>(w);
    uint32_t uH = static_cast<uint32_t>(h);
    uint32_t uTc = static_cast<uint32_t>(tc);
    memcpy(wire.data() + 0, &uWin, 4);
    memcpy(wire.data() + 4, &uBuf, 4);
    memcpy(wire.data() + 8, &uW, 4);
    memcpy(wire.data() + 12, &uH, 4);
    memcpy(wire.data() + 16, &uTc, 4);
    for (size_t i = 0; i < tc; ++i) {
        if (isFd[i]) {
            wire[20 + (i >> 3)] |= static_cast<uint8_t>(1u << (i & 7u));
        }
    }
    if (!ints.empty()) {
        memcpy(wire.data() + 20 + maskBytes, ints.data(), ints.size() * 4);
    }
    bool sent = sendMessageFds(kGpuBuffer, wire.data(), static_cast<uint32_t>(wire.size()), fds.data(), fds.size());
    for (int cfd : fds) {
        close(cfd);
    }
    return sent;
}

int64_t queryFenceSignalNs(int fd) {
    struct sync_file_info info;
    memset(&info, 0, sizeof(info));
    if (ioctl(fd, SYNC_IOC_FILE_INFO, &info) != 0 || info.status <= 0 || info.num_fences == 0 || info.num_fences > 16) {
        return 0;
    }
    std::vector<struct sync_fence_info> fences(info.num_fences);
    info.sync_fence_info = reinterpret_cast<uintptr_t>(fences.data());
    if (ioctl(fd, SYNC_IOC_FILE_INFO, &info) != 0) {
        return 0;
    }
    uint64_t maxTs = 0;
    for (uint32_t i = 0; i < info.num_fences; ++i) {
        if (fences[i].status > 0 && fences[i].timestamp_ns > maxTs) {
            maxTs = fences[i].timestamp_ns;
        }
    }
    return static_cast<int64_t>(maxTs);
}

bool sendSurface(uint32_t winId, int32_t w, int32_t h, double scale, double hz, bool fresh = false) {
    uint32_t hdr[5] = {
        winId,
        static_cast<uint32_t>(w),
        static_cast<uint32_t>(h),
        static_cast<uint32_t>(std::lround(scale * 1000.0)),
        static_cast<uint32_t>(std::lround(hz * 1000.0)),
    };
    uint8_t wire[21] = {0};
    memcpy(wire, hdr, sizeof(hdr));
    wire[20] = fresh ? 1 : 0;
    return sendMessage(kSurface, wire, sizeof(wire), -1);
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

bool isPerfHudAvailable() {
    if (gPerfHudAvailable.load(std::memory_order_relaxed)) {
        return true;
    }
    static const bool fromEnv = envTruthy(getenv("MYGO_PERF_HUD"));
    return fromEnv;
}

void markInputArrival(WinSession *ws) {
    if (ws != nullptr && gPerfHudEnabled.load(std::memory_order_relaxed)) {
        ws->lastInputNs.store(monoNs(), std::memory_order_relaxed);
        ws->inputSincePresent.store(true, std::memory_order_relaxed);
    }
}

// wakeRenderForInput arms hardware VSync and starts swapchain buffer prefetch
// in parallel right after kInput is sent to Go so the first interactive frame
// dispatches with zero VSync or RequestBuffer delay.
void wakeRenderForInput(WinSession *ws) {
    if (ws == nullptr) {
        return;
    }
    bool notify = false;
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        if (ws->renderRunning && !ws->stopRender) {
            ws->ensureVsyncArmedLocked();
            if (!ws->inputPrefetch && !ws->frameInFlight) {
                ws->inputPrefetch = true;
                notify = true;
            }
        }
    }
    if (notify) {
        ws->cv.notify_all();
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
    wakeRenderForInput(ws);
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
    wakeRenderForInput(ws);
}

void sendScrollInput(WinSession *ws, uint8_t flags, uint32_t mods, float x, float y, float dx, float dy) {
    if (ws == nullptr) {
        return;
    }
    if (gPerfHudEnabled.load(std::memory_order_relaxed)) {
        const int64_t now = monoNs();
        ws->lastInputNs.store(now, std::memory_order_relaxed);
        ws->inputSincePresent.store(true, std::memory_order_relaxed);
        std::lock_guard<std::mutex> lk(ws->perfMu);
        if (ws->lastScrollInputNs > 0) {
            double dt = static_cast<double>(now - ws->lastScrollInputNs) / 1e6;
            if (dt > 400.0) {
                ws->scrollDtMs.clear();
            } else if (dt >= 0.4) {
                if (ws->scrollDtMs.size() >= 120) {
                    ws->scrollDtMs.pop_front();
                }
                ws->scrollDtMs.push_back(dt);
            }
        }
        ws->lastScrollInputNs = now;
    }
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
    wakeRenderForInput(ws);
}

void setPerfHudEnabled(uint32_t winId, int mode) {
    gPerfHudAvailable.store(true, std::memory_order_relaxed);
    bool prev = gPerfHudEnabled.load(std::memory_order_relaxed);
    bool next = (mode < 0) ? !prev : (mode != 0);
    gPerfHudEnabled.store(next, std::memory_order_relaxed);
    uint32_t targetId = winId > 0 ? winId : 1;
    auto ws = getOrCreateSession(targetId);
    if (!ws) {
        return;
    }
    PerfHudData snap;
    int32_t w = 0, h = 0;
    double sc = 1.0, hz = 120.0;
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        w = ws->curW > 0 ? ws->curW : ws->targetW;
        h = ws->curH > 0 ? ws->curH : ws->targetH;
        sc = ws->scale;
        hz = ws->hz;
        ws->forcePerfPush = true;
    }
    {
        std::lock_guard<std::mutex> lk(ws->perfMu);
        snap = ws->lastPerfSnapshot;
        snap.winId = ws->winId;
        snap.visible = next;
        if (snap.headerLine.empty()) {
            char hdr[160];
            snprintf(hdr, sizeof(hdr), "FPS --.-  (1%%L --.-)  |  %.0fHz · %dx%d @%.2fx  [F12]",
                     hz, w, h, sc);
            snap.headerLine = hdr;
            snap.pacingLine = "Frame dt  waiting for active frames...";
            snap.stageLine = "Stages    UI+GL -- | GPU -- | Copy -- | Swap -- | VSync --";
            snap.inputLine = "Input     Scroll --Hz  |  Whole --%  Abort 0";
        }
        ws->lastPerfSnapshot = snap;
    }
    auto bev = std::make_unique<BridgeEvent>();
    bev->kind = BridgeEventKind::PerfHud;
    bev->perfHud = std::move(snap);
    postBridgeEvent(std::move(bev));
    ws->cv.notify_all();
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
    wakeRenderForInput(ws);
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
    wakeRenderForInput(ws.get());
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

void unmountWindowXComponent(const std::shared_ptr<WinSession> &ws, bool eraseTree);
bool mountWindowXComponent(const std::shared_ptr<WinSession> &ws, bool layered);

void enableWebLayerOnUI(void *userData) {
    uint32_t winId = static_cast<uint32_t>(reinterpret_cast<uintptr_t>(userData));
    auto ws = findSession(winId);
    if (!ws || ws->xcNode == nullptr || nodeAPI == nullptr) {
        return;
    }
    if (!ws->xcIsTexture && ws->nodeContent != nullptr) {
        unmountWindowXComponent(ws, false);
        mountWindowXComponent(ws, true);
        return;
    }
    ArkUI_NumberValue bgVal[] = {{.u32 = 0x00000000u}};
    ArkUI_AttributeItem bgItem = {bgVal, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_BACKGROUND_COLOR, &bgItem);
    if (ws->surfaceHolder != nullptr) {
        ArkUI_XComponentSurfaceConfig *cfg = OH_ArkUI_XComponentSurfaceConfig_Create();
        if (cfg != nullptr) {
            OH_ArkUI_XComponentSurfaceConfig_SetIsOpaque(cfg, false);
            OH_ArkUI_SurfaceHolder_SetSurfaceConfig(ws->surfaceHolder, cfg);
            OH_ArkUI_XComponentSurfaceConfig_Dispose(cfg);
        }
    }
}

void focusSurfaceOnUI(void *userData) {
    uint32_t winId = static_cast<uint32_t>(reinterpret_cast<uintptr_t>(userData));
    auto ws = findSession(winId);
    if (ws && ws->xcNode != nullptr) {
        OH_ArkUI_FocusRequest(ws->xcNode);
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

// ---------- Batch 7: Asset Store Secrets, Global Hotkeys, Drag & Drop, Accessibility ----------

std::mutex gSecretExecMu;

std::string makeSecretAlias(const std::string &service, const std::string &account) {
    std::string raw = service;
    raw.push_back('\x1f');
    raw += account;
    if (raw.empty()) {
        return "_";
    }
    if (raw.size() <= 240) {
        return raw;
    }
    uint64_t h = 14695981039346656037ULL;
    for (unsigned char c : raw) {
        h ^= static_cast<uint64_t>(c);
        h *= 1099511628211ULL;
    }
    char hex[20];
    snprintf(hex, sizeof(hex), "#%016llx", static_cast<unsigned long long>(h));
    return raw.substr(0, 200) + hex;
}

void sendSecretResWire(uint32_t reqId, bool notFound, const std::string &errStr, const std::vector<uint8_t> &secret) {
    std::vector<uint8_t> wire(8, 0);
    memcpy(wire.data() + 0, &reqId, 4);
    wire[4] = notFound ? 1 : 0;
    appendStr(wire, errStr);
    appendBytes(wire, secret.data(), secret.size());
    sendMessage(kSecretRes, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void handleSecretReq(const std::vector<uint8_t> &p) {
    if (p.size() < 8) {
        return;
    }
    uint32_t reqId = 0;
    memcpy(&reqId, p.data() + 0, 4);
    uint8_t op = p[4];
    size_t off = 8;
    std::string service, account;
    std::vector<uint8_t> secret;
    if (!readStr(p, off, &service) || !readStr(p, off, &account) ||
        !readBytes(p, off, &secret) || off != p.size()) {
        sendSecretResWire(reqId, false, "ohos: malformed secret request", {});
        return;
    }

    std::thread([reqId, op, service = std::move(service), account = std::move(account),
                 secret = std::move(secret)]() mutable {
        std::lock_guard<std::mutex> lk(gSecretExecMu);
        std::string alias = makeSecretAlias(service, account);

        if (op == 1) { // set
            Asset_Attr delAttr{};
            delAttr.tag = ASSET_TAG_ALIAS;
            delAttr.value.blob.size = static_cast<uint32_t>(alias.size());
            delAttr.value.blob.data = reinterpret_cast<uint8_t *>(alias.data());
            (void)OH_Asset_Remove(&delAttr, 1);

            std::vector<uint8_t> blob(1 + secret.size(), 0x01);
            if (!secret.empty()) {
                memcpy(blob.data() + 1, secret.data(), secret.size());
            }
            Asset_Attr attrs[3]{};
            attrs[0].tag = ASSET_TAG_ALIAS;
            attrs[0].value.blob.size = static_cast<uint32_t>(alias.size());
            attrs[0].value.blob.data = reinterpret_cast<uint8_t *>(alias.data());
            attrs[1].tag = ASSET_TAG_SECRET;
            attrs[1].value.blob.size = static_cast<uint32_t>(blob.size());
            attrs[1].value.blob.data = blob.data();
            attrs[2].tag = ASSET_TAG_ACCESSIBILITY;
            attrs[2].value.u32 = ASSET_ACCESSIBILITY_DEVICE_POWERED_ON;

            int32_t rc = OH_Asset_Add(attrs, 3);
            if (rc == ASSET_SUCCESS) {
                sendSecretResWire(reqId, false, "", {});
            } else {
                sendSecretResWire(reqId, false, "ohos: OH_Asset_Add failed (" + std::to_string(rc) + ")", {});
            }
            return;
        }

        if (op == 2) { // get
            Asset_Attr query[2]{};
            query[0].tag = ASSET_TAG_ALIAS;
            query[0].value.blob.size = static_cast<uint32_t>(alias.size());
            query[0].value.blob.data = reinterpret_cast<uint8_t *>(alias.data());
            query[1].tag = ASSET_TAG_RETURN_TYPE;
            query[1].value.u32 = ASSET_RETURN_ALL;

            Asset_ResultSet rset{0, nullptr};
            int32_t rc = OH_Asset_Query(query, 2, &rset);
            if (rc == ASSET_NOT_FOUND || (rc == ASSET_SUCCESS && rset.count == 0)) {
                if (rset.results != nullptr) {
                    OH_Asset_FreeResultSet(&rset);
                }
                sendSecretResWire(reqId, true, "", {});
                return;
            }
            if (rc != ASSET_SUCCESS) {
                if (rset.results != nullptr) {
                    OH_Asset_FreeResultSet(&rset);
                }
                sendSecretResWire(reqId, false, "ohos: OH_Asset_Query failed (" + std::to_string(rc) + ")", {});
                return;
            }
            std::vector<uint8_t> out;
            Asset_Attr *sec = OH_Asset_ParseAttr(rset.results, ASSET_TAG_SECRET);
            if (sec != nullptr && sec->value.blob.data != nullptr && sec->value.blob.size >= 1) {
                const uint8_t *raw = sec->value.blob.data;
                uint32_t sz = sec->value.blob.size;
                if (raw[0] == 0x01) {
                    out.assign(raw + 1, raw + sz);
                } else {
                    out.assign(raw, raw + sz);
                }
            }
            OH_Asset_FreeResultSet(&rset);
            sendSecretResWire(reqId, false, "", out);
            return;
        }

        if (op == 3) { // delete
            Asset_Attr query{};
            query.tag = ASSET_TAG_ALIAS;
            query.value.blob.size = static_cast<uint32_t>(alias.size());
            query.value.blob.data = reinterpret_cast<uint8_t *>(alias.data());
            int32_t rc = OH_Asset_Remove(&query, 1);
            if (rc == ASSET_SUCCESS || rc == ASSET_NOT_FOUND) {
                sendSecretResWire(reqId, false, "", {});
            } else {
                sendSecretResWire(reqId, false, "ohos: OH_Asset_Remove failed (" + std::to_string(rc) + ")", {});
            }
            return;
        }

        sendSecretResWire(reqId, false, "ohos: unknown secret op", {});
    }).detach();
}

struct HotkeyEntry {
    int32_t id = 0;
    int32_t finalKey = 0;
    std::vector<int32_t> preKeys;
    Input_Hotkey *handle = nullptr;
};

std::mutex gHotkeyMu;
std::vector<HotkeyEntry> gHotkeys;

void OnHotkeyTriggered(Input_Hotkey *hotkey) {
    int32_t matchedId = 0;
    bool found = false;
    {
        std::lock_guard<std::mutex> lk(gHotkeyMu);
        for (const auto &e : gHotkeys) {
            if (e.handle == hotkey) {
                matchedId = e.id;
                found = true;
                break;
            }
        }
        if (!found && hotkey != nullptr) {
            int32_t fk = 0;
            if (OH_Input_GetFinalKey(hotkey, &fk) == INPUT_SUCCESS) {
                for (const auto &e : gHotkeys) {
                    if (e.finalKey == fk) {
                        matchedId = e.id;
                        found = true;
                        break;
                    }
                }
            }
        }
    }
    if (found) {
        uint8_t wire[4];
        memcpy(wire, &matchedId, 4);
        sendMessage(kHotkeyTrig, wire, 4, -1);
    }
}

void clearAllHotkeys() {
    std::vector<HotkeyEntry> old;
    {
        std::lock_guard<std::mutex> lk(gHotkeyMu);
        old.swap(gHotkeys);
    }
    for (auto &e : old) {
        if (e.handle != nullptr) {
            (void)OH_Input_RemoveHotkeyMonitor(e.handle, OnHotkeyTriggered);
            OH_Input_DestroyHotkey(&e.handle);
        }
    }
}

void sendHotkeyResWire(uint32_t reqId, const std::string &errStr) {
    if (reqId == 0) {
        return;
    }
    std::vector<uint8_t> wire;
    appendU32(wire, reqId);
    appendStr(wire, errStr);
    sendMessage(kHotkeyRes, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void handleHotkeyReq(const std::vector<uint8_t> &p) {
    if (p.size() < 20) {
        return;
    }
    uint32_t reqId = 0, preCount = 0;
    int32_t hotkeyId = 0, finalKey = 0;
    memcpy(&reqId, p.data() + 0, 4);
    memcpy(&hotkeyId, p.data() + 4, 4);
    bool reg = p[8] != 0;
    memcpy(&finalKey, p.data() + 12, 4);
    memcpy(&preCount, p.data() + 16, 4);
    if (preCount > 8 || 20u + preCount * 4u != p.size()) {
        sendHotkeyResWire(reqId, "ohos: malformed hotkey request");
        return;
    }
    std::vector<int32_t> preKeys(preCount);
    for (uint32_t i = 0; i < preCount; ++i) {
        memcpy(&preKeys[i], p.data() + 20 + i * 4, 4);
    }

    {
        std::lock_guard<std::mutex> lk(gHotkeyMu);
        for (auto it = gHotkeys.begin(); it != gHotkeys.end(); ) {
            if (it->id == hotkeyId) {
                if (it->handle != nullptr) {
                    (void)OH_Input_RemoveHotkeyMonitor(it->handle, OnHotkeyTriggered);
                    OH_Input_DestroyHotkey(&it->handle);
                }
                it = gHotkeys.erase(it);
            } else {
                ++it;
            }
        }
        if (!reg) {
            return;
        }
        Input_Hotkey *hk = OH_Input_CreateHotkey();
        if (hk == nullptr) {
            sendHotkeyResWire(reqId, "ohos: OH_Input_CreateHotkey failed");
            return;
        }
        if (!preKeys.empty()) {
            OH_Input_SetPreKeys(hk, preKeys.data(), static_cast<int32_t>(preKeys.size()));
        }
        OH_Input_SetFinalKey(hk, finalKey);
        OH_Input_SetRepeat(hk, false);
        Input_Result rc = OH_Input_AddHotkeyMonitor(hk, OnHotkeyTriggered);
        if (rc != INPUT_SUCCESS) {
            OH_Input_DestroyHotkey(&hk);
            sendHotkeyResWire(reqId, "ohos: hotkey unavailable or conflict (" + std::to_string(static_cast<int>(rc)) + ")");
            return;
        }
        HotkeyEntry entry;
        entry.id = hotkeyId;
        entry.finalKey = finalKey;
        entry.preKeys = std::move(preKeys);
        entry.handle = hk;
        gHotkeys.push_back(std::move(entry));
    }
    sendHotkeyResWire(reqId, "");
}

struct HostDragRep {
    std::string format;
    std::vector<uint8_t> data;
};

struct ActiveHostDrag {
    bool active = false;
    uint32_t srcWinId = 0;
    uint32_t ops = 1;
    std::string session;
    std::vector<HostDragRep> reps;
};

struct PendingDragReply {
    bool ready = false;
    uint32_t op = 0;
    bool taken = false;
};

std::mutex gDragMu;
ActiveHostDrag gActiveHostDrag;

std::mutex gDragReplyMu;
std::condition_variable gDragReplyCv;
uint32_t gNextDragPendingId = 0;
std::unordered_map<uint32_t, PendingDragReply> gDragReplies;

void sendDragEventWire(uint32_t winId, uint8_t ev, bool canceled, bool escaped, uint32_t pendingId,
                       uint32_t ops, uint32_t suggestedOp, double x, double y,
                       const std::string &session, const std::string &errStr,
                       const std::vector<std::string> &formats, const std::vector<HostDragRep> &reps) {
    std::vector<uint8_t> wire(36, 0);
    memcpy(wire.data() + 0, &winId, 4);
    wire[4] = ev;
    wire[5] = canceled ? 1 : 0;
    wire[6] = escaped ? 1 : 0;
    memcpy(wire.data() + 8, &pendingId, 4);
    memcpy(wire.data() + 12, &ops, 4);
    memcpy(wire.data() + 16, &suggestedOp, 4);
    memcpy(wire.data() + 20, &x, 8);
    memcpy(wire.data() + 28, &y, 8);
    appendStr(wire, session);
    appendStr(wire, errStr);
    appendU32(wire, static_cast<uint32_t>(formats.size()));
    for (const auto &f : formats) {
        appendStr(wire, f);
    }
    appendU32(wire, static_cast<uint32_t>(reps.size()));
    for (const auto &r : reps) {
        appendStr(wire, r.format);
        appendBytes(wire, r.data.data(), r.data.size());
    }
    sendMessage(kDragEvent, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void handleDragReply(const std::vector<uint8_t> &p) {
    if (p.size() != 16) {
        return;
    }
    uint32_t pendingId = 0, op = 0;
    memcpy(&pendingId, p.data() + 4, 4);
    memcpy(&op, p.data() + 8, 4);
    bool taken = p[12] != 0;
    if (pendingId == 0) {
        return;
    }
    {
        std::lock_guard<std::mutex> lk(gDragReplyMu);
        auto it = gDragReplies.find(pendingId);
        if (it != gDragReplies.end()) {
            it->second.ready = true;
            it->second.op = op;
            it->second.taken = taken;
        }
    }
    gDragReplyCv.notify_all();
}

void disposeActiveDragAction(WinSession *ws) {
    if (ws != nullptr && ws->activeDragAction != nullptr) {
        ArkUI_DragAction *act = ws->activeDragAction;
        ws->activeDragAction = nullptr;
        ws->activeDragCanceled = false;
        OH_ArkUI_DragAction_UnregisterStatusListener(act);
        OH_ArkUI_DragAction_Dispose(act);
    }
}

void OnDragActionStatus(ArkUI_DragAndDropInfo *info, void *userData) {
    if (info == nullptr) {
        return;
    }
    ArkUI_DragStatus st = OH_ArkUI_DragAndDropInfo_GetDragStatus(info);
    if (st != ARKUI_DRAG_STATUS_ENDED) {
        return;
    }
    uint32_t winId = static_cast<uint32_t>(reinterpret_cast<uintptr_t>(userData));
    ArkUI_DragEvent *dev = OH_ArkUI_DragAndDropInfo_GetDragEvent(info);
    ArkUI_DragResult dres = ARKUI_DRAG_RESULT_CANCELED;
    ArkUI_DropOperation dop = ARKUI_DROP_OPERATION_COPY;
    if (dev != nullptr) {
        OH_ArkUI_DragEvent_GetDragResult(dev, &dres);
        OH_ArkUI_DragEvent_GetDropOperation(dev, &dop);
    }
    {
        std::lock_guard<std::mutex> lk(gDragMu);
        if (gActiveHostDrag.srcWinId == winId) {
            gActiveHostDrag = ActiveHostDrag{};
        }
    }
    bool wasCanceled = false;
    if (auto ws = findSession(winId)) {
        wasCanceled = ws->activeDragCanceled;
        disposeActiveDragAction(ws.get());
    }
    uint32_t ops = 0;
    bool canceled = false;
    bool escaped = false;
    if (!wasCanceled && dres == ARKUI_DRAG_RESULT_SUCCESSFUL) {
        ops = (dop == ARKUI_DROP_OPERATION_MOVE) ? 2u : 1u;
    } else {
        canceled = true;
        escaped = wasCanceled || (dres == ARKUI_DRAG_RESULT_CANCELED);
    }
    sendDragEventWire(winId, 5, canceled, escaped, 0, ops, 0, 0.0, 0.0, "", "", {}, {});
}

struct DragStartTask {
    uint32_t winId = 0;
    uint32_t ops = 1;
    int32_t width = 0;
    int32_t height = 0;
    int32_t hotX = 0;
    int32_t hotY = 0;
    std::string session;
    std::vector<uint8_t> preview;
    std::vector<HostDragRep> reps;
};

OH_PixelmapNative *createDragPreviewPixelmap(std::vector<uint8_t> &preview, int32_t w, int32_t h) {
    OH_Pixelmap_InitializationOptions *opts = nullptr;
    if (OH_PixelmapInitializationOptions_Create(&opts) != IMAGE_SUCCESS || opts == nullptr) {
        return nullptr;
    }
    uint8_t fallback[4] = {0, 0, 0, 0};
    uint8_t *srcPtr = fallback;
    size_t srcLen = sizeof(fallback);
    uint32_t pw = 1, ph = 1;
    if (w > 0 && h > 0 && preview.size() >= static_cast<size_t>(w) * static_cast<size_t>(h) * 4u) {
        pw = static_cast<uint32_t>(w);
        ph = static_cast<uint32_t>(h);
        srcPtr = preview.data();
        srcLen = static_cast<size_t>(w) * static_cast<size_t>(h) * 4u;
    }
    OH_PixelmapInitializationOptions_SetWidth(opts, pw);
    OH_PixelmapInitializationOptions_SetHeight(opts, ph);
    OH_PixelmapInitializationOptions_SetSrcPixelFormat(opts, PIXEL_FORMAT_BGRA_8888);
    OH_PixelmapInitializationOptions_SetPixelFormat(opts, PIXEL_FORMAT_BGRA_8888);
    OH_PixelmapInitializationOptions_SetAlphaType(opts, PIXELMAP_ALPHA_TYPE_PREMULTIPLIED);
    OH_PixelmapNative *pm = nullptr;
    (void)OH_PixelmapNative_CreatePixelmap(srcPtr, srcLen, opts, &pm);
    OH_PixelmapInitializationOptions_Release(opts);
    return pm;
}

void RunStartDragUITask(void *data) {
    std::unique_ptr<DragStartTask> task(static_cast<DragStartTask *>(data));
    if (!task) {
        return;
    }
    auto ws = findSession(task->winId);
    if (!ws || ws->xcNode == nullptr) {
        {
            std::lock_guard<std::mutex> lk(gDragMu);
            if (gActiveHostDrag.srcWinId == task->winId) {
                gActiveHostDrag = ActiveHostDrag{};
            }
        }
        sendDragEventWire(task->winId, 5, false, false, 0, 0, 0, 0.0, 0.0, "",
                          "ohos: window closed before drag started", {}, {});
        return;
    }
    disposeActiveDragAction(ws.get());

    ArkUI_DragAction *act = OH_ArkUI_CreateDragActionWithNode(ws->xcNode);
    if (act == nullptr) {
        {
            std::lock_guard<std::mutex> lk(gDragMu);
            if (gActiveHostDrag.srcWinId == task->winId) {
                gActiveHostDrag = ActiveHostDrag{};
            }
        }
        sendDragEventWire(task->winId, 5, false, false, 0, 0, 0, 0.0, 0.0, "",
                          "ohos: OH_ArkUI_CreateDragActionWithNode failed", {}, {});
        return;
    }

    OH_ArkUI_DragAction_SetPointerId(act, 0);
    OH_PixelmapNative *pm = createDragPreviewPixelmap(task->preview, task->width, task->height);
    if (pm != nullptr) {
        OH_PixelmapNative *pms[1] = {pm};
        OH_ArkUI_DragAction_SetPixelMaps(act, pms, 1);
    }
    OH_ArkUI_DragAction_SetTouchPointX(act, static_cast<float>(std::max(0, task->hotX)));
    OH_ArkUI_DragAction_SetTouchPointY(act, static_cast<float>(std::max(0, task->hotY)));

    OH_UdmfData *ud = OH_UdmfData_Create();
    if (ud != nullptr) {
        OH_UdmfRecord *rec = OH_UdmfRecord_Create();
        bool recHasEntry = false;
        uint32_t addedRecords = 0;
        for (auto &rep : task->reps) {
            if (rep.format == "text/plain") {
                if (rec != nullptr) {
                    OH_UdsPlainText *pt = OH_UdsPlainText_Create();
                    if (pt != nullptr) {
                        std::string s(reinterpret_cast<const char *>(rep.data.data()), rep.data.size());
                        OH_UdsPlainText_SetContent(pt, s.c_str());
                        OH_UdmfRecord_AddPlainText(rec, pt);
                        OH_UdsPlainText_Destroy(pt);
                        recHasEntry = true;
                    }
                }
            } else if (rep.format == "text/html") {
                if (rec != nullptr) {
                    OH_UdsHtml *ht = OH_UdsHtml_Create();
                    if (ht != nullptr) {
                        std::string s(reinterpret_cast<const char *>(rep.data.data()), rep.data.size());
                        OH_UdsHtml_SetContent(ht, s.c_str());
                        OH_UdmfRecord_AddHtml(rec, ht);
                        OH_UdsHtml_Destroy(ht);
                        recHasEntry = true;
                    }
                }
            } else if (rep.format == "text/uri-list") {
                std::string all(reinterpret_cast<const char *>(rep.data.data()), rep.data.size());
                size_t pos = 0;
                while (pos < all.size()) {
                    size_t nl = all.find('\n', pos);
                    std::string line = (nl == std::string::npos) ? all.substr(pos) : all.substr(pos, nl - pos);
                    pos = (nl == std::string::npos) ? all.size() : nl + 1;
                    if (!line.empty() && line.back() == '\r') {
                        line.pop_back();
                    }
                    if (line.empty() || line[0] == '#') {
                        continue;
                    }
                    if (line[0] == '/') {
                        line = "file://" + line;
                    }
                    OH_UdmfRecord *frec = OH_UdmfRecord_Create();
                    OH_UdsFileUri *fu = OH_UdsFileUri_Create();
                    if (frec != nullptr && fu != nullptr) {
                        OH_UdsFileUri_SetFileUri(fu, line.c_str());
                        OH_UdsFileUri_SetFileType(fu, "general.file");
                        OH_UdmfRecord_AddFileUri(frec, fu);
                        OH_UdmfData_AddRecord(ud, frec);
                        ++addedRecords;
                    }
                    if (fu != nullptr) {
                        OH_UdsFileUri_Destroy(fu);
                    }
                    if (frec != nullptr) {
                        OH_UdmfRecord_Destroy(frec);
                    }
                }
            } else if (rec != nullptr && !rep.format.empty()) {
                unsigned char dummy = 0;
                unsigned char *ptr = rep.data.empty() ? &dummy : rep.data.data();
                OH_UdmfRecord_AddGeneralEntry(rec, rep.format.c_str(), ptr, static_cast<unsigned int>(rep.data.size()));
                recHasEntry = true;
            }
        }
        if (rec != nullptr) {
            if (!recHasEntry && addedRecords == 0) {
                OH_UdsPlainText *pt = OH_UdsPlainText_Create();
                if (pt != nullptr) {
                    OH_UdsPlainText_SetContent(pt, "");
                    OH_UdmfRecord_AddPlainText(rec, pt);
                    OH_UdsPlainText_Destroy(pt);
                    recHasEntry = true;
                }
            }
            if (recHasEntry) {
                OH_UdmfData_AddRecord(ud, rec);
            }
            OH_UdmfRecord_Destroy(rec);
        }
        OH_ArkUI_DragAction_SetData(act, ud);
        OH_UdmfData_Destroy(ud);
    }

    ArkUI_DragPreviewOption *opt = OH_ArkUI_CreateDragPreviewOption();
    if (opt != nullptr) {
        OH_ArkUI_DragPreviewOption_SetScaleMode(opt, ARKUI_DRAG_PREVIEW_SCALE_DISABLED);
        OH_ArkUI_DragPreviewOption_SetDefaultShadowEnabled(opt, true);
        OH_ArkUI_DragAction_SetDragPreviewOption(act, opt);
        OH_ArkUI_DragPreviewOption_Dispose(opt);
    }

    ws->activeDragAction = act;
    ws->activeDragCanceled = false;
    OH_ArkUI_DragAction_RegisterStatusListener(act, reinterpret_cast<void *>(static_cast<uintptr_t>(ws->winId)),
                                               OnDragActionStatus);
    int32_t rc = OH_ArkUI_StartDrag(act);
    if (pm != nullptr) {
        OH_PixelmapNative_Release(pm);
    }
    if (rc != ARKUI_ERROR_CODE_NO_ERROR) {
        disposeActiveDragAction(ws.get());
        {
            std::lock_guard<std::mutex> lk(gDragMu);
            if (gActiveHostDrag.srcWinId == task->winId) {
                gActiveHostDrag = ActiveHostDrag{};
            }
        }
        sendDragEventWire(task->winId, 5, false, false, 0, 0, 0, 0.0, 0.0, "",
                          "ohos: OH_ArkUI_StartDrag failed (" + std::to_string(rc) + ")", {}, {});
    }
}

void handleDragStart(const std::vector<uint8_t> &p) {
    if (p.size() < 40) {
        return;
    }
    auto task = std::make_unique<DragStartTask>();
    memcpy(&task->winId, p.data() + 0, 4);
    memcpy(&task->ops, p.data() + 4, 4);
    memcpy(&task->width, p.data() + 24, 4);
    memcpy(&task->height, p.data() + 28, 4);
    memcpy(&task->hotX, p.data() + 32, 4);
    memcpy(&task->hotY, p.data() + 36, 4);
    size_t off = 40;
    uint32_t repCount = 0;
    if (!readStr(p, off, &task->session) || !readBytes(p, off, &task->preview) ||
        !readU32(p, off, &repCount) || repCount > 128) {
        return;
    }
    task->reps.reserve(repCount);
    for (uint32_t i = 0; i < repCount; ++i) {
        HostDragRep r;
        if (!readStr(p, off, &r.format) || !readBytes(p, off, &r.data)) {
            return;
        }
        task->reps.push_back(std::move(r));
    }
    if (off != p.size()) {
        return;
    }

    auto ws = findSession(task->winId);
    if (!ws || ws->uiContext == nullptr || ws->xcNode == nullptr) {
        sendDragEventWire(task->winId, 5, false, false, 0, 0, 0, 0.0, 0.0, "",
                          "ohos: surface unavailable for drag", {}, {});
        return;
    }
    {
        std::lock_guard<std::mutex> lk(gDragMu);
        gActiveHostDrag.active = true;
        gActiveHostDrag.srcWinId = task->winId;
        gActiveHostDrag.ops = task->ops != 0 ? task->ops : 1u;
        gActiveHostDrag.session = task->session;
        gActiveHostDrag.reps = task->reps;
    }
    DragStartTask *raw = task.release();
    if (OH_ArkUI_PostUITask(ws->uiContext, raw, RunStartDragUITask) != ARKUI_ERROR_CODE_NO_ERROR) {
        uint32_t wid = raw->winId;
        delete raw;
        {
            std::lock_guard<std::mutex> lk(gDragMu);
            if (gActiveHostDrag.srcWinId == wid) {
                gActiveHostDrag = ActiveHostDrag{};
            }
        }
        sendDragEventWire(wid, 5, false, false, 0, 0, 0, 0.0, 0.0, "",
                          "ohos: PostUITask failed for drag", {}, {});
    }
}

void RunCancelDragUITask(void *data) {
    uint32_t winId = static_cast<uint32_t>(reinterpret_cast<uintptr_t>(data));
    bool hadActive = false;
    if (auto ws = findSession(winId)) {
        if (ws->activeDragAction != nullptr) {
            hadActive = true;
            ws->activeDragCanceled = true;
            disposeActiveDragAction(ws.get());
        }
    }
    {
        std::lock_guard<std::mutex> lk(gDragMu);
        if (gActiveHostDrag.srcWinId == winId) {
            hadActive = true;
            gActiveHostDrag = ActiveHostDrag{};
        }
    }
    if (hadActive) {
        sendDragEventWire(winId, 5, true, true, 0, 0, 0, 0.0, 0.0, "", "", {}, {});
    }
}

void handleDragCancel(const std::vector<uint8_t> &p) {
    if (p.size() < 4) {
        return;
    }
    uint32_t winId = 0;
    memcpy(&winId, p.data(), 4);
    auto ws = findSession(winId);
    if (ws && ws->uiContext != nullptr) {
        OH_ArkUI_PostUITask(ws->uiContext, reinterpret_cast<void *>(static_cast<uintptr_t>(winId)), RunCancelDragUITask);
    } else {
        RunCancelDragUITask(reinterpret_cast<void *>(static_cast<uintptr_t>(winId)));
    }
}

void handleNodeDragEvent(WinSession *ws, ArkUI_NodeEventType eventType, ArkUI_NodeEvent *event) {
    if (ws == nullptr || event == nullptr) {
        return;
    }
    if (eventType == NODE_ON_DRAG_LEAVE) {
        sendDragEventWire(ws->winId, 3, false, false, 0, 0, 0, 0.0, 0.0, "", "", {}, {});
        return;
    }
    if (eventType != NODE_ON_DRAG_ENTER && eventType != NODE_ON_DRAG_MOVE && eventType != NODE_ON_DROP) {
        return;
    }
    ArkUI_DragEvent *dev = OH_ArkUI_NodeEvent_GetDragEvent(event);
    if (dev == nullptr) {
        return;
    }

    const double sc = ws->scale > 0 ? ws->scale : 1.0;
    const double x = static_cast<double>(OH_ArkUI_DragEvent_GetTouchPointXToWindow(dev)) / sc;
    const double y = static_cast<double>(OH_ArkUI_DragEvent_GetTouchPointYToWindow(dev)) / sc;
    ws->lastPointerX = static_cast<float>(x);
    ws->lastPointerY = static_cast<float>(y);

    std::vector<std::string> formats;
    auto addFmt = [&](const std::string &f) {
        if (!f.empty() && std::find(formats.begin(), formats.end(), f) == formats.end()) {
            formats.push_back(f);
        }
    };

    int32_t typeCnt = 0;
    if (OH_ArkUI_DragEvent_GetDataTypeCount(dev, &typeCnt) == ARKUI_ERROR_CODE_NO_ERROR &&
        typeCnt > 0 && typeCnt <= 128) {
        std::vector<char> storage(static_cast<size_t>(typeCnt) * 256u, 0);
        std::vector<char *> ptrs(static_cast<size_t>(typeCnt), nullptr);
        for (int32_t i = 0; i < typeCnt; ++i) {
            ptrs[static_cast<size_t>(i)] = storage.data() + static_cast<size_t>(i) * 256u;
        }
        if (OH_ArkUI_DragEvent_GetDataTypes(dev, ptrs.data(), typeCnt, 256) == ARKUI_ERROR_CODE_NO_ERROR) {
            for (int32_t i = 0; i < typeCnt; ++i) {
                addFmt(ptrs[static_cast<size_t>(i)]);
            }
        }
    }

    uint32_t ops = 3u;
    uint32_t suggestedOp = 1u;
    uint64_t modKeys = 0;
    if (OH_ArkUI_DragEvent_GetModifierKeyStates(dev, &modKeys) == ARKUI_ERROR_CODE_NO_ERROR &&
        (modKeys & ARKUI_MODIFIER_KEY_SHIFT) != 0) {
        suggestedOp = 2u;
    }

    std::string session;
    std::vector<HostDragRep> reps;
    bool fromLocalDrag = false;
    {
        std::lock_guard<std::mutex> lk(gDragMu);
        if (gActiveHostDrag.active) {
            fromLocalDrag = true;
            ops = gActiveHostDrag.ops != 0 ? gActiveHostDrag.ops : 1u;
            if ((ops & suggestedOp) == 0) {
                suggestedOp = (ops & 1u) ? 1u : 2u;
            }
            session = gActiveHostDrag.session;
            for (const auto &r : gActiveHostDrag.reps) {
                addFmt(r.format);
            }
            if (eventType == NODE_ON_DROP) {
                reps = gActiveHostDrag.reps;
            }
        }
    }

    if (eventType == NODE_ON_DROP && !fromLocalDrag) {
        OH_UdmfData *ud = OH_UdmfData_Create();
        if (ud != nullptr) {
            if (OH_ArkUI_DragEvent_GetUdmfData(dev, ud) == ARKUI_ERROR_CODE_NO_ERROR) {
                unsigned int rcnt = 0;
                OH_UdmfRecord **recs = OH_UdmfData_GetRecords(ud, &rcnt);
                std::string uriLines;
                for (unsigned int i = 0; recs != nullptr && i < rcnt; ++i) {
                    OH_UdmfRecord *rec = recs[i];
                    if (rec == nullptr) {
                        continue;
                    }
                    unsigned int tcnt = 0;
                    char **types = OH_UdmfRecord_GetTypes(rec, &tcnt);
                    for (unsigned int t = 0; types != nullptr && t < tcnt; ++t) {
                        const char *ts = types[t];
                        if (ts == nullptr) {
                            continue;
                        }
                        std::string tstr(ts);
                        addFmt(tstr);
                        if (tstr == "general.plain-text" || tstr == "text/plain") {
                            OH_UdsPlainText *pt = OH_UdsPlainText_Create();
                            if (pt != nullptr && OH_UdmfRecord_GetPlainText(rec, pt) == 0) {
                                const char *c = OH_UdsPlainText_GetContent(pt);
                                if (c != nullptr) {
                                    HostDragRep rep;
                                    rep.format = "text/plain";
                                    rep.data.assign(reinterpret_cast<const uint8_t *>(c),
                                                    reinterpret_cast<const uint8_t *>(c) + strlen(c));
                                    reps.push_back(std::move(rep));
                                }
                            }
                            if (pt != nullptr) {
                                OH_UdsPlainText_Destroy(pt);
                            }
                        } else if (tstr == "general.html" || tstr == "text/html") {
                            OH_UdsHtml *ht = OH_UdsHtml_Create();
                            if (ht != nullptr && OH_UdmfRecord_GetHtml(rec, ht) == 0) {
                                const char *c = OH_UdsHtml_GetContent(ht);
                                if (c != nullptr) {
                                    HostDragRep rep;
                                    rep.format = "text/html";
                                    rep.data.assign(reinterpret_cast<const uint8_t *>(c),
                                                    reinterpret_cast<const uint8_t *>(c) + strlen(c));
                                    reps.push_back(std::move(rep));
                                }
                            }
                            if (ht != nullptr) {
                                OH_UdsHtml_Destroy(ht);
                            }
                        } else if (tstr == "general.file-uri" || tstr == "general.file" ||
                                   tstr == "general.folder" || tstr == "general.image" ||
                                   tstr == "general.video" || tstr == "general.audio") {
                            OH_UdsFileUri *fu = OH_UdsFileUri_Create();
                            if (fu != nullptr && OH_UdmfRecord_GetFileUri(rec, fu) == 0) {
                                const char *u = OH_UdsFileUri_GetFileUri(fu);
                                if (u != nullptr && u[0] != '\0') {
                                    if (!uriLines.empty()) {
                                        uriLines += "\r\n";
                                    }
                                    uriLines += u;
                                }
                            }
                            if (fu != nullptr) {
                                OH_UdsFileUri_Destroy(fu);
                            }
                        } else {
                            unsigned char *entry = nullptr;
                            unsigned int elen = 0;
                            if (OH_UdmfRecord_GetGeneralEntry(rec, ts, &entry, &elen) == 0 && entry != nullptr) {
                                HostDragRep rep;
                                rep.format = tstr;
                                rep.data.assign(entry, entry + elen);
                                reps.push_back(std::move(rep));
                            }
                        }
                    }
                }
                if (!uriLines.empty()) {
                    addFmt("text/uri-list");
                    HostDragRep rep;
                    rep.format = "text/uri-list";
                    rep.data.assign(reinterpret_cast<const uint8_t *>(uriLines.data()),
                                    reinterpret_cast<const uint8_t *>(uriLines.data()) + uriLines.size());
                    reps.push_back(std::move(rep));
                }
            }
            OH_UdmfData_Destroy(ud);
        }
    }

    uint8_t wireEv = 2;
    if (eventType == NODE_ON_DRAG_ENTER) {
        wireEv = 1;
    } else if (eventType == NODE_ON_DROP) {
        wireEv = 4;
    }

    uint32_t pendingId = 0;
    {
        std::lock_guard<std::mutex> lk(gDragReplyMu);
        pendingId = ++gNextDragPendingId;
        if (pendingId == 0) {
            pendingId = ++gNextDragPendingId;
        }
        gDragReplies[pendingId] = PendingDragReply{};
    }

    sendDragEventWire(ws->winId, wireEv, false, false, pendingId, ops, suggestedOp, x, y,
                      session, "", formats, reps);

    PendingDragReply reply{};
    const int waitMs = (eventType == NODE_ON_DROP) ? 400 : 80;
    {
        std::unique_lock<std::mutex> lk(gDragReplyMu);
        gDragReplyCv.wait_for(lk, std::chrono::milliseconds(waitMs), [&] {
            auto it = gDragReplies.find(pendingId);
            return it == gDragReplies.end() || it->second.ready;
        });
        auto it = gDragReplies.find(pendingId);
        if (it != gDragReplies.end()) {
            reply = it->second;
            gDragReplies.erase(it);
        }
    }

    if (reply.taken) {
        ArkUI_DropOperation dropOp = (reply.op & 2u) ? ARKUI_DROP_OPERATION_MOVE : ARKUI_DROP_OPERATION_COPY;
        OH_ArkUI_DragEvent_SetSuggestedDropOperation(dev, dropOp);
        OH_ArkUI_DragEvent_SetDragResult(dev, ARKUI_DRAG_RESULT_SUCCESSFUL);
    } else {
        OH_ArkUI_DragEvent_SetDragResult(dev, ARKUI_DRAG_RESULT_FAILED);
    }
}

struct HostAccessNode {
    uint64_t id = 0;
    int32_t parent = -1;
    uint8_t role = 0;
    uint8_t actions = 0;
    uint8_t current = 0;
    uint8_t hasPopup = 0;
    uint32_t states = 0;
    float x = 0, y = 0, w = 0, h = 0;
    double minVal = 0, maxVal = 0, nowVal = 0, stepVal = 0;
    int32_t selStart = 0, selEnd = 0, level = 0;
    uint16_t posInSet = 0, setSize = 0;
    std::string label;
    std::string value;
    std::string placeholder;
    std::string description;
    std::string roleDesc;
    std::vector<int64_t> childElemIds;
};

struct AccessWinSnapshot {
    bool enabledNotified = false;
    uint64_t focusId = 0;
    int32_t a11yFocusElemId = 0;
    std::string selectedWord;
    std::vector<HostAccessNode> nodes;
    std::vector<int64_t> rootChildElemIds;
};

std::mutex gAccessMu;
std::unordered_map<uint32_t, AccessWinSnapshot> gAccessTrees;
std::unordered_map<std::string, uint32_t> gAccessInstToWinId;

void sendAccessEventWire(uint32_t winId, uint8_t ev, uint8_t actionKind, uint64_t nodeId,
                         int32_t from, int32_t to, const std::string &text) {
    std::vector<uint8_t> wire(24, 0);
    memcpy(wire.data() + 0, &winId, 4);
    wire[4] = ev;
    wire[5] = actionKind;
    memcpy(wire.data() + 8, &nodeId, 8);
    memcpy(wire.data() + 16, &from, 4);
    memcpy(wire.data() + 20, &to, 4);
    appendStr(wire, text);
    sendMessage(kAccessEvent, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

const char *mapAccessRoleType(uint8_t role) {
    switch (role) {
    case 1:  return "text";
    case 2:  return "button";
    case 3:  return "link";
    case 4:  return "checkbox";
    case 5:  return "radio";
    case 6:  return "Toggle";
    case 7:  return "slider";
    case 8:  return "progress";
    case 9:  return "textInput";
    case 10: return "image";
    case 11: return "list";
    case 12: return "scroll";
    case 13: return "dialog";
    case 17: return "tabBar";
    case 18: return "tabContent";
    case 27: return "listItem";
    case 31: return "button";
    case 32: return "select";
    case 39: return "menu";
    case 41: return "menuitem";
    case 42: return "checkbox";
    case 43: return "radio";
    default: return "column";
    }
}

uint32_t resolveAccessWinIdLocked(const char *instanceId) {
    if (instanceId != nullptr) {
        auto it = gAccessInstToWinId.find(instanceId);
        if (it != gAccessInstToWinId.end()) {
            return it->second;
        }
    }
    return 1;
}

void ensureAccessEnabledForWinLocked(uint32_t winId, AccessWinSnapshot &snap, bool *sendEnableOut) {
    if (!snap.enabledNotified) {
        snap.enabledNotified = true;
        *sendEnableOut = true;
    }
}

void fillAccessElementLocked(uint32_t winId, const AccessWinSnapshot &snap, int32_t elemId,
                             double scale, int32_t surfW, int32_t surfH,
                             ArkUI_AccessibilityElementInfo *info) {
    if (info == nullptr) {
        return;
    }
    if (elemId <= 0 || static_cast<size_t>(elemId) > snap.nodes.size()) {
        OH_ArkUI_AccessibilityElementInfoSetElementId(info, 0);
        OH_ArkUI_AccessibilityElementInfoSetParentId(info, -2100000);
        OH_ArkUI_AccessibilityElementInfoSetComponentType(info, "root");
        OH_ArkUI_AccessibilityElementInfoSetEnabled(info, true);
        OH_ArkUI_AccessibilityElementInfoSetVisible(info, true);
        OH_ArkUI_AccessibilityElementInfoSetAccessibilityLevel(info, "yes");
        ArkUI_AccessibleRect rr{0, 0, std::max(1, surfW), std::max(1, surfH)};
        OH_ArkUI_AccessibilityElementInfoSetScreenRect(info, &rr);
        if (!snap.rootChildElemIds.empty()) {
            std::vector<int64_t> kids = snap.rootChildElemIds;
            OH_ArkUI_AccessibilityElementInfoSetChildNodeIds(info, static_cast<int32_t>(kids.size()), kids.data());
        }
        return;
    }

    const HostAccessNode &n = snap.nodes[static_cast<size_t>(elemId - 1)];
    const int32_t parentElem = (n.parent >= 0 && static_cast<size_t>(n.parent) < snap.nodes.size())
                                   ? (n.parent + 1)
                                   : 0;
    OH_ArkUI_AccessibilityElementInfoSetElementId(info, elemId);
    OH_ArkUI_AccessibilityElementInfoSetParentId(info, parentElem);
    OH_ArkUI_AccessibilityElementInfoSetComponentType(info,
                                                      n.roleDesc.empty() ? mapAccessRoleType(n.role) : n.roleDesc.c_str());

    const std::string &primaryText = !n.value.empty() ? n.value : n.label;
    OH_ArkUI_AccessibilityElementInfoSetContents(info, primaryText.c_str());
    OH_ArkUI_AccessibilityElementInfoSetAccessibilityText(info, n.label.c_str());
    if (!n.placeholder.empty()) {
        OH_ArkUI_AccessibilityElementInfoSetIsHint(info, n.value.empty());
        OH_ArkUI_AccessibilityElementInfoSetHintText(info, n.placeholder.c_str());
    }
    if (!n.description.empty()) {
        OH_ArkUI_AccessibilityElementInfoSetAccessibilityDescription(info, n.description.c_str());
    }

    const bool disabled = (n.states & (1u << 1)) != 0;
    const bool focusable = (n.states & (1u << 0)) != 0 || (n.actions & (1u << 1)) != 0;
    const bool checked = (n.states & (1u << 2)) != 0;
    const bool checkable = checked || (n.states & (1u << 3)) != 0 ||
                           n.role == 4 || n.role == 5 || n.role == 6 || n.role == 31 || n.role == 42 || n.role == 43;
    const bool selected = (n.states & (1u << 4)) != 0;
    const bool password = (n.states & (1u << 7)) != 0;
    const bool readOnly = (n.states & (1u << 8)) != 0;
    const bool offscreen = (n.states & (1u << 10)) != 0;
    const bool isFocused = (snap.focusId != 0 && n.id == snap.focusId);
    const bool isA11yFocused = (snap.a11yFocusElemId == elemId);
    const bool editable = (n.role == 9 || n.role == 32) && !readOnly && !disabled;
    const bool clickable = (n.actions & (1u << 0)) != 0 && !disabled;
    const bool scrollable = n.role == 11 || n.role == 12 || ((n.actions & ((1u << 2) | (1u << 3))) != 0);

    OH_ArkUI_AccessibilityElementInfoSetEnabled(info, !disabled);
    OH_ArkUI_AccessibilityElementInfoSetVisible(info, !offscreen);
    OH_ArkUI_AccessibilityElementInfoSetFocusable(info, focusable);
    OH_ArkUI_AccessibilityElementInfoSetFocused(info, isFocused);
    OH_ArkUI_AccessibilityElementInfoSetAccessibilityFocused(info, isA11yFocused);
    OH_ArkUI_AccessibilityElementInfoSetCheckable(info, checkable);
    OH_ArkUI_AccessibilityElementInfoSetChecked(info, checked);
    OH_ArkUI_AccessibilityElementInfoSetSelected(info, selected);
    OH_ArkUI_AccessibilityElementInfoSetClickable(info, clickable);
    OH_ArkUI_AccessibilityElementInfoSetIsPassword(info, password);
    OH_ArkUI_AccessibilityElementInfoSetEditable(info, editable);
    OH_ArkUI_AccessibilityElementInfoSetScrollable(info, scrollable);
    OH_ArkUI_AccessibilityElementInfoSetAccessibilityLevel(info, "yes");

    const double sc = scale > 0 ? scale : 1.0;
    ArkUI_AccessibleRect rect{
        static_cast<int32_t>(std::lround(n.x * sc)),
        static_cast<int32_t>(std::lround(n.y * sc)),
        static_cast<int32_t>(std::lround((n.x + n.w) * sc)),
        static_cast<int32_t>(std::lround((n.y + n.h) * sc)),
    };
    OH_ArkUI_AccessibilityElementInfoSetScreenRect(info, &rect);

    if (n.role == 7 || n.role == 8 || n.role == 34 || n.role == 35) {
        ArkUI_AccessibleRangeInfo ri{n.minVal, n.maxVal, n.nowVal};
        OH_ArkUI_AccessibilityElementInfoSetRangeInfo(info, &ri);
    }
    if (n.role == 9 || n.role == 32) {
        OH_ArkUI_AccessibilityElementInfoSetSelectedTextStart(info, n.selStart);
        OH_ArkUI_AccessibilityElementInfoSetSelectedTextEnd(info, n.selEnd);
    }
    if (n.setSize > 0) {
        OH_ArkUI_AccessibilityElementInfoSetItemCount(info, n.setSize);
        if (n.posInSet > 0) {
            OH_ArkUI_AccessibilityElementInfoSetCurrentItemIndex(info, n.posInSet - 1);
        }
    }

    if (!n.childElemIds.empty()) {
        std::vector<int64_t> kids = n.childElemIds;
        OH_ArkUI_AccessibilityElementInfoSetChildNodeIds(info, static_cast<int32_t>(kids.size()), kids.data());
    }

    std::vector<ArkUI_AccessibleAction> ops;
    ops.push_back({isA11yFocused ? ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_CLEAR_ACCESSIBILITY_FOCUS
                                 : ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_GAIN_ACCESSIBILITY_FOCUS,
                   "focus"});
    if (clickable) {
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_CLICK, "click"});
    }
    if ((n.actions & (1u << 6)) != 0) {
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_LONG_CLICK, "expand"});
    }
    if ((n.actions & (1u << 2)) != 0) {
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SCROLL_FORWARD, "increment"});
    }
    if ((n.actions & (1u << 3)) != 0) {
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SCROLL_BACKWARD, "decrement"});
    }
    if (editable || (n.actions & (1u << 4)) != 0) {
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SET_TEXT, "setText"});
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SELECT_TEXT, "selectText"});
        ops.push_back({ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SET_CURSOR_POSITION, "setCursor"});
    }
    OH_ArkUI_AccessibilityElementInfoSetOperationActions(info, static_cast<int32_t>(ops.size()), ops.data());
    (void)winId;
}

void getWindowMetricsForAccess(uint32_t winId, double *scaleOut, int32_t *wOut, int32_t *hOut) {
    *scaleOut = 1.0;
    *wOut = 1280;
    *hOut = 800;
    if (auto ws = findSession(winId)) {
        std::lock_guard<std::mutex> lk(ws->mu);
        *scaleOut = ws->scale > 0 ? ws->scale : 1.0;
        if (ws->curW > 0) {
            *wOut = ws->curW;
        }
        if (ws->curH > 0) {
            *hOut = ws->curH;
        }
    }
}

void appendSubtreeToAccessListLocked(uint32_t winId, const AccessWinSnapshot &snap, int32_t elemId,
                                     double sc, int32_t sw, int32_t sh,
                                     ArkUI_AccessibilityElementInfoList *list, bool recursive) {
    ArkUI_AccessibilityElementInfo *ei = OH_ArkUI_AddAndGetAccessibilityElementInfo(list);
    fillAccessElementLocked(winId, snap, elemId, sc, sw, sh, ei);
    const std::vector<int64_t> *kids = nullptr;
    if (elemId <= 0) {
        kids = &snap.rootChildElemIds;
    } else if (static_cast<size_t>(elemId) <= snap.nodes.size()) {
        kids = &snap.nodes[static_cast<size_t>(elemId - 1)].childElemIds;
    }
    if (kids == nullptr) {
        return;
    }
    for (int64_t cid : *kids) {
        if (recursive) {
            appendSubtreeToAccessListLocked(winId, snap, static_cast<int32_t>(cid), sc, sw, sh, list, true);
        } else {
            ArkUI_AccessibilityElementInfo *ci = OH_ArkUI_AddAndGetAccessibilityElementInfo(list);
            fillAccessElementLocked(winId, snap, static_cast<int32_t>(cid), sc, sw, sh, ci);
        }
    }
}

int32_t AccessFindById(const char *instanceId, int64_t elementId, ArkUI_AccessibilitySearchMode mode,
                       int32_t, ArkUI_AccessibilityElementInfoList *elementList) {
    if (elementList == nullptr) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_BAD_PARAMETER;
    }
    uint32_t winId = 1;
    bool notifyEnable = false;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        winId = resolveAccessWinIdLocked(instanceId);
        ensureAccessEnabledForWinLocked(winId, gAccessTrees[winId], &notifyEnable);
    }
    double sc = 1.0;
    int32_t sw = 1280, sh = 800;
    getWindowMetricsForAccess(winId, &sc, &sw, &sh);
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        const auto &snap = gAccessTrees[winId];
        int32_t eid = elementId <= 0 ? 0 : static_cast<int32_t>(elementId);
        if (eid > 0 && static_cast<size_t>(eid) > snap.nodes.size()) {
            return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
        }
        if ((mode & ARKUI_ACCESSIBILITY_NATIVE_SEARCH_MODE_PREFETCH_RECURSIVE_CHILDREN) != 0) {
            appendSubtreeToAccessListLocked(winId, snap, eid, sc, sw, sh, elementList, true);
        } else if ((mode & ARKUI_ACCESSIBILITY_NATIVE_SEARCH_MODE_PREFETCH_CHILDREN) != 0) {
            appendSubtreeToAccessListLocked(winId, snap, eid, sc, sw, sh, elementList, false);
        } else {
            ArkUI_AccessibilityElementInfo *ei = OH_ArkUI_AddAndGetAccessibilityElementInfo(elementList);
            fillAccessElementLocked(winId, snap, eid, sc, sw, sh, ei);
        }
    }
    if (notifyEnable) {
        sendAccessEventWire(winId, 1, 0, 0, 0, 0, "");
    }
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
}

int32_t AccessFindByText(const char *instanceId, int64_t elementId, const char *text,
                         int32_t, ArkUI_AccessibilityElementInfoList *elementList) {
    if (elementList == nullptr || text == nullptr) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_BAD_PARAMETER;
    }
    std::string needle(text);
    uint32_t winId = 1;
    bool notifyEnable = false;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        winId = resolveAccessWinIdLocked(instanceId);
        ensureAccessEnabledForWinLocked(winId, gAccessTrees[winId], &notifyEnable);
    }
    double sc = 1.0;
    int32_t sw = 1280, sh = 800;
    getWindowMetricsForAccess(winId, &sc, &sw, &sh);
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        const auto &snap = gAccessTrees[winId];
        for (size_t i = 0; i < snap.nodes.size(); ++i) {
            const auto &n = snap.nodes[i];
            if (!needle.empty() &&
                n.label.find(needle) == std::string::npos &&
                n.value.find(needle) == std::string::npos) {
                continue;
            }
            ArkUI_AccessibilityElementInfo *ei = OH_ArkUI_AddAndGetAccessibilityElementInfo(elementList);
            fillAccessElementLocked(winId, snap, static_cast<int32_t>(i + 1), sc, sw, sh, ei);
        }
    }
    if (notifyEnable) {
        sendAccessEventWire(winId, 1, 0, 0, 0, 0, "");
    }
    (void)elementId;
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
}

int32_t AccessFindFocused(const char *instanceId, int64_t, ArkUI_AccessibilityFocusType focusType,
                          int32_t, ArkUI_AccessibilityElementInfo *elementInfo) {
    if (elementInfo == nullptr) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_BAD_PARAMETER;
    }
    uint32_t winId = 1;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        winId = resolveAccessWinIdLocked(instanceId);
    }
    double sc = 1.0;
    int32_t sw = 1280, sh = 800;
    getWindowMetricsForAccess(winId, &sc, &sw, &sh);

    std::lock_guard<std::mutex> lk(gAccessMu);
    const auto &snap = gAccessTrees[winId];
    int32_t foundElem = 0;
    if (focusType == ARKUI_ACCESSIBILITY_NATIVE_FOCUS_TYPE_ACCESSIBILITY) {
        if (snap.a11yFocusElemId > 0 && static_cast<size_t>(snap.a11yFocusElemId) <= snap.nodes.size()) {
            foundElem = snap.a11yFocusElemId;
        }
    } else if (focusType == ARKUI_ACCESSIBILITY_NATIVE_FOCUS_TYPE_INPUT && snap.focusId != 0) {
        for (size_t i = 0; i < snap.nodes.size(); ++i) {
            if (snap.nodes[i].id == snap.focusId) {
                foundElem = static_cast<int32_t>(i + 1);
                break;
            }
        }
    }
    if (foundElem <= 0) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
    }
    fillAccessElementLocked(winId, snap, foundElem, sc, sw, sh, elementInfo);
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
}

int32_t AccessFindNextFocus(const char *instanceId, int64_t elementId,
                            ArkUI_AccessibilityFocusMoveDirection direction,
                            int32_t, ArkUI_AccessibilityElementInfo *elementInfo) {
    if (elementInfo == nullptr) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_BAD_PARAMETER;
    }
    uint32_t winId = 1;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        winId = resolveAccessWinIdLocked(instanceId);
    }
    double sc = 1.0;
    int32_t sw = 1280, sh = 800;
    getWindowMetricsForAccess(winId, &sc, &sw, &sh);

    std::lock_guard<std::mutex> lk(gAccessMu);
    const auto &snap = gAccessTrees[winId];
    if (snap.nodes.empty()) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
    }
    const int32_t n = static_cast<int32_t>(snap.nodes.size());
    const bool backward = (direction == ARKUI_ACCESSIBILITY_NATIVE_DIRECTION_BACKWARD ||
                           direction == ARKUI_ACCESSIBILITY_NATIVE_DIRECTION_UP ||
                           direction == ARKUI_ACCESSIBILITY_NATIVE_DIRECTION_LEFT);
    int32_t cur = elementId <= 0 ? (backward ? n + 1 : 0) : static_cast<int32_t>(elementId);
    int32_t step = backward ? -1 : 1;
    for (int32_t e = cur + step; e >= 1 && e <= n; e += step) {
        const auto &nd = snap.nodes[static_cast<size_t>(e - 1)];
        if ((nd.states & (1u << 10)) != 0) {
            continue;
        }
        fillAccessElementLocked(winId, snap, e, sc, sw, sh, elementInfo);
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    }
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
}

int32_t AccessExecuteAction(const char *instanceId, int64_t elementId,
                            ArkUI_Accessibility_ActionType action,
                            ArkUI_AccessibilityActionArguments *args, int32_t) {
    uint32_t winId = 1;
    uint64_t nodeId = 0;
    uint8_t nodeStates = 0;
    ArkUI_AccessibilityProvider *provider = nullptr;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        winId = resolveAccessWinIdLocked(instanceId);
        auto &snap = gAccessTrees[winId];
        if (action == ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_CLEAR_ACCESSIBILITY_FOCUS) {
            snap.a11yFocusElemId = 0;
            return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
        }
        if (elementId <= 0 || static_cast<size_t>(elementId) > snap.nodes.size()) {
            return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
        }
        const auto &nd = snap.nodes[static_cast<size_t>(elementId - 1)];
        nodeId = nd.id;
        nodeStates = static_cast<uint8_t>(nd.states & 0xFFu);
        if (action == ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_GAIN_ACCESSIBILITY_FOCUS) {
            snap.a11yFocusElemId = static_cast<int32_t>(elementId);
        }
    }
    if (auto ws = findSession(winId)) {
        provider = ws->accessProvider;
    }

    auto getArgStr = [&](const char *key) -> std::string {
        if (args == nullptr) {
            return "";
        }
        char *val = nullptr;
        if (OH_ArkUI_FindAccessibilityActionArgumentByKey(args, key, &val) ==
                ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL &&
            val != nullptr) {
            return std::string(val);
        }
        return "";
    };

    switch (action) {
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_GAIN_ACCESSIBILITY_FOCUS:
        sendAccessEventWire(winId, 2, 5, nodeId, 0, 0, ""); // AccessScrollIntoView
        if (provider != nullptr) {
            ArkUI_AccessibilityEventInfo *ev = OH_ArkUI_CreateAccessibilityEventInfo();
            if (ev != nullptr) {
                OH_ArkUI_AccessibilityEventSetEventType(ev, ARKUI_ACCESSIBILITY_NATIVE_EVENT_TYPE_ACCESSIBILITY_FOCUSED);
                OH_ArkUI_SendAccessibilityAsyncEvent(provider, ev, [](int32_t) {});
                OH_ArkUI_DestoryAccessibilityEventInfo(ev);
            }
        }
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_CLICK:
        sendAccessEventWire(winId, 2, 0, nodeId, 0, 0, ""); // AccessPress
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_LONG_CLICK: {
        const bool expanded = (nodeStates & (1u << 5)) != 0;
        sendAccessEventWire(winId, 2, expanded ? 7 : 6, nodeId, 0, 0, ""); // Collapse / Expand
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    }
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SCROLL_FORWARD:
        sendAccessEventWire(winId, 2, 2, nodeId, 0, 0, ""); // AccessIncrement
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SCROLL_BACKWARD:
        sendAccessEventWire(winId, 2, 3, nodeId, 0, 0, ""); // AccessDecrement
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SET_TEXT: {
        std::string txt = getArgStr("setText");
        sendAccessEventWire(winId, 2, 4, nodeId, 0, 0, txt); // AccessSetValue
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    }
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SELECT_TEXT: {
        int32_t b = atoi(getArgStr("selectTextBegin").c_str());
        int32_t e = atoi(getArgStr("selectTextEnd").c_str());
        sendAccessEventWire(winId, 2, 8, nodeId, b, e, ""); // AccessSelectText
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    }
    case ARKUI_ACCESSIBILITY_NATIVE_ACTION_TYPE_SET_CURSOR_POSITION: {
        int32_t pos = atoi(getArgStr("offset").c_str());
        sendAccessEventWire(winId, 2, 8, nodeId, pos, pos, ""); // AccessSelectText(pos, pos)
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
    }
    default:
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
    }
}

int32_t AccessClearFocus(const char *instanceId) {
    std::lock_guard<std::mutex> lk(gAccessMu);
    uint32_t winId = resolveAccessWinIdLocked(instanceId);
    gAccessTrees[winId].a11yFocusElemId = 0;
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
}

int32_t AccessGetCursorPos(const char *instanceId, int64_t elementId, int32_t, int32_t *index) {
    if (index == nullptr) {
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_BAD_PARAMETER;
    }
    std::lock_guard<std::mutex> lk(gAccessMu);
    uint32_t winId = resolveAccessWinIdLocked(instanceId);
    const auto &snap = gAccessTrees[winId];
    if (elementId <= 0 || static_cast<size_t>(elementId) > snap.nodes.size()) {
        *index = 0;
        return ARKUI_ACCESSIBILITY_NATIVE_RESULT_FAILED;
    }
    *index = snap.nodes[static_cast<size_t>(elementId - 1)].selEnd;
    return ARKUI_ACCESSIBILITY_NATIVE_RESULT_SUCCESSFUL;
}

ArkUI_AccessibilityProviderCallbacksWithInstance gAccessCallbacks = {
    AccessFindById,
    AccessFindByText,
    AccessFindFocused,
    AccessFindNextFocus,
    AccessExecuteAction,
    AccessClearFocus,
    AccessGetCursorPos,
};

void attachWindowAccessibility(WinSession *ws) {
    if (ws == nullptr || ws->xcNode == nullptr) {
        return;
    }
    ws->accessInstId = "mygo_win_" + std::to_string(ws->winId);
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        gAccessInstToWinId[ws->accessInstId] = ws->winId;
    }
    ws->accessProvider = OH_ArkUI_AccessibilityProvider_Create(ws->xcNode);
    if (ws->accessProvider != nullptr) {
        OH_ArkUI_AccessibilityProviderRegisterCallbackWithInstance(
            ws->accessInstId.c_str(), ws->accessProvider, &gAccessCallbacks);
    }
}

void detachWindowAccessibility(WinSession *ws, bool eraseTree) {
    if (ws == nullptr) {
        return;
    }
    if (ws->accessProvider != nullptr) {
        OH_ArkUI_AccessibilityProvider_Dispose(ws->accessProvider);
        ws->accessProvider = nullptr;
    }
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        if (!ws->accessInstId.empty()) {
            gAccessInstToWinId.erase(ws->accessInstId);
        }
        if (eraseTree) {
            gAccessTrees.erase(ws->winId);
        }
    }
    ws->accessInstId.clear();
}

void handleAccessTree(const std::vector<uint8_t> &p) {
    if (p.size() < 12) {
        return;
    }
    uint32_t winId = 0;
    uint64_t focusId = 0;
    memcpy(&winId, p.data() + 0, 4);
    memcpy(&focusId, p.data() + 4, 8);
    size_t off = 12;
    std::string selectedWord;
    uint32_t annCount = 0, alertCount = 0, nodeCount = 0;
    if (!readStr(p, off, &selectedWord) || !readU32(p, off, &annCount) || annCount > 256) {
        return;
    }
    std::vector<std::string> anns(annCount);
    for (uint32_t i = 0; i < annCount; ++i) {
        if (!readStr(p, off, &anns[i])) {
            return;
        }
    }
    if (!readU32(p, off, &alertCount) || alertCount > 256) {
        return;
    }
    std::vector<std::string> alerts(alertCount);
    for (uint32_t i = 0; i < alertCount; ++i) {
        if (!readStr(p, off, &alerts[i])) {
            return;
        }
    }
    if (!readU32(p, off, &nodeCount) || nodeCount > 16384) {
        return;
    }
    std::vector<HostAccessNode> nodes(nodeCount);
    std::vector<int64_t> rootKids;
    for (uint32_t i = 0; i < nodeCount; ++i) {
        if (off + 84 > p.size()) {
            return;
        }
        HostAccessNode &n = nodes[i];
        const uint8_t *nb = p.data() + off;
        off += 84;
        memcpy(&n.id, nb + 0, 8);
        memcpy(&n.parent, nb + 8, 4);
        n.role = nb[12];
        n.actions = nb[13];
        n.current = nb[14];
        n.hasPopup = nb[15];
        memcpy(&n.states, nb + 16, 4);
        memcpy(&n.x, nb + 20, 4);
        memcpy(&n.y, nb + 24, 4);
        memcpy(&n.w, nb + 28, 4);
        memcpy(&n.h, nb + 32, 4);
        memcpy(&n.minVal, nb + 36, 8);
        memcpy(&n.maxVal, nb + 44, 8);
        memcpy(&n.nowVal, nb + 52, 8);
        memcpy(&n.stepVal, nb + 60, 8);
        memcpy(&n.selStart, nb + 68, 4);
        memcpy(&n.selEnd, nb + 72, 4);
        memcpy(&n.level, nb + 76, 4);
        memcpy(&n.posInSet, nb + 80, 2);
        memcpy(&n.setSize, nb + 82, 2);
        if (!readStr(p, off, &n.label) ||
            !readStr(p, off, &n.value) ||
            !readStr(p, off, &n.placeholder) ||
            !readStr(p, off, &n.description) ||
            !readStr(p, off, &n.roleDesc)) {
            return;
        }
    }
    if (off != p.size()) {
        return;
    }
    for (uint32_t i = 0; i < nodeCount; ++i) {
        int32_t par = nodes[i].parent;
        if (par >= 0 && static_cast<uint32_t>(par) < nodeCount) {
            nodes[static_cast<size_t>(par)].childElemIds.push_back(static_cast<int64_t>(i + 1));
        } else {
            rootKids.push_back(static_cast<int64_t>(i + 1));
        }
    }

    bool focusChanged = false;
    int32_t focusElemId = 0;
    {
        std::lock_guard<std::mutex> lk(gAccessMu);
        auto &snap = gAccessTrees[winId];
        focusChanged = (snap.focusId != focusId && focusId != 0);
        snap.focusId = focusId;
        snap.selectedWord = std::move(selectedWord);
        snap.nodes = std::move(nodes);
        snap.rootChildElemIds = std::move(rootKids);
        if (focusChanged) {
            for (size_t i = 0; i < snap.nodes.size(); ++i) {
                if (snap.nodes[i].id == focusId) {
                    focusElemId = static_cast<int32_t>(i + 1);
                    break;
                }
            }
        }
    }

    auto ws = findSession(winId);
    if (!ws || ws->accessProvider == nullptr) {
        return;
    }
    ArkUI_AccessibilityProvider *prov = ws->accessProvider;

    ArkUI_AccessibilityEventInfo *pageEv = OH_ArkUI_CreateAccessibilityEventInfo();
    if (pageEv != nullptr) {
        OH_ArkUI_AccessibilityEventSetEventType(pageEv, ARKUI_ACCESSIBILITY_NATIVE_EVENT_TYPE_PAGE_CONTENT_UPDATE);
        OH_ArkUI_SendAccessibilityAsyncEvent(prov, pageEv, [](int32_t) {});
        OH_ArkUI_DestoryAccessibilityEventInfo(pageEv);
    }
    if (focusChanged && focusElemId > 0) {
        ArkUI_AccessibilityEventInfo *fev = OH_ArkUI_CreateAccessibilityEventInfo();
        if (fev != nullptr) {
            OH_ArkUI_AccessibilityEventSetEventType(fev, ARKUI_ACCESSIBILITY_NATIVE_EVENT_TYPE_FOCUS_NODE_UPDATE);
            OH_ArkUI_AccessibilityEventSetRequestFocusId(fev, focusElemId);
            OH_ArkUI_SendAccessibilityAsyncEvent(prov, fev, [](int32_t) {});
            OH_ArkUI_DestoryAccessibilityEventInfo(fev);
        }
    }
    auto sendAnnounce = [&](const std::string &msg) {
        if (msg.empty()) {
            return;
        }
        ArkUI_AccessibilityEventInfo *aev = OH_ArkUI_CreateAccessibilityEventInfo();
        if (aev != nullptr) {
            OH_ArkUI_AccessibilityEventSetEventType(aev, ARKUI_ACCESSIBILITY_NATIVE_EVENT_TYPE_ANNOUNCE_FOR_ACCESSIBILITY);
            OH_ArkUI_AccessibilityEventSetTextAnnouncedForAccessibility(aev, msg.c_str());
            OH_ArkUI_SendAccessibilityAsyncEvent(prov, aev, [](int32_t) {});
            OH_ArkUI_DestoryAccessibilityEventInfo(aev);
        }
    };
    for (const auto &s : alerts) {
        sendAnnounce(s);
    }
    for (const auto &s : anns) {
        sendAnnounce(s);
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
        ws->f12Down = false;
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
        int32_t source = OH_ArkUI_UIInputEvent_GetSourceType(ev);
        int32_t tool = OH_ArkUI_UIInputEvent_GetToolType(ev);
        if (source == UI_INPUT_EVENT_SOURCE_TYPE_MOUSE ||
            tool == UI_INPUT_EVENT_TOOL_TYPE_MOUSE ||
            tool == UI_INPUT_EVENT_TOOL_TYPE_TOUCHPAD) {
            return;
        }
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
        if (code == kArkKeyF12 && isPerfHudAvailable()) {
            if (kt == ARKUI_KEY_EVENT_DOWN) {
                if (!ws->f12Down) {
                    ws->f12Down = true;
                    setPerfHudEnabled(ws->winId, -1);
                }
            } else {
                ws->f12Down = false;
            }
            OH_ArkUI_KeyEvent_SetConsumed(ev, true);
            return;
        }
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
    case NODE_ON_DRAG_ENTER:
    case NODE_ON_DRAG_MOVE:
    case NODE_ON_DRAG_LEAVE:
    case NODE_ON_DROP:
        handleNodeDragEvent(ws.get(), type, event);
        return;
    default:
        return;
    }
}

// ---------- Per-Window VSync & Swapchain Render Thread ----------

void OnVsyncCallback(long long timestamp, void *data) {
    auto *ws = static_cast<WinSession *>(data);
    if (ws == nullptr) {
        return;
    }
    const int64_t nowNs = monoNs();
    int64_t hwNs = static_cast<int64_t>(timestamp);
    if (hwNs <= 0 || std::llabs(nowNs - hwNs) > 50000000LL) {
        hwNs = nowNs;
    }
    long long periodNs = 0;
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->vsyncSeq++;
        ws->vsyncHwNs = hwNs;
        if (!ws->stopRender && ws->vsync != nullptr) {
            if (OH_NativeVSync_GetPeriod(ws->vsync, &periodNs) == 0 &&
                periodNs >= 2000000LL && periodNs <= 50000000LL) {
                ws->vsyncPeriodHwNs = static_cast<int64_t>(periodNs);
            }
            if (ws->wantFrame || ws->frameInFlight || ws->inputPrefetch) {
                ws->vsyncIdleTicks = 0;
            } else {
                ws->vsyncIdleTicks++;
            }
            // Self-rearm right at t=0.0ms of the hardware VSync callback across active
            // rendering and brief (~100ms) input gaps so 5.5-7.9ms frames never miss
            // Rosen's registration cutoff for the next 120Hz VSync.
            if (ws->vsyncIdleTicks <= 12) {
                ws->vsyncArmed = (OH_NativeVSync_RequestFrame(ws->vsync, OnVsyncCallback, ws) == 0);
            } else {
                ws->vsyncArmed = false;
            }
        } else {
            ws->vsyncArmed = false;
        }
    }
    ws->cv.notify_all();
}

void WinSession::ensureVsyncArmedLocked() {
    if (stopRender || vsync == nullptr) {
        return;
    }
    vsyncIdleTicks = 0;
    if (!vsyncArmed) {
        vsyncArmed = (OH_NativeVSync_RequestFrame(vsync, OnVsyncCallback, this) == 0);
    }
}

void WinSession::applyExpectedFrameRateLocked(uint32_t) {
    if (vsync == nullptr) {
        return;
    }
    // Keep hardware VSync and Rosen/APS GPU clocks at the display's native max
    // refresh rate (e.g. 120Hz). Lower-rate canvas animations (AnimationFrameRate)
    // are paced on exact VSync multiples by package ui's paceSkips() without
    // downclocking touchpad scrolling or UI transitions to 30Hz.
    int32_t maxHz = std::max<int32_t>(60, static_cast<int32_t>(std::lround(hz)));
    OH_NativeVSync_ExpectedRateRange range = {maxHz, maxHz, maxHz};
    OH_NativeVSync_SetExpectedFrameRateRange(vsync, &range);
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

struct PerfFrameSample {
    uint64_t sampleSeq = 0;
    double dtMs = 0.0;
    double uiGlMs = 0.0;
    double gpuMs = 0.0;
    double copyMs = 0.0;
    double swapMs = 0.0;
    double vsyncMs = 0.0;
    double in2FlushMs = -1.0;
    bool wholeCopy = false;
    bool zeroCopy = false;
    uint8_t coalesced = 0;
    uint8_t scrollEvs = 0;
};

double percentileOfSorted(const std::vector<double> &sorted, double q) {
    if (sorted.empty()) {
        return 0.0;
    }
    size_t idx = static_cast<size_t>(std::ceil(q * static_cast<double>(sorted.size())));
    if (idx > 0) {
        idx--;
    }
    if (idx >= sorted.size()) {
        idx = sorted.size() - 1;
    }
    return sorted[idx];
}

PerfHudData buildPerfHudSnapshot(WinSession *ws, int32_t w, int32_t h, double sc, double refresh,
                                 int aborted, bool live, bool visible,
                                 const std::deque<PerfFrameSample> &window) {
    PerfHudData out;
    out.winId = ws->winId;
    out.visible = visible;
    out.live = live;

    std::vector<double> dts;
    std::vector<double> uiGls;
    std::vector<double> gpus;
    std::vector<double> inLats;
    dts.reserve(window.size());
    uiGls.reserve(window.size());
    gpus.reserve(window.size());
    inLats.reserve(window.size());

    double sumDt = 0.0;
    double sumUiGl = 0.0, sumGpu = 0.0, sumCopy = 0.0, sumSwap = 0.0, sumVsync = 0.0;
    int tier120 = 0, tier60 = 0, tierDrop = 0;
    int wholeCnt = 0;
    int zeroCopyCnt = 0;
    int coalSum = 0;

    for (const auto &s : window) {
        if (s.dtMs > 0.2) {
            dts.push_back(s.dtMs);
            sumDt += s.dtMs;
            if (s.dtMs <= 10.5) {
                tier120++;
            } else if (s.dtMs <= 19.0) {
                tier60++;
            } else {
                tierDrop++;
            }
        }
        uiGls.push_back(s.uiGlMs);
        gpus.push_back(s.gpuMs);
        sumUiGl += s.uiGlMs;
        sumGpu += s.gpuMs;
        sumCopy += s.copyMs;
        sumSwap += s.swapMs;
        sumVsync += s.vsyncMs;
        if (s.in2FlushMs >= 0.0) {
            inLats.push_back(s.in2FlushMs);
        }
        if (s.wholeCopy) {
            wholeCnt++;
        }
        if (s.zeroCopy) {
            zeroCopyCnt++;
        }
        coalSum += s.coalesced;
    }

    const double nFrames = window.empty() ? 1.0 : static_cast<double>(window.size());
    std::sort(dts.begin(), dts.end());
    std::sort(uiGls.begin(), uiGls.end());
    std::sort(gpus.begin(), gpus.end());
    std::sort(inLats.begin(), inLats.end());

    const double avgDt = dts.empty() ? 0.0 : (sumDt / static_cast<double>(dts.size()));
    const double p50Dt = percentileOfSorted(dts, 0.50);
    const double p90Dt = percentileOfSorted(dts, 0.90);
    const double p99Dt = percentileOfSorted(dts, 0.99);
    const double maxDt = dts.empty() ? 0.0 : dts.back();

    const double fps = avgDt > 0.1 ? (1000.0 / avgDt) : 0.0;
    const double low1Fps = p99Dt > 0.1 ? (1000.0 / p99Dt) : 0.0;

    const int nDt = static_cast<int>(dts.size());
    const int pct120 = nDt > 0 ? static_cast<int>(std::lround(100.0 * tier120 / nDt)) : 0;
    const int pct60 = nDt > 0 ? static_cast<int>(std::lround(100.0 * tier60 / nDt)) : 0;
    const int pctDrop = nDt > 0 ? std::max(0, 100 - pct120 - pct60) : 0;

    const double avgUiGl = sumUiGl / nFrames;
    const double p90UiGl = percentileOfSorted(uiGls, 0.90);
    const double avgGpu = sumGpu / nFrames;
    const double p90Gpu = percentileOfSorted(gpus, 0.90);
    const double avgCopy = sumCopy / nFrames;
    const double avgSwap = sumSwap / nFrames;
    const double avgVsync = sumVsync / nFrames;
    const int wholePct = window.empty() ? 0 : static_cast<int>(std::lround(100.0 * wholeCnt / nFrames));
    const double p50InLat = percentileOfSorted(inLats, 0.50);

    std::vector<double> scDts;
    {
        std::lock_guard<std::mutex> lk(ws->perfMu);
        scDts.assign(ws->scrollDtMs.begin(), ws->scrollDtMs.end());
    }
    double scHz = 0.0, scP50 = 0.0, scP99 = 0.0;
    if (!scDts.empty()) {
        double scSum = 0.0;
        for (double v : scDts) {
            scSum += v;
        }
        double scAvg = scSum / static_cast<double>(scDts.size());
        if (scAvg > 0.1) {
            scHz = 1000.0 / scAvg;
        }
        std::sort(scDts.begin(), scDts.end());
        scP50 = percentileOfSorted(scDts, 0.50);
        scP99 = percentileOfSorted(scDts, 0.99);
    }

    char buf[220];
    if (nDt > 0) {
        snprintf(buf, sizeof(buf), "FPS %5.1f  (1%%L %4.1f)  |  %.0fHz · %dx%d @%.2fx  [F12]",
                 fps, low1Fps, refresh, w, h, sc);
    } else {
        snprintf(buf, sizeof(buf), "FPS  --.-  (1%%L --.-)  |  %.0fHz · %dx%d @%.2fx  [F12]",
                 refresh, w, h, sc);
    }
    out.headerLine = buf;

    if (nDt > 0) {
        snprintf(buf, sizeof(buf),
                 "dt(ms) p50 %.1f p90 %.1f p99 %.1f max %.1f | 120f %d%% 60f %d%% <60f %d%%",
                 p50Dt, p90Dt, p99Dt, maxDt, pct120, pct60, pctDrop);
    } else {
        snprintf(buf, sizeof(buf), "dt(ms)  waiting for continuous motion frames...");
    }
    out.pacingLine = buf;

    if (zeroCopyCnt > 0) {
        snprintf(buf, sizeof(buf),
                 "Stage  UI+GL %.1f/%.1f | GPU %.1f/%.1f | ZC | Swap %.1f | VS %.1fms",
                 avgUiGl, p90UiGl, avgGpu, p90Gpu, avgSwap, avgVsync);
    } else {
        snprintf(buf, sizeof(buf),
                 "Stage  UI+GL %.1f/%.1f | GPU %.1f/%.1f | Copy %.1f | Swap %.1fms",
                 avgUiGl, p90UiGl, avgGpu, p90Gpu, avgCopy, avgSwap);
    }
    out.stageLine = buf;

    if (scHz > 0.1) {
        snprintf(buf, sizeof(buf),
                 "Input   Scroll %.0fHz (dt %.1f/%.1fms)  Coal %d  In->Flush %.1fms  |  Whole %d%%  Abt %d",
                 scHz, scP50, scP99, coalSum, p50InLat, wholePct, aborted);
    } else {
        snprintf(buf, sizeof(buf),
                 "Input   Scroll --Hz  In->Flush %.1fms  |  Whole %d%%  Abt %d",
                 p50InLat, wholePct, aborted);
    }
    out.inputLine = buf;

    const size_t kBars = 60;
    const size_t startIdx = window.size() > kBars ? (window.size() - kBars) : 0;
    const size_t barCount = window.size() - startIdx;
    out.dtMs.reserve(barCount);
    out.uiGlMs.reserve(barCount);
    out.gpuMs.reserve(barCount);
    out.copySwapMs.reserve(barCount);
    for (size_t i = startIdx; i < window.size(); ++i) {
        const auto &s = window[i];
        out.dtMs.push_back(s.dtMs > 0.2 ? s.dtMs : 8.33);
        out.uiGlMs.push_back(s.uiGlMs);
        out.gpuMs.push_back(s.gpuMs);
        out.copySwapMs.push_back(s.copyMs + s.swapMs);
    }

    {
        std::lock_guard<std::mutex> lk(ws->perfMu);
        ws->lastPerfSnapshot = out;
    }
    return out;
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
        fpsDirty = false;
        applyExpectedFrameRateLocked(requestedFps);
    }
    if (win == nullptr || w <= 0 || h <= 0) {
        return;
    }

    bool gpuMode = wantGpuSwapchain();
    const bool uncappedMode = envTruthy(getenv("MYGO_UNCAPPED"));
    int32_t queueSize = 0;
    auto configureWinUsage = [&](bool gpu, int32_t initW, int32_t initH) {
        OH_NativeWindow_NativeWindowHandleOpt(win, SET_FORMAT, NATIVEBUFFER_PIXEL_FMT_RGBA_8888);
        OH_NativeWindow_NativeWindowHandleOpt(win, SET_SOURCE_TYPE, static_cast<int32_t>(OH_SURFACE_SOURCE_UI));
        OH_NativeWindow_NativeWindowHandleOpt(win, SET_SWAP_INTERVAL, uncappedMode ? 0 : 1);
        uint64_t usage = 0;
        OH_NativeWindow_NativeWindowHandleOpt(win, GET_USAGE, &usage);
        if (gpu) {
            usage &= ~static_cast<uint64_t>(NATIVEBUFFER_USAGE_CPU_READ |
                                            NATIVEBUFFER_USAGE_CPU_WRITE |
                                            NATIVEBUFFER_USAGE_CPU_READ_OFTEN |
                                            NATIVEBUFFER_USAGE_MEM_MMZ_CACHE);
            usage |= NATIVEBUFFER_USAGE_HW_RENDER | NATIVEBUFFER_USAGE_HW_TEXTURE | NATIVEBUFFER_USAGE_MEM_DMA;
        } else {
            usage &= ~static_cast<uint64_t>(NATIVEBUFFER_USAGE_CPU_READ_OFTEN);
            usage |= NATIVEBUFFER_USAGE_CPU_READ | NATIVEBUFFER_USAGE_CPU_WRITE | NATIVEBUFFER_USAGE_MEM_DMA;
        }
        OH_NativeWindow_NativeWindowHandleOpt(win, SET_USAGE, usage);
        if (initW > 0 && initH > 0) {
            OH_NativeWindow_NativeWindowHandleOpt(win, SET_BUFFER_GEOMETRY, initW, initH);
        }
        queueSize = 0;
        OH_NativeWindow_NativeWindowHandleOpt(win, GET_BUFFERQUEUE_SIZE, &queueSize);
        using PreAllocFn = int32_t (*)(OHNativeWindow *, uint32_t);
        static PreAllocFn preAllocFn = reinterpret_cast<PreAllocFn>(
            dlsym(RTLD_DEFAULT, "OH_NativeWindow_PreAllocBuffers"));
        if (preAllocFn != nullptr) {
            uint32_t preCnt = std::min<uint32_t>(
                6u, std::max<uint32_t>(4u, static_cast<uint32_t>(std::max<int32_t>(0, queueSize))));
            (void)preAllocFn(win, preCnt);
        }
    };
    configureWinUsage(gpuMode, w, h);
    OH_LOG_INFO(LOG_APP,
                "win=%{public}u surface %{public}dx%{public}d scale=%{public}.2f hz=%{public}.0f gpuSwap=%{public}d qSize=%{public}d uncapped=%{public}d",
                winId, w, h, sc, refresh, gpuMode ? 1 : 0, queueSize, uncappedMode ? 1 : 0);
    sendSurface(winId, w, h, sc, refresh, true);

    struct SentGpuBuf {
        uint32_t id = 0;
        int32_t w = 0;
        int32_t h = 0;
    };
    struct PendingGpuFence {
        int fd = -1;
        int64_t submitNs = 0;
        uint64_t sampleSeq = 0;
    };
    struct PrefetchedBuf {
        OHNativeWindowBuffer *buf = nullptr;
        int fence = -1;
        int32_t w = 0;
        int32_t h = 0;
        double reqMs = 0.0;
    };
    std::deque<SentGpuBuf> sentGpuBufs;
    std::deque<PendingGpuFence> pendingGpuFences;
    PrefetchedBuf prefetched;
    uint64_t nextSampleSeq = 1;
    int64_t lastGpuSignalNs = 0;
    double lastMeasuredGpuMs = 0.0;

    uint32_t seq = 0;
    int frames = 0, damaged = 0, whole = 0, aborted = 0;
    int unloggedFrames = 0;
    int64_t lastFlushNs = 0;
    uint64_t lastFlushVsyncSeq = 0;
    int64_t lastHudPushNs = 0;
    bool hudSettled = true;
    LatencyRing drawRing, inRing, intervalRing;
    std::deque<PerfFrameSample> perfWindow;

    auto releasePrefetched = [&]() {
        if (prefetched.fence >= 0) {
            close(prefetched.fence);
            prefetched.fence = -1;
        }
        if (prefetched.buf != nullptr) {
            OH_NativeWindow_NativeWindowAbortBuffer(win, prefetched.buf);
            prefetched.buf = nullptr;
        }
        prefetched.w = 0;
        prefetched.h = 0;
        prefetched.reqMs = 0.0;
    };

    auto ensureGeometryAndPrefetch = [&](int32_t reqW, int32_t reqH) -> bool {
        if (reqW <= 0 || reqH <= 0) {
            return false;
        }
        if (prefetched.buf != nullptr) {
            if (prefetched.w == reqW && prefetched.h == reqH) {
                return true;
            }
            releasePrefetched();
        }
        int32_t gw = 0, gh = 0;
        if (OH_NativeWindow_NativeWindowHandleOpt(win, GET_BUFFER_GEOMETRY, &gh, &gw) != 0 ||
            gw != reqW || gh != reqH) {
            OH_NativeWindow_NativeWindowHandleOpt(win, SET_BUFFER_GEOMETRY, reqW, reqH);
        }
        const int64_t t0 = monoNs();
        OHNativeWindowBuffer *b = nullptr;
        int f = -1;
        int32_t rc = OH_NativeWindow_NativeWindowRequestBuffer(win, &b, &f);
        if (rc != 0 || b == nullptr) {
            if (f >= 0) {
                close(f);
            }
            OH_LOG_ERROR(LOG_APP, "win=%{public}u RequestBuffer failed rc=%{public}d", winId, rc);
            return false;
        }
        prefetched.buf = b;
        prefetched.fence = f;
        prefetched.w = reqW;
        prefetched.h = reqH;
        prefetched.reqMs = static_cast<double>(std::max<int64_t>(0, monoNs() - t0)) / 1e6;

        // Pre-register new swapchain slots with Go off the critical path so kFrame
        // dispatch right after VSync or kRequest is a single lightweight sendmsg.
        if (gpuMode) {
            OH_NativeBuffer *nb = nullptr;
            if (OH_NativeBuffer_FromNativeWindowBuffer(b, &nb) == 0 && nb != nullptr) {
                uint32_t bid = OH_NativeBuffer_GetSeqNum(nb);
                if (bid == 0) {
                    uintptr_t ptr = reinterpret_cast<uintptr_t>(b);
                    bid = static_cast<uint32_t>(ptr ^ (ptr >> 32)) | 1u;
                }
                OH_NativeBuffer_Config nbCfg = {};
                OH_NativeBuffer_GetConfig(nb, &nbCfg);
                BufferHandle *bh = OH_NativeWindow_GetBufferHandleFromNative(b);
                int32_t bw = nbCfg.width > 0 ? nbCfg.width : (bh != nullptr ? bh->width : 0);
                int32_t bhgt = nbCfg.height > 0 ? nbCfg.height : (bh != nullptr ? bh->height : 0);
                if (bw > 0 && bhgt > 0) {
                    bool alreadySent = false;
                    for (const auto &sb : sentGpuBufs) {
                        if (sb.id == bid && sb.w == bw && sb.h == bhgt) {
                            alreadySent = true;
                            break;
                        }
                    }
                    if (!alreadySent && sendGpuBuffer(winId, bid, bw, bhgt, nb)) {
                        if (sentGpuBufs.size() >= 6) {
                            sentGpuBufs.pop_front();
                        }
                        sentGpuBufs.push_back(SentGpuBuf{bid, bw, bhgt});
                    }
                }
            }
        }
        return true;
    };

    auto drainGpuFences = [&]() {
        for (auto it = pendingGpuFences.begin(); it != pendingGpuFences.end();) {
            struct pollfd pfd = {};
            pfd.fd = it->fd;
            pfd.events = POLLIN;
            int prc = poll(&pfd, 1, 0);
            if (prc > 0 && (pfd.revents & (POLLIN | POLLERR | POLLHUP)) != 0) {
                int64_t tsNs = queryFenceSignalNs(it->fd);
                close(it->fd);
                if (tsNs > 0) {
                    int64_t startNs = std::max(it->submitNs, lastGpuSignalNs);
                    double ms = tsNs > startNs ? std::min(200.0, static_cast<double>(tsNs - startNs) / 1e6) : 0.05;
                    lastGpuSignalNs = std::max(lastGpuSignalNs, tsNs);
                    lastMeasuredGpuMs = ms;
                    for (auto &s : perfWindow) {
                        if (s.sampleSeq == it->sampleSeq) {
                            s.gpuMs = ms;
                            break;
                        }
                    }
                }
                it = pendingGpuFences.erase(it);
            } else if (prc < 0 && errno != EINTR && errno != EAGAIN) {
                close(it->fd);
                it = pendingGpuFences.erase(it);
            } else {
                ++it;
            }
        }
    };

    for (;;) {
        bool pushNow = false;
        bool doPrefetchOnly = false;
        int64_t reqArriveNs = 0;
        {
            std::unique_lock<std::mutex> lk(mu);
            bool woke = cv.wait_for(lk, std::chrono::milliseconds(250), [&] {
                return stopRender || forceResendSurface || targetW != curW || targetH != curH ||
                       fpsDirty || wantFrame || inputPrefetch || forcePerfPush;
            });
            if (stopRender) {
                break;
            }
            if (forcePerfPush) {
                forcePerfPush = false;
                pushNow = true;
            }
            if (!woke) {
                lk.unlock();
                releasePrefetched();
                if (!pendingGpuFences.empty()) {
                    drainGpuFences();
                }
                if (!hudSettled && !perfWindow.empty()) {
                    hudSettled = true;
                    bool vis = gPerfHudEnabled.load(std::memory_order_relaxed);
                    if (vis) {
                        PerfHudData snap = buildPerfHudSnapshot(this, w, h, sc, refresh, aborted, false, true, perfWindow);
                        OH_LOG_INFO(LOG_APP, "win=%{public}u perf[settle] %{public}s | %{public}s | %{public}s",
                                    winId, snap.pacingLine.c_str(), snap.stageLine.c_str(), snap.inputLine.c_str());
                        auto bev = std::make_unique<BridgeEvent>();
                        bev->kind = BridgeEventKind::PerfHud;
                        bev->perfHud = std::move(snap);
                        postBridgeEvent(std::move(bev));
                    }
                }
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
                sentGpuBufs.clear();
                lk.unlock();
                releasePrefetched();
                OH_LOG_INFO(LOG_APP, "win=%{public}u resize %{public}dx%{public}d", winId, w, h);
                sendSurface(winId, w, h, sc, refresh, false);
                lk.lock();
                forceResendSurface = false;
                if (stopRender) {
                    break;
                }
            } else if (forceResendSurface) {
                forceResendSurface = false;
                sc = scale;
                refresh = hz;
                sentGpuBufs.clear();
                lk.unlock();
                releasePrefetched();
                sendSurface(winId, w, h, sc, refresh, true);
                lk.lock();
                if (stopRender) {
                    break;
                }
            }
            if (pushNow && !wantFrame) {
                lk.unlock();
                drainGpuFences();
                bool vis = gPerfHudEnabled.load(std::memory_order_relaxed);
                PerfHudData snap = buildPerfHudSnapshot(this, w, h, sc, refresh, aborted, !hudSettled, vis, perfWindow);
                auto bev = std::make_unique<BridgeEvent>();
                bev->kind = BridgeEventKind::PerfHud;
                bev->perfHud = std::move(snap);
                postBridgeEvent(std::move(bev));
                if (!inputPrefetch) {
                    continue;
                }
                lk.lock();
                if (stopRender) {
                    break;
                }
            }
            if (!wantFrame) {
                if (inputPrefetch) {
                    inputPrefetch = false;
                    if (!uncappedMode) {
                        ensureVsyncArmedLocked();
                    }
                    doPrefetchOnly = true;
                } else {
                    continue;
                }
            } else {
                wantFrame = false;
                inputPrefetch = false;
                frameInFlight = true;
                reqArriveNs = lastWantFrameNs;
                if (!uncappedMode) {
                    ensureVsyncArmedLocked();
                }
            }
        }

        if (doPrefetchOnly) {
            (void)ensureGeometryAndPrefetch(w, h);
            continue;
        }

        struct FrameFlightGuard {
            WinSession *ws;
            ~FrameFlightGuard() {
                std::lock_guard<std::mutex> lk(ws->mu);
                ws->frameInFlight = false;
            }
        } flightGuard{this};

        // Acquire the next swapchain buffer BEFORE waiting on VSync so RequestBuffer
        // and any first-use OH_NativeBuffer IPC serialization overlap with VSync slack.
        if (!ensureGeometryAndPrefetch(w, h)) {
            usleep(4000);
            std::lock_guard<std::mutex> lk(mu);
            if (!stopRender) {
                wantFrame = true;
            }
            continue;
        }

        double vsyncWaitMs = 0.0;
        if (!uncappedMode) {
            std::unique_lock<std::mutex> lk(mu);
            const int64_t vsCheckNs = monoNs();
            const int64_t periodNs = vsyncPeriodHwNs > 0
                ? vsyncPeriodHwNs
                : static_cast<int64_t>(1e9 / std::max(60.0, refresh));
            // Immediate dispatch (0ms VSync wait, matching macOS drawSoon) when:
            //   1. First frame on this surface (lastFlushNs == 0), OR
            //   2. Surface was idle for >= 1 refresh period before input woke it, OR
            //   3. A hardware VSync boundary has already passed since the last FlushBuffer.
            // Only wait when a frame was already flushed in this [VSync_k, VSync_{k+1}) slot.
            const bool needWait = (lastFlushNs > 0) &&
                                  ((vsCheckNs - lastFlushNs) < periodNs) &&
                                  (vsyncSeq <= lastFlushVsyncSeq);
            if (needWait) {
                ensureVsyncArmedLocked();
                const uint64_t waitTargetSeq = lastFlushVsyncSeq + 1;
                const auto maxWait = std::chrono::nanoseconds(std::max<int64_t>(periodNs * 2, 20000000LL));
                cv.wait_for(lk, maxWait, [&] {
                    return stopRender || forceResendSurface ||
                           targetW != curW || targetH != curH ||
                           vsyncSeq >= waitTargetSeq;
                });
                const int64_t afterVsNs = monoNs();
                vsyncWaitMs = static_cast<double>(std::max<int64_t>(0, afterVsNs - vsCheckNs)) / 1e6;
            }
            if (stopRender) {
                break;
            }
            if ((targetW > 0 && targetH > 0 && (targetW != curW || targetH != curH)) || forceResendSurface) {
                wantFrame = true;
                lk.unlock();
                releasePrefetched();
                continue;
            }
            clearReplyFenceLocked();
            reply = BufReply::None;
        } else {
            std::unique_lock<std::mutex> lk(mu);
            if (stopRender) {
                break;
            }
            if ((targetW > 0 && targetH > 0 && (targetW != curW || targetH != curH)) || forceResendSurface) {
                wantFrame = true;
                lk.unlock();
                releasePrefetched();
                continue;
            }
            clearReplyFenceLocked();
            reply = BufReply::None;
        }

        OHNativeWindowBuffer *buf = prefetched.buf;
        int fence = prefetched.fence;
        double reqFenceMs = prefetched.reqMs;
        prefetched = {};

        BufferHandle *bh = OH_NativeWindow_GetBufferHandleFromNative(buf);
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
        int64_t sendFrameNs = 0;

        if (gpuMode) {
            OH_NativeBuffer_Config nbCfg = {};
            if (nb != nullptr) {
                OH_NativeBuffer_GetConfig(nb, &nbCfg);
            }
            int32_t bufW = nbCfg.width > 0 ? nbCfg.width : (bh != nullptr ? bh->width : 0);
            int32_t bufH = nbCfg.height > 0 ? nbCfg.height : (bh != nullptr ? bh->height : 0);
            int32_t bufStride = nbCfg.stride > 0 ? nbCfg.stride : (bh != nullptr ? bh->stride : bufW * 4);
            if (nb == nullptr || bufW <= 0 || bufH <= 0) {
                if (fence >= 0) {
                    close(fence);
                    fence = -1;
                }
                OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
                gpuMode = false;
                sentGpuBufs.clear();
                configureWinUsage(false, w, h);
                std::lock_guard<std::mutex> lk(mu);
                wantFrame = true;
                continue;
            }
            bool alreadySent = false;
            for (const auto &sb : sentGpuBufs) {
                if (sb.id == bufId && sb.w == bufW && sb.h == bufH) {
                    alreadySent = true;
                    break;
                }
            }
            if (!alreadySent) {
                const int64_t ipcStartNs = monoNs();
                if (!sendGpuBuffer(winId, bufId, bufW, bufH, nb)) {
                    if (fence >= 0) {
                        close(fence);
                        fence = -1;
                    }
                    OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
                    continue;
                }
                reqFenceMs += static_cast<double>(std::max<int64_t>(0, monoNs() - ipcStartNs)) / 1e6;
                if (sentGpuBufs.size() >= 6) {
                    sentGpuBufs.pop_front();
                }
                sentGpuBufs.push_back(SentGpuBuf{bufId, bufW, bufH});
            }
            sendFrameNs = monoNs();
            bool sent = sendFrame(winId, curSeq, bufId, bufW, bufH, bufStride, 0, fence);
            if (fence >= 0) {
                close(fence);
                fence = -1;
            }
            if (!sent) {
                OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
                continue;
            }
        } else {
            if (fence >= 0) {
                const int64_t pollStartNs = monoNs();
                struct pollfd pfd = {};
                pfd.fd = fence;
                pfd.events = POLLIN;
                int prc = -1;
                do {
                    prc = poll(&pfd, 1, 3000);
                } while (prc == -1 && (errno == EINTR || errno == EAGAIN));
                close(fence);
                fence = -1;
                reqFenceMs += static_cast<double>(std::max<int64_t>(0, monoNs() - pollStartNs)) / 1e6;
            }
            if (bh == nullptr || bh->fd < 0) {
                OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
                continue;
            }
            struct dma_buf_sync syncStart = {DMA_BUF_SYNC_START | DMA_BUF_SYNC_WRITE};
            (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncStart);
            sendFrameNs = monoNs();
            if (!sendFrame(winId, curSeq, bufId, bh->width, bh->height, bh->stride, bh->size, bh->fd)) {
                struct dma_buf_sync syncEnd = {DMA_BUF_SYNC_END | DMA_BUF_SYNC_WRITE};
                (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncEnd);
                OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
                continue;
            }
        }

        BufReply gotReply = BufReply::None;
        uint32_t rectCount = 0;
        uint32_t uiGLUs = 0, gpuFinishUs = 0, copyUs = 0, perfMeta = 0;
        int releaseFence = -1;
        bool timedOut = false;
        {
            std::unique_lock<std::mutex> lk(mu);
            const int waitMs = (frames == 0) ? 15000 : 5000;
            bool ok = cv.wait_for(lk, std::chrono::milliseconds(waitMs), [&] {
                return stopRender || reply != BufReply::None;
            });
            if (!ok) {
                timedOut = true;
                clearReplyFenceLocked();
            } else {
                gotReply = reply;
                rectCount = replyRectCount;
                uiGLUs = replyUiGLUs;
                gpuFinishUs = replyGpuFinishUs;
                copyUs = replyCopyUs;
                perfMeta = replyPerfMeta;
                releaseFence = replyFenceFd;
                replyFenceFd = -1;
                reply = BufReply::None;
            }
        }

        if (!gpuMode && bh != nullptr && bh->fd >= 0) {
            struct dma_buf_sync syncEnd = {DMA_BUF_SYNC_END | DMA_BUF_SYNC_WRITE};
            (void)ioctl(bh->fd, DMA_BUF_IOCTL_SYNC, &syncEnd);
        }

        if (timedOut) {
            if (releaseFence >= 0) {
                close(releaseFence);
            }
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
            if (releaseFence >= 0) {
                close(releaseFence);
            }
            OH_NativeWindow_NativeWindowAbortBuffer(win, buf);
            if (gotReply == BufReply::Abort) {
                aborted++;
            }
            continue;
        }

        const bool hudVis = gPerfHudEnabled.load(std::memory_order_relaxed);
        if (!pendingGpuFences.empty()) {
            drainGpuFences();
        }
        const int64_t flushStartNs = hudVis ? monoNs() : 0;
        const uint64_t curSampleSeq = hudVis ? nextSampleSeq++ : 0;
        double immediateGpuMs = -1.0;
        if (hudVis && gpuMode && releaseFence >= 0) {
            const int64_t glSubmitNs = std::min<int64_t>(flushStartNs, sendFrameNs + static_cast<int64_t>(uiGLUs) * 1000);
            struct pollfd pfd = {};
            pfd.fd = releaseFence;
            pfd.events = POLLIN;
            if (poll(&pfd, 1, 0) > 0 && (pfd.revents & POLLIN) != 0) {
                int64_t tsNs = queryFenceSignalNs(releaseFence);
                if (tsNs > 0) {
                    int64_t startNs = std::max(glSubmitNs, lastGpuSignalNs);
                    immediateGpuMs = tsNs > startNs ? std::min(200.0, static_cast<double>(tsNs - startNs) / 1e6) : 0.05;
                    lastGpuSignalNs = std::max(lastGpuSignalNs, tsNs);
                    lastMeasuredGpuMs = immediateGpuMs;
                }
            } else {
                int dupFd = dup(releaseFence);
                if (dupFd >= 0) {
                    if (pendingGpuFences.size() >= 8) {
                        close(pendingGpuFences.front().fd);
                        pendingGpuFences.pop_front();
                    }
                    pendingGpuFences.push_back(PendingGpuFence{dupFd, glSubmitNs, curSampleSeq});
                }
            }
        } else if (!gpuMode && releaseFence >= 0) {
            close(releaseFence);
            releaseFence = -1;
        }

        Region region = {nullptr, 0};
        int32_t flushRc = OH_NativeWindow_NativeWindowFlushBuffer(win, buf, gpuMode ? releaseFence : -1, region);
        if (flushRc != 0) {
            OH_LOG_ERROR(LOG_APP, "win=%{public}u FlushBuffer failed rc=%{public}d", winId, flushRc);
            continue;
        }

        const int64_t nowNs = monoNs();
        {
            std::lock_guard<std::mutex> lk(mu);
            lastFlushVsyncSeq = vsyncSeq;
        }

        if (rectCount == 0) {
            whole++;
        } else {
            damaged++;
        }
        frames++;
        if (frames == 1 && winId >= 2) {
            auto fev = std::make_unique<BridgeEvent>();
            fev->kind = BridgeEventKind::FirstFrame;
            fev->winId = winId;
            postBridgeEvent(std::move(fev));
        }

        if (!hudVis) {
            lastFlushNs = nowNs;
            if (frames == 1) {
                OH_LOG_INFO(LOG_APP, "win=%{public}u first frame presented gpuSwap=%{public}d",
                            winId, gpuMode ? 1 : 0);
            }
            continue;
        }

        const double flushMs = static_cast<double>(std::max<int64_t>(0, nowNs - flushStartNs)) / 1e6;
        const double totalDrawMs = static_cast<double>(std::max<int64_t>(0, nowNs - sendFrameNs)) / 1e6;
        if (frames > 1 && sendFrameNs > 0) {
            drawRing.add(totalDrawMs);
        }
        double in2FlushMs = -1.0;
        if (inputSincePresent.exchange(false, std::memory_order_relaxed)) {
            int64_t inNs = lastInputNs.load(std::memory_order_relaxed);
            if (inNs > 0 && nowNs >= inNs && (nowNs - inNs) <= 200000000LL &&
                (reqArriveNs == 0 || reqArriveNs <= inNs + 100000000LL)) {
                in2FlushMs = static_cast<double>(nowNs - inNs) / 1e6;
                inRing.add(in2FlushMs);
            }
        }
        double ivMs = 0.0;
        if (lastFlushNs > 0) {
            double rawIv = static_cast<double>(nowNs - lastFlushNs) / 1e6;
            double idleGapMs = reqArriveNs > lastFlushNs
                ? static_cast<double>(reqArriveNs - lastFlushNs) / 1e6
                : 0.0;
            if (rawIv >= 350.0) {
                perfWindow.clear();
            } else if (idleGapMs < 50.0 && rawIv > 0.2) {
                ivMs = rawIv;
                if (rawIv < 200.0) {
                    intervalRing.add(rawIv);
                }
            }
        }
        lastFlushNs = nowNs;

        PerfFrameSample sample;
        sample.sampleSeq = curSampleSeq;
        sample.dtMs = ivMs;
        sample.zeroCopy = (perfMeta & (1u << 2)) != 0;
        if (uiGLUs > 0 || gpuFinishUs > 0 || copyUs > 0 || sample.zeroCopy) {
            sample.uiGlMs = static_cast<double>(uiGLUs) / 1000.0;
            if (immediateGpuMs >= 0.0) {
                sample.gpuMs = immediateGpuMs;
            } else if (gpuFinishUs > 0) {
                sample.gpuMs = static_cast<double>(gpuFinishUs) / 1000.0;
            } else if (sample.zeroCopy) {
                sample.gpuMs = lastMeasuredGpuMs;
            }
            sample.copyMs = static_cast<double>(copyUs) / 1000.0;
        } else {
            sample.uiGlMs = std::max(0.0, totalDrawMs - flushMs);
        }
        sample.swapMs = reqFenceMs + flushMs;
        sample.vsyncMs = vsyncWaitMs;
        sample.in2FlushMs = in2FlushMs;
        sample.wholeCopy = (rectCount == 0) || ((perfMeta & (1u << 1)) != 0);
        sample.coalesced = static_cast<uint8_t>((perfMeta >> 8) & 0xFFu);
        sample.scrollEvs = static_cast<uint8_t>((perfMeta >> 16) & 0xFFu);
        if (perfWindow.size() >= 120) {
            perfWindow.pop_front();
        }
        perfWindow.push_back(sample);
        hudSettled = false;

        if (pushNow || lastHudPushNs == 0 || (nowNs - lastHudPushNs) >= 150000000LL) {
            lastHudPushNs = nowNs;
            PerfHudData snap = buildPerfHudSnapshot(this, w, h, sc, refresh, aborted, true, true, perfWindow);
            auto bev = std::make_unique<BridgeEvent>();
            bev->kind = BridgeEventKind::PerfHud;
            bev->perfHud = std::move(snap);
            postBridgeEvent(std::move(bev));
        }

        unloggedFrames++;
        if (frames <= 3 || frames % 60 == 0) {
            OH_LOG_INFO(LOG_APP, "win=%{public}u frames=%{public}d damaged=%{public}d whole=%{public}d aborted=%{public}d gpuSwap=%{public}d",
                        winId, frames, damaged, whole, aborted, gpuMode ? 1 : 0);
        }
        if (frames % 60 == 0) {
            logLatencyStats(winId, "periodic", drawRing, inRing, intervalRing);
            PerfHudData snap = buildPerfHudSnapshot(this, w, h, sc, refresh, aborted, true, true, perfWindow);
            OH_LOG_INFO(LOG_APP, "win=%{public}u perf[periodic] %{public}s | %{public}s | %{public}s",
                        winId, snap.pacingLine.c_str(), snap.stageLine.c_str(), snap.inputLine.c_str());
            unloggedFrames = 0;
        }
    }
    releasePrefetched();
    for (const auto &pf : pendingGpuFences) {
        if (pf.fd >= 0) {
            close(pf.fd);
        }
    }
    {
        std::lock_guard<std::mutex> lk(mu);
        clearReplyFenceLocked();
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
        ws->renderRunning = false;
        ws->frameInFlight = false;
        ws->inputPrefetch = false;
        if (ws->reply == BufReply::None) {
            ws->clearReplyFenceLocked();
            ws->reply = BufReply::Reset;
        }
    }
    ws->cv.notify_all();
    if (ws->renderThread.joinable()) {
        ws->renderThread.join();
    }
    OH_NativeVSync *oldVsync = nullptr;
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        oldVsync = ws->vsync;
        ws->vsync = nullptr;
        ws->vsyncArmed = false;
    }
    if (oldVsync != nullptr) {
        OH_NativeVSync_Destroy(oldVsync);
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

    const std::string cfgDir = filesDir + "/.config";
    mkdir(cfgDir.c_str(), 0755);
    const std::string userDirsFile = cfgDir + "/user-dirs.dirs";
    if (access("/storage/Users/currentUser", F_OK) == 0 && access(userDirsFile.c_str(), F_OK) != 0) {
        int ufd = open(userDirsFile.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
        if (ufd >= 0) {
            static const char kUserDirs[] =
                "XDG_DESKTOP_DIR=\"/storage/Users/currentUser/Desktop\"\n"
                "XDG_DOWNLOAD_DIR=\"/storage/Users/currentUser/Download\"\n"
                "XDG_DOCUMENTS_DIR=\"/storage/Users/currentUser/Documents\"\n"
                "XDG_MUSIC_DIR=\"/storage/Users/currentUser/Music\"\n"
                "XDG_PICTURES_DIR=\"/storage/Users/currentUser/Images\"\n"
                "XDG_VIDEOS_DIR=\"/storage/Users/currentUser/Videos\"\n";
            ssize_t ignored = write(ufd, kUserDirs, sizeof(kUserDirs) - 1);
            (void)ignored;
            close(ufd);
        }
    }

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
            "MYGO_OHOS_FIRST_WEEKDAY=", "MYGO_OHOS_DISPLAYS=", "MYGO_OHOS_TEXT_SCALE=",
        };
        for (const char *k : keys) {
            if (strncmp(entry, k, strlen(k)) == 0) {
                return true;
            }
        }
        return false;
    };

    bool hasMygoEnv = false;
    bool isMygoDev = false;
    std::vector<std::string> env;
    if (environ != nullptr) {
        for (char **e = environ; *e != nullptr; ++e) {
            if (strncmp(*e, "MYGO_ENV=", 9) == 0) {
                hasMygoEnv = true;
            } else if (strcmp(*e, "MYGO_DEV=1") == 0) {
                isMygoDev = true;
            }
            if (!overridden(*e)) {
                env.emplace_back(*e);
            }
        }
    }
    if (!hasMygoEnv && !isMygoDev) {
        env.emplace_back("MYGO_ENV=production");
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
    bool onBat = false;
    bool netOn = true;
    bool netMet = false;
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        ce = gCachedEnv;
        onBat = gOnBattery;
        netOn = gNetOnline;
        netMet = gNetMetered;
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
    if (ce.textScaleMilli > 0) {
        char tsBuf[32];
        snprintf(tsBuf, sizeof(tsBuf), "%.3f", static_cast<double>(ce.textScaleMilli) / 1000.0);
        env.push_back(std::string("MYGO_OHOS_TEXT_SCALE=") + tsBuf);
    }
    env.push_back(std::string("MYGO_OHOS_ON_BATTERY=") + (onBat ? "1" : "0"));
    env.push_back(std::string("MYGO_OHOS_NET_ONLINE=") + (netOn ? "1" : "0"));
    env.push_back(std::string("MYGO_OHOS_NET_METERED=") + (netMet ? "1" : "0"));
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

// ---------- Shared-Memory SPSC Audio Output (OH_AudioRenderer) ----------

constexpr uint32_t kAudioHeaderBytes  = 64;
constexpr uint32_t kAudioStateRunning = 0;
constexpr uint32_t kAudioStateClosed  = 1;
constexpr uint32_t kAudioStateError   = 2;

struct AudioShmHeader {
    std::atomic<uint64_t> writeFrames; // [0..7]
    std::atomic<uint64_t> readFrames;  // [8..15]
    uint32_t capFrames;                // [16..19]
    uint32_t targetAhead;              // [20..23]
    std::atomic<uint32_t> state;       // [24..27]
    std::atomic<uint32_t> errCode;     // [28..31]
    std::atomic<uint64_t> heartbeatNs; // [32..39]
    std::atomic<uint32_t> readSeq;     // [40..43]
    uint8_t pad[20];                   // [44..63]
};
static_assert(sizeof(AudioShmHeader) == kAudioHeaderBytes, "AudioShmHeader size mismatch");

inline void wakeAudioProducer(AudioShmHeader *hdr) {
    if (hdr == nullptr) {
        return;
    }
    hdr->readSeq.fetch_add(1, std::memory_order_release);
    syscall(SYS_futex, reinterpret_cast<uint32_t *>(&hdr->readSeq), FUTEX_WAKE, 1, nullptr, nullptr, 0);
}

void sendMediaCmdWire(uint32_t cmd, int64_t posMs) {
    uint8_t wire[12] = {0};
    memcpy(wire + 0, &cmd, 4);
    memcpy(wire + 4, &posMs, 8);
    sendMessage(kMediaCmd, wire, sizeof(wire), -1);
}

struct AudioStreamSession {
    uint32_t streamId = 0;
    uint32_t sampleRate = 48000;
    uint32_t channels = 2;
    uint32_t capFrames = 0;
    bool useFloat32 = true;
    void *shmPtr = nullptr;
    size_t shmBytes = 0;
    AudioShmHeader *hdr = nullptr;
    const float *samples = nullptr;
    OH_AudioRenderer *renderer = nullptr;
    std::atomic<bool> closing{false};
};

std::mutex gAudioMu;
std::unordered_map<uint32_t, std::shared_ptr<AudioStreamSession>> gAudioStreams;

OH_AudioData_Callback_Result OnAudioWriteData(OH_AudioRenderer *, void *userData,
                                              void *audioData, int32_t audioDataSize) {
    auto *as = static_cast<AudioStreamSession *>(userData);
    if (as == nullptr || audioData == nullptr || audioDataSize <= 0) {
        return AUDIO_DATA_CALLBACK_RESULT_INVALID;
    }
    if (as->closing.load(std::memory_order_acquire) || as->hdr == nullptr || as->samples == nullptr) {
        memset(audioData, 0, static_cast<size_t>(audioDataSize));
        return AUDIO_DATA_CALLBACK_RESULT_VALID;
    }

    AudioShmHeader *hdr = as->hdr;
    const uint32_t ch = as->channels;
    const uint32_t cap = as->capFrames;
    if (ch == 0 || cap == 0 || (cap & (cap - 1)) != 0) {
        memset(audioData, 0, static_cast<size_t>(audioDataSize));
        return AUDIO_DATA_CALLBACK_RESULT_VALID;
    }

    const uint32_t bytesPerFrame = ch * (as->useFloat32 ? 4u : 2u);
    const uint32_t wantFrames = static_cast<uint32_t>(audioDataSize) / bytesPerFrame;
    const uint64_t w = hdr->writeFrames.load(std::memory_order_acquire);
    const uint64_t r = hdr->readFrames.load(std::memory_order_relaxed);
    const uint32_t avail = (w > r) ? static_cast<uint32_t>(std::min<uint64_t>(w - r, cap)) : 0u;
    const uint32_t take = std::min<uint32_t>(wantFrames, avail);
    const uint64_t mask = static_cast<uint64_t>(cap - 1);

    if (as->useFloat32) {
        float *dst = static_cast<float *>(audioData);
        if (take > 0) {
            const uint32_t pos = static_cast<uint32_t>(r & mask);
            const uint32_t first = std::min<uint32_t>(take, cap - pos);
            memcpy(dst, as->samples + static_cast<size_t>(pos) * ch, static_cast<size_t>(first) * ch * sizeof(float));
            const uint32_t second = take - first;
            if (second > 0) {
                memcpy(dst + static_cast<size_t>(first) * ch, as->samples,
                       static_cast<size_t>(second) * ch * sizeof(float));
            }
        }
        const size_t filledBytes = static_cast<size_t>(take) * bytesPerFrame;
        if (filledBytes < static_cast<size_t>(audioDataSize)) {
            memset(static_cast<uint8_t *>(audioData) + filledBytes, 0,
                   static_cast<size_t>(audioDataSize) - filledBytes);
        }
    } else {
        int16_t *dst = static_cast<int16_t *>(audioData);
        uint32_t outIdx = 0;
        for (uint32_t f = 0; f < take; ++f) {
            const size_t srcBase = static_cast<size_t>((r + f) & mask) * ch;
            for (uint32_t c = 0; c < ch; ++c) {
                float v = std::clamp(as->samples[srcBase + c], -1.0f, 1.0f);
                dst[outIdx++] = static_cast<int16_t>(std::lrintf(v * 32767.0f));
            }
        }
        const size_t filledBytes = static_cast<size_t>(take) * bytesPerFrame;
        if (filledBytes < static_cast<size_t>(audioDataSize)) {
            memset(static_cast<uint8_t *>(audioData) + filledBytes, 0,
                   static_cast<size_t>(audioDataSize) - filledBytes);
        }
    }

    if (take > 0) {
        hdr->readFrames.store(r + take, std::memory_order_release);
    }
    hdr->heartbeatNs.store(static_cast<uint64_t>(monoNs()), std::memory_order_relaxed);
    if (take > 0 || hdr->state.load(std::memory_order_relaxed) != kAudioStateRunning) {
        wakeAudioProducer(hdr);
    }
    return AUDIO_DATA_CALLBACK_RESULT_VALID;
}

void OnAudioInterrupt(OH_AudioRenderer *renderer, void *userData,
                      OH_AudioInterrupt_ForceType type, OH_AudioInterrupt_Hint hint) {
    auto *as = static_cast<AudioStreamSession *>(userData);
    if (as == nullptr || as->closing.load(std::memory_order_acquire)) {
        return;
    }
    if (hint == AUDIOSTREAM_INTERRUPT_HINT_RESUME) {
        if (renderer != nullptr) {
            OH_AudioRenderer_Start(renderer);
        }
        return;
    }
    if (hint == AUDIOSTREAM_INTERRUPT_HINT_PAUSE) {
        if (type == AUDIOSTREAM_INTERRUPT_SHARE && renderer != nullptr) {
            OH_AudioRenderer_Pause(renderer);
        }
        if (gNowPlayingActive.load(std::memory_order_relaxed) &&
            (gNowPlayingCmds.load(std::memory_order_relaxed) & kMediaPause) != 0) {
            sendMediaCmdWire(kMediaPause, 0);
        }
        return;
    }
    if (hint == AUDIOSTREAM_INTERRUPT_HINT_STOP && as->hdr != nullptr) {
        as->hdr->errCode.store(static_cast<uint32_t>(AUDIOSTREAM_ERROR_ILLEGAL_STATE), std::memory_order_relaxed);
        as->hdr->state.store(kAudioStateError, std::memory_order_release);
        wakeAudioProducer(as->hdr);
    }
}

void OnAudioOutputDeviceChange(OH_AudioRenderer *, void *userData,
                               OH_AudioStream_DeviceChangeReason reason) {
    auto *as = static_cast<AudioStreamSession *>(userData);
    if (as == nullptr || as->closing.load(std::memory_order_acquire)) {
        return;
    }
    if (reason == REASON_OLD_DEVICE_UNAVAILABLE &&
        gNowPlayingActive.load(std::memory_order_relaxed) &&
        (gNowPlayingCmds.load(std::memory_order_relaxed) & kMediaPause) != 0) {
        sendMediaCmdWire(kMediaPause, 0);
    }
}

void OnAudioError(OH_AudioRenderer *, void *userData, OH_AudioStream_Result error) {
    auto *as = static_cast<AudioStreamSession *>(userData);
    if (as == nullptr || as->hdr == nullptr) {
        return;
    }
    OH_LOG_ERROR(LOG_APP, "OH_AudioRenderer error=%{public}d stream=%{public}u",
                 static_cast<int>(error), as->streamId);
    as->hdr->errCode.store(static_cast<uint32_t>(error), std::memory_order_relaxed);
    as->hdr->state.store(kAudioStateError, std::memory_order_release);
    wakeAudioProducer(as->hdr);
}

void teardownAudioSession(std::shared_ptr<AudioStreamSession> sp) {
    if (!sp) {
        return;
    }
    sp->closing.store(true, std::memory_order_release);
    if (sp->hdr != nullptr) {
        sp->hdr->state.store(kAudioStateClosed, std::memory_order_release);
        wakeAudioProducer(sp->hdr);
    }
    if (sp->renderer != nullptr) {
        OH_AudioRenderer_Stop(sp->renderer);
        OH_AudioRenderer_Release(sp->renderer);
        sp->renderer = nullptr;
    }
    if (sp->shmPtr != nullptr && sp->shmBytes > 0) {
        munmap(sp->shmPtr, sp->shmBytes);
        sp->shmPtr = nullptr;
        sp->hdr = nullptr;
        sp->samples = nullptr;
    }
}

void closeAudioStream(uint32_t streamId) {
    std::shared_ptr<AudioStreamSession> sp;
    {
        std::lock_guard<std::mutex> lk(gAudioMu);
        auto it = gAudioStreams.find(streamId);
        if (it != gAudioStreams.end()) {
            sp = std::move(it->second);
            gAudioStreams.erase(it);
        }
    }
    teardownAudioSession(std::move(sp));
}

void closeAllAudioStreams() {
    std::unordered_map<uint32_t, std::shared_ptr<AudioStreamSession>> all;
    {
        std::lock_guard<std::mutex> lk(gAudioMu);
        all.swap(gAudioStreams);
    }
    for (auto &kv : all) {
        teardownAudioSession(std::move(kv.second));
    }
}

void openAudioStream(uint32_t streamId, uint32_t sampleRate, uint32_t channels,
                     uint32_t latencyMode, uint32_t shmBytes, int shmFd) {
    if (shmFd < 0) {
        return;
    }
    if (shmBytes <= kAudioHeaderBytes || sampleRate == 0 || channels == 0 || channels > 8) {
        close(shmFd);
        return;
    }
    void *mapped = mmap(nullptr, shmBytes, PROT_READ | PROT_WRITE, MAP_SHARED, shmFd, 0);
    close(shmFd);
    if (mapped == MAP_FAILED || mapped == nullptr) {
        OH_LOG_ERROR(LOG_APP, "audio mmap failed bytes=%{public}u errno=%{public}d", shmBytes, errno);
        return;
    }

    auto sp = std::make_shared<AudioStreamSession>();
    sp->streamId = streamId;
    sp->sampleRate = sampleRate;
    sp->channels = channels;
    sp->shmPtr = mapped;
    sp->shmBytes = shmBytes;
    sp->hdr = static_cast<AudioShmHeader *>(mapped);
    sp->capFrames = sp->hdr->capFrames;
    sp->samples = reinterpret_cast<const float *>(static_cast<const uint8_t *>(mapped) + kAudioHeaderBytes);

    const size_t neededBytes = kAudioHeaderBytes + static_cast<size_t>(sp->capFrames) * channels * sizeof(float);
    if (sp->capFrames == 0 || (sp->capFrames & (sp->capFrames - 1)) != 0 || neededBytes > shmBytes) {
        sp->hdr->errCode.store(static_cast<uint32_t>(AUDIOSTREAM_ERROR_INVALID_PARAM), std::memory_order_relaxed);
        sp->hdr->state.store(kAudioStateError, std::memory_order_release);
        wakeAudioProducer(sp->hdr);
        teardownAudioSession(std::move(sp));
        return;
    }

    auto tryGenerate = [&](OH_AudioStream_SampleFormat fmt, OH_AudioRenderer **outRen) -> OH_AudioStream_Result {
        OH_AudioStreamBuilder *builder = nullptr;
        OH_AudioStream_Result res = OH_AudioStreamBuilder_Create(&builder, AUDIOSTREAM_TYPE_RENDERER);
        if (res != AUDIOSTREAM_SUCCESS || builder == nullptr) {
            return res == AUDIOSTREAM_SUCCESS ? AUDIOSTREAM_ERROR_SYSTEM : res;
        }
        OH_AudioStreamBuilder_SetSamplingRate(builder, static_cast<int32_t>(sampleRate));
        OH_AudioStreamBuilder_SetChannelCount(builder, static_cast<int32_t>(channels));
        OH_AudioStreamBuilder_SetSampleFormat(builder, fmt);
        OH_AudioStreamBuilder_SetEncodingType(builder, AUDIOSTREAM_ENCODING_TYPE_RAW);
        OH_AudioStreamBuilder_SetRendererInfo(builder, AUDIOSTREAM_USAGE_MUSIC);
        OH_AudioStreamBuilder_SetLatencyMode(builder, latencyMode == 1 ? AUDIOSTREAM_LATENCY_MODE_FAST
                                                                       : AUDIOSTREAM_LATENCY_MODE_NORMAL);
        OH_AudioStreamBuilder_SetRendererWriteDataCallback(builder, OnAudioWriteData, sp.get());
        OH_AudioStreamBuilder_SetRendererInterruptCallback(builder, OnAudioInterrupt, sp.get());
        OH_AudioStreamBuilder_SetRendererOutputDeviceChangeCallback(builder, OnAudioOutputDeviceChange, sp.get());
        OH_AudioStreamBuilder_SetRendererErrorCallback(builder, OnAudioError, sp.get());
        res = OH_AudioStreamBuilder_GenerateRenderer(builder, outRen);
        OH_AudioStreamBuilder_Destroy(builder);
        return res;
    };

    OH_AudioRenderer *ren = nullptr;
    OH_AudioStream_Result res = tryGenerate(AUDIOSTREAM_SAMPLE_F32LE, &ren);
    if (res == AUDIOSTREAM_SUCCESS && ren != nullptr) {
        sp->useFloat32 = true;
    } else {
        res = tryGenerate(AUDIOSTREAM_SAMPLE_S16LE, &ren);
        sp->useFloat32 = false;
    }
    if (res != AUDIOSTREAM_SUCCESS || ren == nullptr) {
        OH_LOG_ERROR(LOG_APP, "OH_AudioStreamBuilder_GenerateRenderer failed res=%{public}d", static_cast<int>(res));
        sp->hdr->errCode.store(static_cast<uint32_t>(res), std::memory_order_relaxed);
        sp->hdr->state.store(kAudioStateError, std::memory_order_release);
        wakeAudioProducer(sp->hdr);
        teardownAudioSession(std::move(sp));
        return;
    }
    sp->renderer = ren;

    closeAudioStream(streamId);
    {
        std::lock_guard<std::mutex> lk(gAudioMu);
        gAudioStreams[streamId] = sp;
    }

    res = OH_AudioRenderer_Start(ren);
    if (res != AUDIOSTREAM_SUCCESS) {
        OH_LOG_ERROR(LOG_APP, "OH_AudioRenderer_Start failed res=%{public}d", static_cast<int>(res));
        sp->hdr->errCode.store(static_cast<uint32_t>(res), std::memory_order_relaxed);
        sp->hdr->state.store(kAudioStateError, std::memory_order_release);
        wakeAudioProducer(sp->hdr);
        closeAudioStream(streamId);
        return;
    }
    OH_LOG_INFO(LOG_APP, "audio stream=%{public}u started rate=%{public}u ch=%{public}u f32=%{public}d cap=%{public}u",
                streamId, sampleRate, channels, sp->useFloat32 ? 1 : 0, sp->capFrames);
}

void killEngine() {
    closeAllAudioStreams();
    clearAllHotkeys();
    {
        std::lock_guard<std::mutex> lk(gDragMu);
        gActiveHostDrag = ActiveHostDrag{};
    }
    {
        std::lock_guard<std::mutex> lk(gDragReplyMu);
        gDragReplies.clear();
    }
    gDragReplyCv.notify_all();
    gNowPlayingActive.store(false, std::memory_order_relaxed);
    gNowPlayingCmds.store(0, std::memory_order_relaxed);
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

int recvMessage(uint8_t *kind, std::vector<uint8_t> *payload, int *outFd, int timeoutMs) {
    *outFd = -1;
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

    uint8_t hdr[5] = {0};
    struct iovec iov;
    iov.iov_base = hdr;
    iov.iov_len = sizeof(hdr);
    char cbuf[CMSG_SPACE(sizeof(int))];
    memset(cbuf, 0, sizeof(cbuf));
    struct msghdr mh;
    memset(&mh, 0, sizeof(mh));
    mh.msg_iov = &iov;
    mh.msg_iovlen = 1;
    mh.msg_control = cbuf;
    mh.msg_controllen = sizeof(cbuf);

    ssize_t r = 0;
    for (;;) {
        r = recvmsg(fd, &mh, MSG_CMSG_CLOEXEC);
        if (r < 0 && errno == EINTR) {
            continue;
        }
        break;
    }
    if (r <= 0) {
        return -1;
    }
    for (struct cmsghdr *cm = CMSG_FIRSTHDR(&mh); cm != nullptr; cm = CMSG_NXTHDR(&mh, cm)) {
        if (cm->cmsg_level == SOL_SOCKET && cm->cmsg_type == SCM_RIGHTS &&
            cm->cmsg_len >= CMSG_LEN(sizeof(int))) {
            int passed = -1;
            memcpy(&passed, CMSG_DATA(cm), sizeof(int));
            *outFd = passed;
            break;
        }
    }
    if (static_cast<size_t>(r) < sizeof(hdr)) {
        if (!readFull(fd, hdr + r, sizeof(hdr) - static_cast<size_t>(r))) {
            if (*outFd >= 0) {
                close(*outFd);
                *outFd = -1;
            }
            return -1;
        }
    }

    uint32_t len = 0;
    memcpy(&len, hdr + 1, 4);
    if (len > (64u << 20)) {
        if (*outFd >= 0) {
            close(*outFd);
            *outFd = -1;
        }
        return -1;
    }
    *kind = hdr[0];
    payload->resize(len);
    if (len > 0 && !readFull(fd, payload->data(), len)) {
        if (*outFd >= 0) {
            close(*outFd);
            *outFd = -1;
        }
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

bool decodeClipWrite(const std::vector<uint8_t> &p, ClipWriteData *out) {
    if (p.size() < 4) {
        return false;
    }
    out->clear = p[0] != 0;
    out->concealed = p[1] != 0;
    size_t off = 4;
    return readStr(p, off, &out->text) &&
           readStr(p, off, &out->html) &&
           readStr(p, off, &out->uriList) &&
           readBytes(p, off, &out->png) &&
           off == p.size();
}

bool decodeClipRead(const std::vector<uint8_t> &p, uint32_t *reqId, std::vector<std::string> *fmts) {
    size_t off = 0;
    uint32_t count = 0;
    if (!readU32(p, off, reqId) || !readU32(p, off, &count) || count > 128) {
        return false;
    }
    fmts->clear();
    fmts->reserve(count);
    for (uint32_t i = 0; i < count; ++i) {
        std::string s;
        if (!readStr(p, off, &s)) {
            return false;
        }
        fmts->push_back(std::move(s));
    }
    return off == p.size();
}

bool decodeFileDialog(const std::vector<uint8_t> &p, FileDialogData *out) {
    if (p.size() < 12) {
        return false;
    }
    memcpy(&out->reqId, p.data() + 0, 4);
    memcpy(&out->winId, p.data() + 4, 4);
    out->flags = p[8];
    size_t off = 12;
    uint32_t fCount = 0;
    if (!readStr(p, off, &out->title) ||
        !readStr(p, off, &out->defaultPath) ||
        !readStr(p, off, &out->buttonLabel) ||
        !readU32(p, off, &fCount) || fCount > 64) {
        return false;
    }
    out->filters.clear();
    out->filters.reserve(fCount);
    for (uint32_t i = 0; i < fCount; ++i) {
        FileFilterData ff;
        uint32_t eCount = 0;
        if (!readStr(p, off, &ff.name) || !readU32(p, off, &eCount) || eCount > 256) {
            return false;
        }
        ff.extensions.reserve(eCount);
        for (uint32_t j = 0; j < eCount; ++j) {
            std::string ext;
            if (!readStr(p, off, &ext)) {
                return false;
            }
            ff.extensions.push_back(std::move(ext));
        }
        out->filters.push_back(std::move(ff));
    }
    return off == p.size();
}

bool decodeMessageBox(const std::vector<uint8_t> &p, MessageBoxData *out) {
    if (p.size() < 20) {
        return false;
    }
    memcpy(&out->reqId, p.data() + 0, 4);
    memcpy(&out->winId, p.data() + 4, 4);
    memcpy(&out->defaultId, p.data() + 8, 4);
    memcpy(&out->cancelId, p.data() + 12, 4);
    out->checkboxChecked = p[16] != 0;
    size_t off = 20;
    uint32_t bCount = 0;
    if (!readStr(p, off, &out->boxType) ||
        !readStr(p, off, &out->title) ||
        !readStr(p, off, &out->message) ||
        !readStr(p, off, &out->detail) ||
        !readStr(p, off, &out->checkboxLabel) ||
        !readU32(p, off, &bCount) || bCount > 32) {
        return false;
    }
    out->buttons.clear();
    out->buttons.reserve(bCount);
    for (uint32_t i = 0; i < bCount; ++i) {
        std::string s;
        if (!readStr(p, off, &s)) {
            return false;
        }
        out->buttons.push_back(std::move(s));
    }
    uint32_t dCount = 0;
    if (!readU32(p, off, &dCount) || dCount > 32) {
        return false;
    }
    out->destructiveIds.clear();
    out->destructiveIds.reserve(dCount);
    for (uint32_t i = 0; i < dCount; ++i) {
        uint32_t u = 0;
        if (!readU32(p, off, &u)) {
            return false;
        }
        out->destructiveIds.push_back(static_cast<int32_t>(u));
    }
    return off == p.size();
}

void sendDialogFallbackResult(uint32_t reqId, int32_t resp, const std::string &errStr) {
    std::vector<uint8_t> wire(12, 0);
    memcpy(wire.data() + 0, &reqId, 4);
    memcpy(wire.data() + 4, &resp, 4);
    appendStr(wire, errStr);
    appendU32(wire, 0);
    sendMessage(kDialogResult, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void sendShellResult(uint32_t reqId, const std::string &errStr) {
    std::vector<uint8_t> wire;
    appendU32(wire, reqId);
    appendStr(wire, errStr);
    sendMessage(kShellResult, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void sendClipDataFallback(uint32_t reqId, const std::string &errStr) {
    std::vector<uint8_t> wire;
    appendU32(wire, reqId);
    appendStr(wire, errStr);
    appendU32(wire, 0);
    sendMessage(kClipData, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

void sendNotifResultWire(uint32_t reqId, bool denied, const std::string &errStr) {
    std::vector<uint8_t> wire(8, 0);
    memcpy(wire.data() + 0, &reqId, 4);
    wire[4] = denied ? 1 : 0;
    appendStr(wire, errStr);
    sendMessage(kNotifResult, wire.data(), static_cast<uint32_t>(wire.size()), -1);
}

bool decodeNowPlaying(const std::vector<uint8_t> &p, NowPlayingData *out) {
    if (p.size() < 32) {
        return false;
    }
    out->active = p[0] != 0;
    out->state = p[1];
    out->metaChanged = p[2] != 0;
    memcpy(&out->commands, p.data() + 4, 4);
    memcpy(&out->durationMs, p.data() + 8, 8);
    memcpy(&out->positionMs, p.data() + 16, 8);
    memcpy(&out->rate, p.data() + 24, 8);
    size_t off = 32;
    if (!readStr(p, off, &out->title) ||
        !readStr(p, off, &out->artist) ||
        !readStr(p, off, &out->album) ||
        !readBytes(p, off, &out->artwork)) {
        return false;
    }
    return off == p.size();
}

bool decodeNotifShow(const std::vector<uint8_t> &p, NotifShowData *out) {
    if (p.size() < 8) {
        return false;
    }
    memcpy(&out->reqId, p.data() + 0, 4);
    out->silent = p[4] != 0;
    size_t off = 8;
    if (!readStr(p, off, &out->id) ||
        !readStr(p, off, &out->title) ||
        !readStr(p, off, &out->subtitle) ||
        !readStr(p, off, &out->body) ||
        !readStr(p, off, &out->group) ||
        !readBytes(p, off, &out->image)) {
        return false;
    }
    return off == p.size();
}

bool decodePowerReq(const std::vector<uint8_t> &p, PowerReqData *out) {
    if (p.size() < 4) {
        return false;
    }
    out->keepSystem = p[0] != 0;
    out->keepDisplay = p[1] != 0;
    out->keepRunning = p[2] != 0;
    size_t off = 4;
    if (!readStr(p, off, &out->reason)) {
        return false;
    }
    return off == p.size();
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
            ws->clearReplyFenceLocked();
            ws->reply = BufReply::Reset;
            ws->wantFrame = false;
        }
        ws->cv.notify_all();
    }
}

void sendInitialStateToEngine() {
    std::vector<uint8_t> envWire;
    std::vector<uint8_t> clipWire;
    std::vector<std::vector<uint8_t>> appOpens;
    std::vector<std::vector<uint8_t>> notifClicks;
    bool onBat = false;
    bool netOn = true;
    bool netMet = false;
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        envWire = gCachedEnv.wirePayload;
        clipWire = gCachedClipWire;
        appOpens.swap(gPendingAppOpens);
        notifClicks.swap(gPendingNotifClicks);
        onBat = gOnBattery;
        netOn = gNetOnline;
        netMet = gNetMetered;
    }
    if (!envWire.empty()) {
        sendMessage(kEnvInfo, envWire.data(), static_cast<uint32_t>(envWire.size()), -1);
    }
    if (!clipWire.empty()) {
        sendMessage(kClipChanged, clipWire.data(), static_cast<uint32_t>(clipWire.size()), -1);
    }
    {
        std::vector<uint8_t> pwrWire(4, 0);
        pwrWire[0] = onBat ? 1 : 0;
        appendU32(pwrWire, 0);
        sendMessage(kPowerState, pwrWire.data(), static_cast<uint32_t>(pwrWire.size()), -1);
    }
    {
        uint8_t netWire[4] = {static_cast<uint8_t>(netOn ? 1 : 0), static_cast<uint8_t>(netMet ? 1 : 0), 0, 0};
        sendMessage(kNetState, netWire, sizeof(netWire), -1);
    }
    for (const auto &ao : appOpens) {
        if (!ao.empty()) {
            sendMessage(kAppOpen, ao.data(), static_cast<uint32_t>(ao.size()), -1);
        }
    }
    for (const auto &nc : notifClicks) {
        if (!nc.empty()) {
            sendMessage(kNotifClicked, nc.data(), static_cast<uint32_t>(nc.size()), -1);
        }
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
        int inFd = -1;
        int got = recvMessage(&kind, &payload, &inFd, 250);
        if (got == 0) {
            continue;
        }
        if (got < 0) {
            closeAllAudioStreams();
            return !gSupervisorRunning.load() || gEngineCleanQuit.load();
        }
        if (inFd >= 0 && kind != kAudioOpen && kind != kPresent) {
            close(inFd);
            inFd = -1;
        }

        switch (kind) {
        case kQuit: {
            gEngineCleanQuit.store(true);
            closeAllAudioStreams();
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
                const int64_t nowNs = monoNs();
                {
                    std::lock_guard<std::mutex> lk(ws->mu);
                    if (!ws->wantFrame) {
                        ws->lastWantFrameNs = nowNs;
                    }
                    ws->wantFrame = true;
                    ws->ensureVsyncArmedLocked();
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
                uint32_t uiGLUs = 0, gpuFinishUs = 0, copyUs = 0, perfMeta = 0;
                if (rects <= 4096) {
                    size_t perfOff = 12 + static_cast<size_t>(rects) * 16;
                    if (payload.size() >= perfOff + 16) {
                        memcpy(&uiGLUs, payload.data() + perfOff + 0, 4);
                        memcpy(&gpuFinishUs, payload.data() + perfOff + 4, 4);
                        memcpy(&copyUs, payload.data() + perfOff + 8, 4);
                        memcpy(&perfMeta, payload.data() + perfOff + 12, 4);
                    }
                }
                auto ws = findSession(wid > 0 ? wid : 1);
                if (ws) {
                    {
                        std::lock_guard<std::mutex> lk(ws->mu);
                        ws->clearReplyFenceLocked();
                        ws->reply = BufReply::Present;
                        ws->replySeq = seq;
                        ws->replyRectCount = rects;
                        ws->replyUiGLUs = uiGLUs;
                        ws->replyGpuFinishUs = gpuFinishUs;
                        ws->replyCopyUs = copyUs;
                        ws->replyPerfMeta = perfMeta;
                        ws->replyFenceFd = inFd;
                        inFd = -1;
                    }
                    ws->cv.notify_all();
                }
            }
            if (inFd >= 0) {
                close(inFd);
                inFd = -1;
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
                        ws->clearReplyFenceLocked();
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
                uint32_t wid = 0;
                memcpy(&wid, payload.data() + 0, 4);
                uint8_t act = payload[4];
                if (act == kWinActEnableWebLayer) {
                    auto ws = getOrCreateSession(wid > 0 ? wid : 1);
                    ws->webLayerEnabled.store(true, std::memory_order_relaxed);
                    if (ws->uiContext != nullptr) {
                        OH_ArkUI_PostUITask(ws->uiContext,
                                            reinterpret_cast<void *>(static_cast<uintptr_t>(ws->winId)),
                                            enableWebLayerOnUI);
                    }
                    break;
                }
                if (act == kWinActFocusSurface) {
                    auto ws = findSession(wid > 0 ? wid : 1);
                    if (ws && ws->uiContext != nullptr) {
                        OH_ArkUI_PostUITask(ws->uiContext,
                                            reinterpret_cast<void *>(static_cast<uintptr_t>(ws->winId)),
                                            focusSurfaceOnUI);
                    }
                    break;
                }
                if (act == kWinActPerfHUDOn || act == kWinActPerfHUDOff || act == kWinActPerfHUDToggle) {
                    int mode = (act == kWinActPerfHUDToggle) ? -1 : (act == kWinActPerfHUDOn ? 1 : 0);
                    setPerfHudEnabled(wid, mode);
                    break;
                }
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::WinAction;
                ev->winId = wid;
                ev->action = act;
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
        case kClipWrite: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::ClipWrite;
            if (decodeClipWrite(payload, &ev->clipWrite)) {
                postBridgeEvent(std::move(ev));
            }
            break;
        }
        case kClipRead: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::ClipRead;
            if (decodeClipRead(payload, &ev->reqId, &ev->strList)) {
                postBridgeEvent(std::move(ev));
            } else if (payload.size() >= 4) {
                uint32_t reqId = 0;
                memcpy(&reqId, payload.data(), 4);
                sendClipDataFallback(reqId, "ohos: malformed clipboard request");
            }
            break;
        }
        case kDialogOpen:
        case kDialogSave: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = (kind == kDialogOpen) ? BridgeEventKind::DialogOpen : BridgeEventKind::DialogSave;
            if (decodeFileDialog(payload, &ev->fileDlg)) {
                postBridgeEvent(std::move(ev));
            } else if (payload.size() >= 4) {
                uint32_t reqId = 0;
                memcpy(&reqId, payload.data(), 4);
                sendDialogFallbackResult(reqId, 0, "ohos: malformed file dialog request");
            }
            break;
        }
        case kDialogMsg: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::DialogMsg;
            if (decodeMessageBox(payload, &ev->msgBox)) {
                postBridgeEvent(std::move(ev));
            } else if (payload.size() >= 4) {
                uint32_t reqId = 0;
                memcpy(&reqId, payload.data(), 4);
                sendDialogFallbackResult(reqId, -1, "ohos: malformed message box request");
            }
            break;
        }
        case kShellReq: {
            if (payload.size() >= 8) {
                size_t off = 8;
                std::string target;
                if (readStr(payload, off, &target) && off == payload.size()) {
                    auto ev = std::make_unique<BridgeEvent>();
                    ev->kind = BridgeEventKind::ShellReq;
                    memcpy(&ev->reqId, payload.data(), 4);
                    ev->shellOp = payload[4];
                    ev->shellTarget = std::move(target);
                    postBridgeEvent(std::move(ev));
                } else {
                    uint32_t reqId = 0;
                    memcpy(&reqId, payload.data(), 4);
                    sendShellResult(reqId, "ohos: malformed shell request");
                }
            }
            break;
        }
        case kAudioOpen: {
            if (payload.size() == 20 && inFd >= 0) {
                uint32_t streamId = 0, rate = 0, ch = 0, latMode = 0, shmBytes = 0;
                memcpy(&streamId, payload.data() + 0, 4);
                memcpy(&rate, payload.data() + 4, 4);
                memcpy(&ch, payload.data() + 8, 4);
                memcpy(&latMode, payload.data() + 12, 4);
                memcpy(&shmBytes, payload.data() + 16, 4);
                openAudioStream(streamId, rate, ch, latMode, shmBytes, inFd);
                inFd = -1;
            } else if (inFd >= 0) {
                close(inFd);
                inFd = -1;
            }
            break;
        }
        case kAudioClose: {
            if (payload.size() == 4) {
                uint32_t streamId = 0;
                memcpy(&streamId, payload.data(), 4);
                closeAudioStream(streamId);
            }
            break;
        }
        case kNowPlaying: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::NowPlaying;
            if (decodeNowPlaying(payload, &ev->nowPlaying)) {
                gNowPlayingActive.store(ev->nowPlaying.active, std::memory_order_relaxed);
                gNowPlayingCmds.store(ev->nowPlaying.active ? ev->nowPlaying.commands : 0u, std::memory_order_relaxed);
                postBridgeEvent(std::move(ev));
            }
            break;
        }
        case kNotifShow: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::NotifShow;
            if (decodeNotifShow(payload, &ev->notifShow)) {
                postBridgeEvent(std::move(ev));
            } else if (payload.size() >= 4) {
                uint32_t reqId = 0;
                memcpy(&reqId, payload.data(), 4);
                sendNotifResultWire(reqId, false, "ohos: malformed notification request");
            }
            break;
        }
        case kNotifRemove: {
            if (payload.size() >= 4) {
                size_t off = 4;
                std::string id;
                if (readStr(payload, off, &id) && off == payload.size()) {
                    auto ev = std::make_unique<BridgeEvent>();
                    ev->kind = BridgeEventKind::NotifRemove;
                    ev->notifRemoveAll = payload[0] != 0;
                    ev->notifRemoveId = std::move(id);
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kPowerReq: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::PowerReq;
            if (decodePowerReq(payload, &ev->powerReq)) {
                postBridgeEvent(std::move(ev));
            }
            break;
        }
        case kNetWatch: {
            auto ev = std::make_unique<BridgeEvent>();
            ev->kind = BridgeEventKind::NetWatch;
            postBridgeEvent(std::move(ev));
            break;
        }
        case kCookieReq: {
            if (payload.size() >= 12) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::CookieReq;
                memcpy(&ev->cookieReq.reqId, payload.data() + 0, 4);
                ev->cookieReq.op = payload[4];
                ev->cookieReq.incognito = payload[5] != 0;
                memcpy(&ev->cookieReq.siteDataTypes, payload.data() + 8, 4);
                size_t off = 12;
                if (readStr(payload, off, &ev->cookieReq.url) &&
                    readStr(payload, off, &ev->cookieReq.headerOrName) &&
                    readStr(payload, off, &ev->cookieReq.domain) &&
                    readStr(payload, off, &ev->cookieReq.path) &&
                    off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kWebReq: {
            if (payload.size() >= 8) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::WebReq;
                memcpy(&ev->winId, payload.data() + 0, 4);
                ev->webOp = payload[4];
                size_t off = 8;
                if (readStr(payload, off, &ev->webArg1) &&
                    readStr(payload, off, &ev->webArg2) &&
                    off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kWebSetup: {
            if (payload.size() >= 24) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::WebSetup;
                auto &ws = ev->webSetup;
                memcpy(&ws.webId, payload.data() + 0, 4);
                memcpy(&ws.hostWinId, payload.data() + 4, 4);
                memcpy(&ws.flags, payload.data() + 8, 4);
                memcpy(&ws.bgRGBA, payload.data() + 12, 4);
                memcpy(&ws.zoom, payload.data() + 16, 8);
                size_t off = 24;
                uint32_t scCount = 0, usCount = 0;
                bool ok = readStr(payload, off, &ws.ua) && readU32(payload, off, &scCount) && scCount <= 64;
                for (uint32_t i = 0; ok && i < scCount; ++i) {
                    std::string sc;
                    if (!readStr(payload, off, &sc)) {
                        ok = false;
                        break;
                    }
                    ws.schemes.push_back(std::move(sc));
                }
                if (ok) {
                    ok = readU32(payload, off, &usCount) && usCount <= 256;
                }
                for (uint32_t i = 0; ok && i < usCount; ++i) {
                    if (off + 4 > payload.size()) {
                        ok = false;
                        break;
                    }
                    uint8_t sf = payload[off];
                    off += 4;
                    UserScriptData ud;
                    ud.atDocEnd = (sf & 1) != 0;
                    ud.allFrames = (sf & 2) != 0;
                    if (!readStr(payload, off, &ud.source)) {
                        ok = false;
                        break;
                    }
                    ws.scripts.push_back(std::move(ud));
                }
                if (ok && off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kPlaceWebViews: {
            if (payload.size() >= 12) {
                uint32_t wid = 0, vc = 0, rc = 0;
                memcpy(&wid, payload.data() + 0, 4);
                memcpy(&vc, payload.data() + 4, 4);
                memcpy(&rc, payload.data() + 8, 4);
                if (vc <= 1024 && rc <= 8192 && 12u + vc * 60u + rc * 16u == payload.size()) {
                    auto ev = std::make_unique<BridgeEvent>();
                    ev->kind = BridgeEventKind::PlaceWebViews;
                    auto &pw = ev->placeWebViews;
                    pw.winId = wid;
                    pw.views.resize(vc);
                    pw.surfaceRegions.resize(rc);
                    size_t off = 12;
                    for (uint32_t i = 0; i < vc; ++i) {
                        auto &v = pw.views[i];
                        memcpy(&v.webId, payload.data() + off + 0, 4);
                        memcpy(&v.frameX, payload.data() + off + 4, 4);
                        memcpy(&v.frameY, payload.data() + off + 8, 4);
                        memcpy(&v.frameW, payload.data() + off + 12, 4);
                        memcpy(&v.frameH, payload.data() + off + 16, 4);
                        memcpy(&v.clipX, payload.data() + off + 20, 4);
                        memcpy(&v.clipY, payload.data() + off + 24, 4);
                        memcpy(&v.clipW, payload.data() + off + 28, 4);
                        memcpy(&v.clipH, payload.data() + off + 32, 4);
                        memcpy(&v.pageW, payload.data() + off + 36, 4);
                        memcpy(&v.pageH, payload.data() + off + 40, 4);
                        memcpy(&v.r0, payload.data() + off + 44, 4);
                        memcpy(&v.r1, payload.data() + off + 48, 4);
                        memcpy(&v.r2, payload.data() + off + 52, 4);
                        memcpy(&v.r3, payload.data() + off + 56, 4);
                        off += 60;
                    }
                    for (uint32_t i = 0; i < rc; ++i) {
                        auto &r = pw.surfaceRegions[i];
                        memcpy(&r.x, payload.data() + off + 0, 4);
                        memcpy(&r.y, payload.data() + off + 4, 4);
                        memcpy(&r.w, payload.data() + off + 8, 4);
                        memcpy(&r.h, payload.data() + off + 12, 4);
                        off += 16;
                    }
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kWebAsyncReq: {
            if (payload.size() >= 60) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::WebAsyncReq;
                auto &ar = ev->webAsyncReq;
                memcpy(&ar.reqId, payload.data() + 0, 4);
                memcpy(&ar.webId, payload.data() + 4, 4);
                ar.op = payload[8];
                uint8_t pflags = payload[9];
                ar.landscape = (pflags & 1) != 0;
                ar.background = (pflags & 2) != 0;
                memcpy(&ar.pageWidth, payload.data() + 12, 8);
                memcpy(&ar.pageHeight, payload.data() + 20, 8);
                memcpy(&ar.marginTop, payload.data() + 28, 8);
                memcpy(&ar.marginRight, payload.data() + 36, 8);
                memcpy(&ar.marginBottom, payload.data() + 44, 8);
                memcpy(&ar.marginLeft, payload.data() + 52, 8);
                size_t off = 60;
                if (readStr(payload, off, &ar.body) && off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kSchemeRes: {
            if (payload.size() >= 12) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::SchemeRes;
                auto &sr = ev->schemeRes;
                memcpy(&sr.reqId, payload.data() + 0, 4);
                sr.op = payload[4];
                memcpy(&sr.status, payload.data() + 8, 4);
                size_t off = 12;
                uint32_t hc = 0;
                bool ok = readU32(payload, off, &hc) && hc <= 512;
                for (uint32_t i = 0; ok && i < hc; ++i) {
                    SchemeHeaderData hd;
                    if (!readStr(payload, off, &hd.name) || !readStr(payload, off, &hd.headerValue)) {
                        ok = false;
                        break;
                    }
                    sr.headers.push_back(std::move(hd));
                }
                if (ok && readBytes(payload, off, &sr.chunk) && readStr(payload, off, &sr.err) && off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kSecretReq:
            handleSecretReq(payload);
            break;
        case kHotkeyReq:
            handleHotkeyReq(payload);
            break;
        case kTrayUpdate: {
            if (payload.size() >= 8) {
                auto ev = std::make_unique<BridgeEvent>();
                ev->kind = BridgeEventKind::TrayUpdate;
                auto &tu = ev->trayUpdate;
                memcpy(&tu.trayId, payload.data() + 0, 4);
                tu.op = payload[4];
                uint8_t fl = payload[5];
                tu.hasImage = (fl & 1) != 0;
                tu.templateImg = (fl & 2) != 0;
                tu.hasMenu = (fl & 4) != 0;
                size_t off = 8;
                if (readStr(payload, off, &tu.id) &&
                    readStr(payload, off, &tu.title) &&
                    readStr(payload, off, &tu.tooltip) &&
                    readBytes(payload, off, &tu.png) &&
                    decodePopupSlice(payload, off, &tu.menuItems) &&
                    off == payload.size()) {
                    postBridgeEvent(std::move(ev));
                }
            }
            break;
        }
        case kDragSetFormats:
            break; // All drop data types are enabled per-window; filtering happens in Go Offer matching.
        case kDragStart:
            handleDragStart(payload);
            break;
        case kDragCancel:
            handleDragCancel(payload);
            break;
        case kDragReply:
            handleDragReply(payload);
            break;
        case kAccessTree:
            handleAccessTree(payload);
            break;
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
    OH_NativeVSync *newVsync = nullptr;
    if (ws->arkWindowId > 0) {
        newVsync = OH_NativeVSync_Create_ForAssociatedWindow(static_cast<uint64_t>(ws->arkWindowId), "mygo", 4);
    }
    if (newVsync == nullptr) {
        newVsync = OH_NativeVSync_Create("mygo", 4);
    }
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->nativeWindow = win;
        ws->vsync = newVsync;
        ws->vsyncArmed = false;
        ws->vsyncIdleTicks = 0;
        ws->frameInFlight = false;
        ws->inputPrefetch = false;
        ws->curW = w;
        ws->curH = h;
        ws->targetW = w;
        ws->targetH = h;
        ws->stopRender = false;
        ws->reply = BufReply::None;
        ws->fpsDirty = true;
        ws->renderRunning = true;
    }
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

void unmountWindowXComponent(const std::shared_ptr<WinSession> &ws, bool eraseTree) {
    if (!ws) {
        return;
    }
    if (attachedImeWinId == ws->winId) {
        detachIME();
    }
    stopWinRenderThread(ws);
    disposeActiveDragAction(ws.get());
    detachWindowAccessibility(ws.get(), eraseTree);

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
                NODE_ON_DRAG_ENTER, NODE_ON_DRAG_MOVE, NODE_ON_DRAG_LEAVE, NODE_ON_DROP,
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
    ws->xcIsTexture = false;
}

bool mountWindowXComponent(const std::shared_ptr<WinSession> &ws, bool layered) {
    if (!ws || ws->nodeContent == nullptr || nodeAPI == nullptr) {
        return false;
    }
    if (layered) {
        ws->xcNode = nodeAPI->createNode(ARKUI_NODE_XCOMPONENT_TEXTURE);
        ws->xcIsTexture = (ws->xcNode != nullptr);
    }
    if (ws->xcNode == nullptr) {
        ws->xcNode = nodeAPI->createNode(ARKUI_NODE_XCOMPONENT);
        ws->xcIsTexture = false;
    }
    if (ws->xcNode == nullptr) {
        return false;
    }

    ArkUI_NumberValue fullPct[] = {{.f32 = 1.0f}};
    ArkUI_AttributeItem pctItem = {fullPct, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_WIDTH_PERCENT, &pctItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_HEIGHT_PERCENT, &pctItem);

    ArkUI_NumberValue fitVal[] = {{.i32 = ARKUI_RENDER_FIT_RESIZE_FILL}};
    ArkUI_AttributeItem fitItem = {fitVal, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_RENDER_FIT, &fitItem);

    ArkUI_NumberValue bgVal[] = {{.u32 = layered ? 0x00000000u : ws->bgARGB}};
    ArkUI_AttributeItem bgItem = {bgVal, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_BACKGROUND_COLOR, &bgItem);

    ArkUI_NumberValue oneVal[] = {{.i32 = 1}};
    ArkUI_AttributeItem oneItem = {oneVal, 1, nullptr, nullptr};
    nodeAPI->setAttribute(ws->xcNode, NODE_FOCUSABLE, &oneItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_DEFAULT_FOCUS, &oneItem);
    nodeAPI->setAttribute(ws->xcNode, NODE_FOCUS_ON_TOUCH, &oneItem);
    OH_ArkUI_XComponent_SetNeedSoftKeyboard(ws->xcNode, true);

    ws->surfaceHolder = OH_ArkUI_SurfaceHolder_Create(ws->xcNode);
    ws->surfaceCallback = OH_ArkUI_SurfaceCallback_Create();
    if (ws->surfaceHolder == nullptr || ws->surfaceCallback == nullptr) {
        unmountWindowXComponent(ws, false);
        return false;
    }

    ArkUI_XComponentSurfaceConfig *cfg = OH_ArkUI_XComponentSurfaceConfig_Create();
    if (cfg != nullptr) {
        OH_ArkUI_XComponentSurfaceConfig_SetIsOpaque(cfg, !layered);
        OH_ArkUI_SurfaceHolder_SetSurfaceConfig(ws->surfaceHolder, cfg);
        OH_ArkUI_XComponentSurfaceConfig_Dispose(cfg);
    }

    gHolderToWinId[ws->surfaceHolder] = ws->winId;
    gNodeToWinId[ws->xcNode] = ws->winId;

    OH_ArkUI_SurfaceCallback_SetSurfaceCreatedEvent(ws->surfaceCallback, OnSurfaceCreated);
    OH_ArkUI_SurfaceCallback_SetSurfaceChangedEvent(ws->surfaceCallback, OnSurfaceChanged);
    OH_ArkUI_SurfaceCallback_SetSurfaceDestroyedEvent(ws->surfaceCallback, OnSurfaceDestroyed);
    OH_ArkUI_SurfaceCallback_SetSurfaceShowEvent(ws->surfaceCallback, OnSurfaceShow);
    OH_ArkUI_SurfaceCallback_SetSurfaceHideEvent(ws->surfaceCallback, OnSurfaceHide);
    OH_ArkUI_SurfaceHolder_AddSurfaceCallback(ws->surfaceHolder, ws->surfaceCallback);

    nodeAPI->addNodeEventReceiver(ws->xcNode, OnNodeEvent);
    OH_ArkUI_AllowNodeAllDropDataTypes(ws->xcNode);
    OH_ArkUI_SetDragEventStrictReportWithNode(ws->xcNode, true);
    static const ArkUI_NodeEventType kEvents[] = {
        NODE_TOUCH_EVENT, NODE_ON_MOUSE, NODE_ON_HOVER_EVENT,
        NODE_ON_AXIS, NODE_ON_COASTING_AXIS_EVENT, NODE_ON_KEY_EVENT,
        NODE_ON_FOCUS, NODE_ON_BLUR,
        NODE_ON_DRAG_ENTER, NODE_ON_DRAG_MOVE, NODE_ON_DRAG_LEAVE, NODE_ON_DROP,
    };
    for (ArkUI_NodeEventType ev : kEvents) {
        nodeAPI->registerNodeEvent(ws->xcNode, ev, static_cast<int32_t>(ev), nullptr);
    }
    attachWindowAccessibility(ws.get());

    OH_ArkUI_NodeContent_AddNode(ws->nodeContent, ws->xcNode);
    if (ws->uiContext != nullptr) {
        OH_ArkUI_FocusSetKeyProcessingMode(ws->uiContext, ARKUI_KEY_PROCESSING_MODE_FOCUS_ANCESTOR_EVENT);
        OH_ArkUI_FocusActivate(ws->uiContext, false, true);
    }
    OH_ArkUI_FocusRequest(ws->xcNode);
    return true;
}

void teardownWindowNode(uint32_t winId, bool eraseSession) {
    auto ws = findSession(winId);
    if (!ws) {
        return;
    }
    unmountWindowXComponent(ws, eraseSession);
    ws->nodeContent = nullptr;
    ws->uiContext = nullptr;

    if (eraseSession) {
        std::lock_guard<std::mutex> lk(gWinMu);
        gWindows.erase(winId);
    }
    if (winId == 1 && eraseSession) {
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
    size_t argc = 9;
    napi_value argv[9] = {};
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
    if (argc >= 9 && argv[8] != nullptr) {
        double ts = 1.0;
        if (napi_get_value_double(env, argv[8], &ts) == napi_ok && std::isfinite(ts) && ts > 0.0) {
            ce.textScaleMilli = static_cast<uint32_t>(std::clamp(std::lround(ts * 1000.0), 250L, 4000L));
        }
    }

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
    uint8_t tail[8] = {0};
    memcpy(tail + 0, &ce.textScaleMilli, 4);
    tail[4] = 1; // hasPrefs
    wire.insert(wire.end(), tail, tail + 8);
    ce.wirePayload = wire;

    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        gCachedEnv = std::move(ce);
    }
    sendMessage(kEnvInfo, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value BindWindow(napi_env env, napi_callback_info info) {
    size_t argc = 8;
    napi_value argv[8] = {};
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
    uint32_t bgARGB = dark ? 0xFF1C1C1Fu : 0xFFF4F4F6u;
    if (argc >= 8 && argv[7] != nullptr) {
        uint32_t parsedBg = 0;
        if (napi_get_value_uint32(env, argv[7], &parsedBg) == napi_ok && parsedBg != 0) {
            bgARGB = parsedBg;
        }
    }
    ws->bgARGB = bgARGB;
    {
        std::lock_guard<std::mutex> lk(ws->mu);
        ws->scale = scale > 0 ? scale : 1.0;
        double nextHz = hz > 0 ? hz : 120.0;
        if (ws->hz != nextHz) {
            ws->hz = nextHz;
            ws->fpsDirty = true;
        }
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

    const bool layered = ws->webLayerEnabled.load(std::memory_order_relaxed);
    if (!mountWindowXComponent(ws, layered)) {
        teardownWindowNode(winId, winId != 1);
        return nullptr;
    }

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
    if ((op >= kWinFocus && op <= kWinClosed) || op == kWinQuitRequest) {
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

napi_value NotifyClipChanged(napi_env env, napi_callback_info info) {
    size_t argc = 3;
    napi_value argv[3] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 3) {
        return nullptr;
    }
    uint32_t changeCount = 0;
    bool external = true;
    napi_get_value_uint32(env, argv[0], &changeCount);
    napi_get_value_bool(env, argv[1], &external);

    std::vector<std::string> mimes;
    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[2], &arrLen) == napi_ok) {
        for (uint32_t i = 0; i < arrLen && i < 128; ++i) {
            napi_value elem = nullptr;
            if (napi_get_element(env, argv[2], i, &elem) == napi_ok && elem != nullptr) {
                std::string m = getNapiString(env, elem);
                if (!m.empty()) {
                    mimes.push_back(std::move(m));
                }
            }
        }
    }

    std::vector<uint8_t> wire(8, 0);
    memcpy(wire.data() + 0, &changeCount, 4);
    wire[4] = external ? 1 : 0;
    appendU32(wire, static_cast<uint32_t>(mimes.size()));
    for (const auto &m : mimes) {
        appendStr(wire, m);
    }

    if (external) {
        std::lock_guard<std::mutex> lk(gEnvMu);
        gCachedClipWire = wire;
    }
    sendMessage(kClipChanged, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value ResolveClipRead(napi_env env, napi_callback_info info) {
    size_t argc = 3;
    napi_value argv[3] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 3) {
        return nullptr;
    }
    uint32_t reqId = 0;
    napi_get_value_uint32(env, argv[0], &reqId);
    std::string errStr = getNapiString(env, argv[1]);

    struct RepEntry {
        std::string fmt;
        const uint8_t *ptr = nullptr;
        size_t len = 0;
    };
    std::vector<RepEntry> reps;
    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[2], &arrLen) == napi_ok) {
        for (uint32_t i = 0; i < arrLen && i < 128; ++i) {
            napi_value obj = nullptr;
            if (napi_get_element(env, argv[2], i, &obj) != napi_ok || obj == nullptr) {
                continue;
            }
            RepEntry re;
            re.fmt = getPropString(env, obj, "format");
            if (re.fmt.empty()) {
                continue;
            }
            napi_value dataVal = nullptr;
            if (napi_get_named_property(env, obj, "data", &dataVal) == napi_ok && dataVal != nullptr) {
                bool isAb = false;
                if (napi_is_arraybuffer(env, dataVal, &isAb) == napi_ok && isAb) {
                    void *raw = nullptr;
                    size_t byteLen = 0;
                    if (napi_get_arraybuffer_info(env, dataVal, &raw, &byteLen) == napi_ok) {
                        re.ptr = static_cast<const uint8_t *>(raw);
                        re.len = byteLen;
                    }
                }
            }
            reps.push_back(re);
        }
    }

    std::vector<uint8_t> wire;
    appendU32(wire, reqId);
    appendStr(wire, errStr);
    appendU32(wire, static_cast<uint32_t>(reps.size()));
    for (const auto &r : reps) {
        appendStr(wire, r.fmt);
        appendBytes(wire, r.ptr, r.len);
    }
    sendMessage(kClipData, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value ResolveDialog(napi_env env, napi_callback_info info) {
    size_t argc = 5;
    napi_value argv[5] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 5) {
        return nullptr;
    }
    uint32_t reqId = 0;
    int32_t response = 0;
    bool checked = false;
    napi_get_value_uint32(env, argv[0], &reqId);
    napi_get_value_int32(env, argv[1], &response);
    napi_get_value_bool(env, argv[2], &checked);
    std::string errStr = getNapiString(env, argv[3]);

    std::vector<std::string> paths;
    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[4], &arrLen) == napi_ok) {
        for (uint32_t i = 0; i < arrLen && i < 4096; ++i) {
            napi_value elem = nullptr;
            if (napi_get_element(env, argv[4], i, &elem) == napi_ok && elem != nullptr) {
                std::string p = getNapiString(env, elem);
                if (!p.empty()) {
                    paths.push_back(std::move(p));
                }
            }
        }
    }

    std::vector<uint8_t> wire(12, 0);
    memcpy(wire.data() + 0, &reqId, 4);
    memcpy(wire.data() + 4, &response, 4);
    wire[8] = checked ? 1 : 0;
    appendStr(wire, errStr);
    appendU32(wire, static_cast<uint32_t>(paths.size()));
    for (const auto &p : paths) {
        appendStr(wire, p);
    }
    sendMessage(kDialogResult, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value ResolveShell(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    uint32_t reqId = 0;
    napi_get_value_uint32(env, argv[0], &reqId);
    std::string errStr = getNapiString(env, argv[1]);
    sendShellResult(reqId, errStr);
    return nullptr;
}

napi_value NotifyAppOpen(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    uint32_t op = 0;
    napi_get_value_uint32(env, argv[0], &op);

    std::vector<std::string> items;
    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[1], &arrLen) == napi_ok) {
        for (uint32_t i = 0; i < arrLen && i < 256; ++i) {
            napi_value elem = nullptr;
            if (napi_get_element(env, argv[1], i, &elem) == napi_ok && elem != nullptr) {
                std::string s = getNapiString(env, elem);
                if (!s.empty()) {
                    items.push_back(std::move(s));
                }
            }
        }
    }

    std::vector<uint8_t> wire(4, 0);
    wire[0] = static_cast<uint8_t>(op);
    appendU32(wire, static_cast<uint32_t>(items.size()));
    for (const auto &s : items) {
        appendStr(wire, s);
    }
    if (!sendMessage(kAppOpen, wire.data(), static_cast<uint32_t>(wire.size()), -1)) {
        std::lock_guard<std::mutex> lk(gEnvMu);
        if (gPendingAppOpens.size() < 32) {
            gPendingAppOpens.push_back(std::move(wire));
        }
    }
    return nullptr;
}

napi_value NotifyMediaCmd(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    uint32_t cmd = 0;
    double posMsD = 0;
    napi_get_value_uint32(env, argv[0], &cmd);
    napi_get_value_double(env, argv[1], &posMsD);
    sendMediaCmdWire(cmd, static_cast<int64_t>(std::llround(posMsD)));
    return nullptr;
}

napi_value ResolveNotif(napi_env env, napi_callback_info info) {
    size_t argc = 3;
    napi_value argv[3] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 3) {
        return nullptr;
    }
    uint32_t reqId = 0;
    bool denied = false;
    napi_get_value_uint32(env, argv[0], &reqId);
    napi_get_value_bool(env, argv[1], &denied);
    std::string errStr = getNapiString(env, argv[2]);
    sendNotifResultWire(reqId, denied, errStr);
    return nullptr;
}

napi_value NotifyNotifClicked(napi_env env, napi_callback_info info) {
    size_t argc = 1;
    napi_value argv[1] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 1) {
        return nullptr;
    }
    std::string id = getNapiString(env, argv[0]);
    if (id.empty()) {
        return nullptr;
    }
    std::vector<uint8_t> wire;
    appendStr(wire, id);
    if (!sendMessage(kNotifClicked, wire.data(), static_cast<uint32_t>(wire.size()), -1)) {
        std::lock_guard<std::mutex> lk(gEnvMu);
        if (gPendingNotifClicks.size() < 32) {
            gPendingNotifClicks.push_back(std::move(wire));
        }
    }
    return nullptr;
}

napi_value NotifyPowerState(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    bool onBat = false;
    napi_get_value_bool(env, argv[0], &onBat);
    std::string evStr = getNapiString(env, argv[1]);
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        gOnBattery = onBat;
    }
    std::vector<uint8_t> wire(4, 0);
    wire[0] = onBat ? 1 : 0;
    appendStr(wire, evStr);
    sendMessage(kPowerState, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value NotifyNetState(napi_env env, napi_callback_info info) {
    size_t argc = 2;
    napi_value argv[2] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 2) {
        return nullptr;
    }
    bool online = true;
    bool metered = false;
    napi_get_value_bool(env, argv[0], &online);
    napi_get_value_bool(env, argv[1], &metered);
    {
        std::lock_guard<std::mutex> lk(gEnvMu);
        gNetOnline = online;
        gNetMetered = metered;
    }
    uint8_t wire[4] = {static_cast<uint8_t>(online ? 1 : 0), static_cast<uint8_t>(metered ? 1 : 0), 0, 0};
    sendMessage(kNetState, wire, sizeof(wire), -1);
    return nullptr;
}

napi_value ResolveCookie(napi_env env, napi_callback_info info) {
    size_t argc = 3;
    napi_value argv[3] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 3) {
        return nullptr;
    }
    uint32_t reqId = 0;
    napi_get_value_uint32(env, argv[0], &reqId);
    std::string errStr = getNapiString(env, argv[1]);

    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[2], &arrLen) != napi_ok) {
        arrLen = 0;
    }
    if (arrLen > 4096) {
        arrLen = 4096;
    }

    std::vector<uint8_t> wire(4, 0);
    memcpy(wire.data(), &reqId, 4);
    appendStr(wire, errStr);
    appendU32(wire, arrLen);
    for (uint32_t i = 0; i < arrLen; ++i) {
        napi_value cObj = nullptr;
        if (napi_get_element(env, argv[2], i, &cObj) != napi_ok || cObj == nullptr) {
            uint8_t emptyHdr[12] = {0};
            wire.insert(wire.end(), emptyHdr, emptyHdr + 12);
            for (int k = 0; k < 5; ++k) {
                appendStr(wire, "");
            }
            continue;
        }
        int64_t expUnix = static_cast<int64_t>(std::llround(getPropDouble(env, cObj, "expiresUnix", 0.0)));
        uint8_t flags = 0;
        if (getPropBool(env, cObj, "secure")) {
            flags |= 1;
        }
        if (getPropBool(env, cObj, "httpOnly")) {
            flags |= 2;
        }
        uint8_t hdr[12] = {0};
        memcpy(hdr, &expUnix, 8);
        hdr[8] = flags;
        wire.insert(wire.end(), hdr, hdr + 12);
        appendStr(wire, getPropString(env, cObj, "name"));
        appendStr(wire, getPropString(env, cObj, "value"));
        appendStr(wire, getPropString(env, cObj, "domain"));
        appendStr(wire, getPropString(env, cObj, "path"));
        appendStr(wire, getPropString(env, cObj, "sameSite"));
    }
    sendMessage(kCookieResult, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value NotifyWebEvent(napi_env env, napi_callback_info info) {
    size_t argc = 7;
    napi_value argv[7] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 7) {
        return nullptr;
    }
    uint32_t winId = 0, evCode = 0;
    bool canBack = false, canFwd = false;
    int32_t errCode = 0;
    napi_get_value_uint32(env, argv[0], &winId);
    napi_get_value_uint32(env, argv[1], &evCode);
    napi_get_value_bool(env, argv[2], &canBack);
    napi_get_value_bool(env, argv[3], &canFwd);
    napi_get_value_int32(env, argv[4], &errCode);
    std::string text = getNapiString(env, argv[5]);
    std::string desc = getNapiString(env, argv[6]);

    std::vector<uint8_t> wire(12, 0);
    memcpy(wire.data() + 0, &winId, 4);
    wire[4] = static_cast<uint8_t>(evCode);
    wire[5] = canBack ? 1 : 0;
    wire[6] = canFwd ? 1 : 0;
    memcpy(wire.data() + 8, &errCode, 4);
    appendStr(wire, text);
    appendStr(wire, desc);
    sendMessage(kWebEvent, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value ResolveWebAsync(napi_env env, napi_callback_info info) {
    size_t argc = 4;
    napi_value argv[4] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 4) {
        return nullptr;
    }
    uint32_t reqId = 0;
    napi_get_value_uint32(env, argv[0], &reqId);
    std::string text = getNapiString(env, argv[1]);
    std::string errStr = getNapiString(env, argv[2]);
    const uint8_t *ptr = nullptr;
    size_t byteLen = 0;
    bool isAb = false;
    if (napi_is_arraybuffer(env, argv[3], &isAb) == napi_ok && isAb) {
        void *raw = nullptr;
        if (napi_get_arraybuffer_info(env, argv[3], &raw, &byteLen) == napi_ok) {
            ptr = static_cast<const uint8_t *>(raw);
        }
    }

    std::vector<uint8_t> wire;
    appendU32(wire, reqId);
    appendStr(wire, text);
    appendStr(wire, errStr);
    appendBytes(wire, ptr, byteLen);
    sendMessage(kWebAsyncRes, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value NotifySchemeReq(napi_env env, napi_callback_info info) {
    size_t argc = 7;
    napi_value argv[7] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 7) {
        return nullptr;
    }
    uint32_t reqId = 0, webId = 0;
    bool cancel = false;
    napi_get_value_uint32(env, argv[0], &reqId);
    napi_get_value_uint32(env, argv[1], &webId);
    napi_get_value_bool(env, argv[2], &cancel);
    std::string method = getNapiString(env, argv[3]);
    std::string url = getNapiString(env, argv[4]);

    std::vector<SchemeHeaderData> hdrs;
    uint32_t arrLen = 0;
    if (napi_get_array_length(env, argv[5], &arrLen) == napi_ok) {
        for (uint32_t i = 0; i < arrLen && i < 512; ++i) {
            napi_value obj = nullptr;
            if (napi_get_element(env, argv[5], i, &obj) != napi_ok || obj == nullptr) {
                continue;
            }
            SchemeHeaderData hd;
            hd.name = getPropString(env, obj, "name");
            hd.headerValue = getPropString(env, obj, "headerValue");
            if (!hd.name.empty()) {
                hdrs.push_back(std::move(hd));
            }
        }
    }

    const uint8_t *bodyPtr = nullptr;
    size_t bodyLen = 0;
    bool isAb = false;
    if (napi_is_arraybuffer(env, argv[6], &isAb) == napi_ok && isAb) {
        void *raw = nullptr;
        if (napi_get_arraybuffer_info(env, argv[6], &raw, &bodyLen) == napi_ok) {
            bodyPtr = static_cast<const uint8_t *>(raw);
        }
    }

    std::vector<uint8_t> wire(12, 0);
    memcpy(wire.data() + 0, &reqId, 4);
    memcpy(wire.data() + 4, &webId, 4);
    wire[8] = cancel ? 1 : 0;
    appendStr(wire, method);
    appendStr(wire, url);
    appendU32(wire, static_cast<uint32_t>(hdrs.size()));
    for (const auto &h : hdrs) {
        appendStr(wire, h.name);
        appendStr(wire, h.headerValue);
    }
    appendBytes(wire, bodyPtr, bodyLen);
    sendMessage(kSchemeReq, wire.data(), static_cast<uint32_t>(wire.size()), -1);
    return nullptr;
}

napi_value ClearWebControllerHandler(napi_env env, napi_callback_info info) {
    size_t argc = 1;
    napi_value argv[1] = {nullptr};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 1 || argv[0] == nullptr) {
        return nullptr;
    }
    napi_value fn = nullptr;
    if (napi_get_named_property(env, argv[0], "setWebController", &fn) != napi_ok || fn == nullptr) {
        return nullptr;
    }
    napi_value nullVal = nullptr;
    napi_get_null(env, &nullVal);
    napi_call_function(env, argv[0], fn, 1, &nullVal, nullptr);
    return nullptr;
}

napi_value NotifyTrayEvent(napi_env env, napi_callback_info info) {
    size_t argc = 3;
    napi_value argv[3] = {};
    napi_get_cb_info(env, info, &argc, argv, nullptr, nullptr);
    if (argc < 3) {
        return nullptr;
    }
    uint32_t trayId = 0;
    int32_t ev = 0, menuItemId = 0;
    napi_get_value_uint32(env, argv[0], &trayId);
    napi_get_value_int32(env, argv[1], &ev);
    napi_get_value_int32(env, argv[2], &menuItemId);
    uint8_t wire[12] = {0};
    memcpy(wire + 0, &trayId, 4);
    wire[4] = static_cast<uint8_t>(ev & 0xFF);
    memcpy(wire + 8, &menuItemId, 4);
    sendMessage(kTrayEvent, wire, sizeof(wire), -1);
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
        {"notifyClipChanged", nullptr, NotifyClipChanged, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveClipRead", nullptr, ResolveClipRead, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveDialog", nullptr, ResolveDialog, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveShell", nullptr, ResolveShell, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyAppOpen", nullptr, NotifyAppOpen, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyMediaCmd", nullptr, NotifyMediaCmd, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveNotif", nullptr, ResolveNotif, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyNotifClicked", nullptr, NotifyNotifClicked, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyPowerState", nullptr, NotifyPowerState, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyNetState", nullptr, NotifyNetState, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveCookie", nullptr, ResolveCookie, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyWebEvent", nullptr, NotifyWebEvent, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"resolveWebAsync", nullptr, ResolveWebAsync, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifySchemeReq", nullptr, NotifySchemeReq, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"clearWebControllerHandler", nullptr, ClearWebControllerHandler, nullptr, nullptr, nullptr, napi_default, nullptr},
        {"notifyTrayEvent", nullptr, NotifyTrayEvent, nullptr, nullptr, nullptr, napi_default, nullptr},
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
