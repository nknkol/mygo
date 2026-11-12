import host, {
  HostMenuItem,
  HostMessageBox,
  HostPlacedWebView,
  HostSurfaceRegion,
  HostUserScript,
  HostWebAsyncReq
} from 'libmygo_host.so';
import { ComponentContent, LengthMetrics, NodeContent, UIContext, display } from '@kit.ArkUI';
import { webview } from '@kit.ArkWeb';
import { BusinessError } from '@kit.BasicServicesKit';
import {
  BridgeWebCmd,
  ManagedWebState,
  WEB_ASYNC_CAPTURE,
  WEB_ASYNC_EVAL,
  WEB_ASYNC_PRINT_PDF,
  WEB_EV_AUDIO_STATE,
  WEB_EV_DOWNLOAD_START,
  WEB_EV_EMBEDDED_PRESS,
  WEB_EV_EMBEDDED_SCROLL,
  WEB_EV_ERROR,
  WEB_EV_MESSAGE,
  WEB_EV_PAGE_BEGIN,
  WEB_EV_PAGE_END,
  WEB_EV_PERM_REQ,
  WEB_EV_RENDER_EXITED,
  WEB_EV_TAB_OUT,
  WEB_EV_TITLE,
  WEB_EV_UNHANDLED_KEY,
  WEB_EV_WINDOW_CLOSE,
  WEB_EV_WINDOW_NEW,
  WEB_OP_BACK,
  WEB_OP_CLEAR_CACHE,
  WEB_OP_DEVTOOLS,
  WEB_OP_EVAL,
  WEB_OP_FOCUS,
  WEB_OP_FORWARD,
  WEB_OP_LOAD_HTML,
  WEB_OP_LOAD_URL,
  WEB_OP_PERM_REPLY,
  WEB_OP_RELOAD,
  WEB_OP_SET_BG,
  WEB_OP_SET_MUTED,
  WEB_OP_SET_UA,
  WEB_OP_SET_ZOOM,
  WEB_OP_STOP,
  WEB_OP_TAB_INTO,
  WEB_SETUP_AUTOPLAY,
  WEB_SETUP_INCOGNITO,
  WEB_SETUP_PINCH_ZOOM,
  WEB_SETUP_SCROLL_TO_CONTAINER,
  hostBridge
} from '../bridge/HostBridge';

const MENU_KIND_SEPARATOR: number = 1;
const MENU_KIND_SUBMENU: number = 2;
const MENU_KIND_CHECKBOX: number = 3;
const MENU_KIND_RADIO: number = 4;

class PendingWebPerm {
  isGeo: boolean = false;
  origin: string = '';
  rawResources: string[] = [];
  permReq: PermissionRequest | undefined = undefined;
  geoReq: JsGeolocation | undefined = undefined;
}

class ArkWebJsProxy {
  private owner: ArkWebDriver;

  constructor(owner: ArkWebDriver) {
    this.owner = owner;
  }

  postMessage(msg: string): void {
    this.owner.onJsMessage(msg);
  }

  asyncReply(reqId: number, ok: boolean, payload: string): void {
    this.owner.settleEval(reqId, ok ? payload : '', ok ? '' : payload);
  }

  tabOut(shiftFlag: number): void {
    this.owner.onJsTabOut(shiftFlag !== 0);
  }

  unhandledKey(modsBits: number, domKey: string, domCode: string): void {
    this.owner.onJsUnhandledKey(modsBits, domKey, domCode);
  }

  wheelScroll(modsBits: number, clientX: number, clientY: number, deltaX: number, deltaY: number): void {
    this.owner.onJsWheelScroll(modsBits, clientX, clientY, deltaX, deltaY);
  }
}

function buildBridgeShimScript(embedded: boolean, scrollToContainer: boolean): string {
  let js: string =
    '(function(){\n' +
    '  if (window !== window.top) return;\n' +
    '  if (!window.webkit) window.webkit = {};\n' +
    '  if (!window.webkit.messageHandlers) window.webkit.messageHandlers = {};\n' +
    '  window.webkit.messageHandlers.mygo = {\n' +
    '    postMessage: function(msg) {\n' +
    '      if (window.__mygoArkWeb && typeof window.__mygoArkWeb.postMessage === "function") {\n' +
    '        window.__mygoArkWeb.postMessage(typeof msg === "string" ? msg : JSON.stringify(msg));\n' +
    '      }\n' +
    '    }\n' +
    '  };\n';

  if (embedded) {
    js +=
      '  if (window.__mygoEmbeddedInstalled) return;\n' +
      '  window.__mygoEmbeddedInstalled = true;\n' +
      '  var focusSelector = \'a[href],button:not([disabled]),input:not([disabled]):not([type="hidden"]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"]),[contenteditable]:not([contenteditable="false"])\';\n' +
      '  function visibleFocusables() {\n' +
      '    var list = Array.prototype.slice.call(document.querySelectorAll(focusSelector));\n' +
      '    return list.filter(function(el) {\n' +
      '      if (!el || typeof el.getClientRects !== "function") return false;\n' +
      '      var st = window.getComputedStyle(el);\n' +
      '      return st.visibility !== "hidden" && st.display !== "none" && el.getClientRects().length > 0;\n' +
      '    });\n' +
      '  }\n' +
      '  window.__mygoFocusEdge = function(last) {\n' +
      '    var items = visibleFocusables();\n' +
      '    if (items.length > 0) {\n' +
      '      var t = last ? items[items.length - 1] : items[0];\n' +
      '      if (t && typeof t.focus === "function") t.focus();\n' +
      '    }\n' +
      '  };\n' +
      '  window.addEventListener("keydown", function(ev) {\n' +
      '    if (ev.defaultPrevented || ev.isComposing) return;\n' +
      '    if (ev.key === "Tab" && !ev.ctrlKey && !ev.altKey && !ev.metaKey) {\n' +
      '      var items = visibleFocusables();\n' +
      '      var active = document.activeElement;\n' +
      '      var atStart = items.length === 0 || active === document.body || active === document.documentElement || active === items[0];\n' +
      '      var atEnd = items.length === 0 || active === items[items.length - 1];\n' +
      '      if ((ev.shiftKey && atStart) || (!ev.shiftKey && atEnd)) {\n' +
      '        ev.preventDefault();\n' +
      '        if (window.__mygoArkWeb && typeof window.__mygoArkWeb.tabOut === "function") {\n' +
      '          window.__mygoArkWeb.tabOut(ev.shiftKey ? 1 : 0);\n' +
      '        }\n' +
      '        return;\n' +
      '      }\n' +
      '    }\n' +
      '  }, true);\n' +
      '  window.addEventListener("keydown", function(ev) {\n' +
      '    if (ev.defaultPrevented || ev.isComposing || ev.key === "Tab") return;\n' +
      '    var t = ev.target;\n' +
      '    var editable = false;\n' +
      '    if (t && t.tagName) {\n' +
      '      var tag = t.tagName.toUpperCase();\n' +
      '      editable = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || !!t.isContentEditable;\n' +
      '    }\n' +
      '    var hasMod = ev.ctrlKey || ev.metaKey || ev.altKey;\n' +
      '    var isFn = /^F([1-9]|1[0-2])$/.test(ev.key || "");\n' +
      '    if (editable && !hasMod && ev.key !== "Escape" && !isFn) return;\n' +
      '    var mods = (ev.shiftKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.altKey ? 4 : 0) | (ev.metaKey ? 8 : 0) | (ev.repeat ? 16 : 0);\n' +
      '    if (window.__mygoArkWeb && typeof window.__mygoArkWeb.unhandledKey === "function") {\n' +
      '      window.__mygoArkWeb.unhandledKey(mods, ev.key || "", ev.code || "");\n' +
      '    }\n' +
      '  }, false);\n';
    if (scrollToContainer) {
      js +=
        '  window.addEventListener("wheel", function(ev) {\n' +
        '    ev.preventDefault();\n' +
        '    var mult = ev.deltaMode === 1 ? 16 : (ev.deltaMode === 2 ? (window.innerHeight || 600) : 1);\n' +
        '    var mods = (ev.shiftKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.altKey ? 4 : 0) | (ev.metaKey ? 8 : 0);\n' +
        '    if (window.__mygoArkWeb && typeof window.__mygoArkWeb.wheelScroll === "function") {\n' +
        '      window.__mygoArkWeb.wheelScroll(mods, ev.clientX || 0, ev.clientY || 0, (ev.deltaX || 0) * mult, (ev.deltaY || 0) * mult);\n' +
        '    }\n' +
        '  }, { passive: false, capture: true });\n';
    }
  }
  js += '})();';
  return js;
}

function buildHybridScriptLists(
  scripts: HostUserScript[],
  embedded: boolean,
  scrollToContainer: boolean
): [Array<ScriptItem>, Array<ScriptItem>] {
  const startItems: Array<ScriptItem> = [
    {
      script: buildBridgeShimScript(embedded, scrollToContainer),
      scriptRules: ['*']
    }
  ];
  const endItems: Array<ScriptItem> = [];
  for (let i: number = 0; i < scripts.length; i++) {
    const us: HostUserScript = scripts[i];
    if (us.source.length === 0) {
      continue;
    }
    const wrapped: string = us.allFrames
      ? us.source
      : ('if (window === window.top) {\n' + us.source + '\n}');
    const item: ScriptItem = {
      script: wrapped,
      scriptRules: ['*']
    };
    if (us.atDocEnd) {
      endItems.push(item);
    } else {
      startItems.push(item);
    }
  }
  return [startItems, endItems];
}

function extractDispositionFilename(disposition: string): string {
  if (disposition.length === 0) {
    return '';
  }
  const parts: string[] = disposition.split(';');
  let fallback: string = '';
  for (let i: number = 0; i < parts.length; i++) {
    const seg: string = parts[i].trim();
    const lower: string = seg.toLowerCase();
    if (lower.startsWith('filename*=')) {
      let val: string = seg.substring(10).trim();
      const qIdx: number = val.indexOf("''");
      if (qIdx >= 0) {
        val = val.substring(qIdx + 2);
      }
      try {
        const dec: string = decodeURIComponent(val);
        if (dec.length > 0) {
          return dec;
        }
      } catch (_) {
        if (val.length > 0) {
          return val;
        }
      }
    } else if (lower.startsWith('filename=')) {
      let val: string = seg.substring(9).trim();
      if (val.startsWith('"') && val.endsWith('"') && val.length >= 2) {
        val = val.substring(1, val.length - 1);
      }
      if (val.length > 0) {
        fallback = val;
      }
    }
  }
  return fallback;
}

class ArkWebDriver {
  ctrl: webview.WebviewController = new webview.WebviewController();
  webId: number = 0;
  embedded: boolean = false;
  attached: boolean = false;
  hasRealNavigation: boolean = false;
  frameX: number = 0;
  frameY: number = 0;
  scaleX: number = 1;
  scaleY: number = 1;
  private uiCtx: UIContext | undefined = undefined;
  private onSetupChanged: (() => void) | undefined = undefined;
  private jsProxy: ArkWebJsProxy;
  private appliedZoom: number = 1.0;
  private nextPermId: number = 0;
  private pendingPerms: Map<number, PendingWebPerm> = new Map<number, PendingWebPerm>();
  private pendingEvals: Map<number, boolean> = new Map<number, boolean>();
  private localCmdQueue: BridgeWebCmd[] = [];
  private localAsyncQueue: HostWebAsyncReq[] = [];

  constructor() {
    this.jsProxy = new ArkWebJsProxy(this);
  }

  compId(): string {
    return 'mygo_arkweb_' + this.webId.toString();
  }

  bind(uiCtx: UIContext, webId: number, embedded: boolean, onSetupChanged: () => void): void {
    this.uiCtx = uiCtx;
    this.webId = webId;
    this.embedded = embedded;
    this.onSetupChanged = onSetupChanged;
    hostBridge.registerWebInstance(
      webId,
      (op: number, arg1: string, arg2: string): void => {
        this.runCommand(op, arg1, arg2);
      },
      (req: HostWebAsyncReq): void => {
        this.runAsync(req);
      },
      (): void => {
        if (this.onSetupChanged !== undefined) {
          this.onSetupChanged();
        }
      }
    );
  }

  unbind(): void {
    if (this.webId > 0) {
      hostBridge.unregisterWebInstance(this.webId);
    }
    if (this.attached) {
      try {
        this.ctrl.deleteJavaScriptRegister('__mygoArkWeb');
      } catch (_) {}
      try {
        this.ctrl.clearWebSchemeHandler();
      } catch (_) {}
    }
    this.attached = false;
    this.localCmdQueue = [];
    const evalIds: number[] = [];
    this.pendingEvals.forEach((_v: boolean, reqId: number): void => {
      evalIds.push(reqId);
    });
    this.pendingEvals.clear();
    for (let i: number = 0; i < evalIds.length; i++) {
      host.resolveWebAsync(evalIds[i], '', 'ohos: webview closed', new ArrayBuffer(0));
    }
    for (let i: number = 0; i < this.localAsyncQueue.length; i++) {
      host.resolveWebAsync(this.localAsyncQueue[i].reqId, '', 'ohos: webview closed', new ArrayBuffer(0));
    }
    this.localAsyncQueue = [];
    this.pendingPerms.forEach((p: PendingWebPerm): void => {
      try {
        if (p.isGeo && p.geoReq !== undefined) {
          p.geoReq.invoke(p.origin, false, false);
        } else if (p.permReq !== undefined) {
          p.permReq.deny();
        }
      } catch (_) {}
    });
    this.pendingPerms.clear();
  }

  safeCanBack(): boolean {
    if (!this.attached) {
      return false;
    }
    try {
      return this.ctrl.accessBackward();
    } catch (_) {
      return false;
    }
  }

  safeCanForward(): boolean {
    if (!this.attached) {
      return false;
    }
    try {
      return this.ctrl.accessForward();
    } catch (_) {
      return false;
    }
  }

  onAttached(): void {
    this.attached = true;
    try {
      this.ctrl.registerJavaScriptProxy(
        this.jsProxy,
        '__mygoArkWeb',
        ['postMessage', 'asyncReply', 'tabOut', 'unhandledKey', 'wheelScroll']
      );
    } catch (_) {}

    hostBridge.attachSchemeHandlers(this.webId, this.ctrl);

    const st: ManagedWebState = hostBridge.getOrCreateWebState(this.webId);
    const ua: string = hostBridge.getWebUserAgent(this.webId);
    if (ua.length > 0) {
      try {
        this.ctrl.setCustomUserAgent(ua);
      } catch (_) {}
    }
    if (st.muted) {
      try {
        this.ctrl.setAudioMuted(true);
      } catch (_) {}
    }
    if (st.zoomFactor > 0 && Math.abs(st.zoomFactor - this.appliedZoom) > 1e-4) {
      try {
        this.ctrl.zoom(st.zoomFactor / this.appliedZoom);
        this.appliedZoom = st.zoomFactor;
      } catch (_) {}
    }

    const pendingHtml: [string, string] = hostBridge.consumePendingWebHtml(this.webId);
    const initialUrl: string = hostBridge.getWebUrl(this.webId);
    if (pendingHtml[0].length > 0) {
      this.hasRealNavigation = true;
      const base: string | undefined = pendingHtml[1].length > 0 ? pendingHtml[1] : undefined;
      try {
        this.ctrl.loadData(pendingHtml[0], 'text/html', 'UTF-8', base, base);
      } catch (_) {}
    } else if (initialUrl.length > 0) {
      this.hasRealNavigation = true;
      try {
        this.ctrl.loadUrl(initialUrl);
      } catch (_) {}
    }

    const bridgeCmds: BridgeWebCmd[] = hostBridge.consumeQueuedWebCmds(this.webId);
    for (let i: number = 0; i < bridgeCmds.length; i++) {
      this.executeCmd(bridgeCmds[i].op, bridgeCmds[i].arg1, bridgeCmds[i].arg2);
    }
    const localCmds: BridgeWebCmd[] = this.localCmdQueue.slice();
    this.localCmdQueue = [];
    for (let i: number = 0; i < localCmds.length; i++) {
      this.executeCmd(localCmds[i].op, localCmds[i].arg1, localCmds[i].arg2);
    }

    const bridgeAsync: HostWebAsyncReq[] = hostBridge.consumeQueuedWebAsync(this.webId);
    for (let i: number = 0; i < bridgeAsync.length; i++) {
      this.executeAsync(bridgeAsync[i]);
    }
    const localAsync: HostWebAsyncReq[] = this.localAsyncQueue.slice();
    this.localAsyncQueue = [];
    for (let i: number = 0; i < localAsync.length; i++) {
      this.executeAsync(localAsync[i]);
    }
  }

  runCommand(op: number, arg1: string, arg2: string): void {
    if (op === WEB_OP_SET_BG && this.onSetupChanged !== undefined) {
      this.onSetupChanged();
    }
    if (!this.attached) {
      if (op === WEB_OP_LOAD_URL || op === WEB_OP_LOAD_HTML || op === WEB_OP_SET_UA || op === WEB_OP_SET_BG) {
        return;
      }
      const cmd: BridgeWebCmd = new BridgeWebCmd();
      cmd.op = op;
      cmd.arg1 = arg1;
      cmd.arg2 = arg2;
      this.localCmdQueue.push(cmd);
      return;
    }
    this.executeCmd(op, arg1, arg2);
  }

  private executeCmd(op: number, arg1: string, arg2: string): void {
    try {
      switch (op) {
        case WEB_OP_LOAD_URL:
          if (arg1.length > 0) {
            this.hasRealNavigation = true;
            this.ctrl.loadUrl(arg1);
          }
          break;
        case WEB_OP_LOAD_HTML: {
          this.hasRealNavigation = true;
          const base: string | undefined = arg2.length > 0 ? arg2 : undefined;
          this.ctrl.loadData(arg1, 'text/html', 'UTF-8', base, base);
          break;
        }
        case WEB_OP_RELOAD:
          this.ctrl.refresh();
          break;
        case WEB_OP_STOP:
          this.ctrl.stop();
          break;
        case WEB_OP_BACK:
          if (this.ctrl.accessBackward()) {
            this.ctrl.backward();
          }
          break;
        case WEB_OP_FORWARD:
          if (this.ctrl.accessForward()) {
            this.ctrl.forward();
          }
          break;
        case WEB_OP_EVAL:
          if (arg1.length > 0) {
            this.ctrl.runJavaScript(arg1).catch((): void => {});
          }
          break;
        case WEB_OP_SET_UA:
          this.ctrl.setCustomUserAgent(arg1);
          break;
        case WEB_OP_SET_ZOOM: {
          const desired: number = Number.parseFloat(arg1);
          if (!Number.isNaN(desired) && desired > 0) {
            const ratio: number = desired / this.appliedZoom;
            if (Math.abs(ratio - 1.0) > 1e-4) {
              this.ctrl.zoom(ratio);
              this.appliedZoom = desired;
            }
          }
          break;
        }
        case WEB_OP_SET_MUTED:
          this.ctrl.setAudioMuted(arg1 === '1');
          break;
        case WEB_OP_DEVTOOLS:
          webview.WebviewController.setWebDebuggingAccess(arg1 === '1');
          break;
        case WEB_OP_FOCUS:
          if (this.uiCtx !== undefined) {
            try {
              this.uiCtx.getFocusController().requestFocus(this.compId());
            } catch (_) {}
          }
          break;
        case WEB_OP_TAB_INTO: {
          if (this.uiCtx !== undefined) {
            try {
              this.uiCtx.getFocusController().requestFocus(this.compId());
            } catch (_) {}
          }
          const focusLast: string = arg1 === '1' ? 'true' : 'false';
          this.ctrl.runJavaScript(
            'if (window.__mygoFocusEdge) { window.__mygoFocusEdge(' + focusLast + '); }'
          ).catch((): void => {});
          break;
        }
        case WEB_OP_PERM_REPLY: {
          const permId: number = Number.parseInt(arg1, 10);
          const allow: boolean = arg2 === '1';
          const perm: PendingWebPerm | undefined = this.pendingPerms.get(permId);
          if (perm !== undefined) {
            this.pendingPerms.delete(permId);
            if (perm.isGeo) {
              if (perm.geoReq !== undefined) {
                perm.geoReq.invoke(perm.origin, allow, false);
              }
            } else if (perm.permReq !== undefined) {
              if (allow) {
                perm.permReq.grant(perm.rawResources);
              } else {
                perm.permReq.deny();
              }
            }
          }
          break;
        }
        case WEB_OP_CLEAR_CACHE:
          this.ctrl.removeCache(true);
          break;
        default:
          break;
      }
    } catch (_) {}
  }

  runAsync(req: HostWebAsyncReq): void {
    if (!this.attached) {
      this.localAsyncQueue.push(req);
      return;
    }
    this.executeAsync(req);
  }

  settleEval(reqId: number, val: string, errStr: string): void {
    if (!this.pendingEvals.has(reqId)) {
      return;
    }
    this.pendingEvals.delete(reqId);
    host.resolveWebAsync(reqId, val, errStr, new ArrayBuffer(0));
  }

  private executeAsync(req: HostWebAsyncReq): void {
    switch (req.op) {
      case WEB_ASYNC_EVAL: {
        this.pendingEvals.set(req.reqId, true);
        const rid: string = req.reqId.toString();
        const wrapper: string =
          '(function(){\n' +
          '  const __p = (async function(){\n' +
          req.body + '\n' +
          '  })();\n' +
          '  __p.then(function(__v){\n' +
          '    if (window.__mygoArkWeb && typeof window.__mygoArkWeb.asyncReply === "function") {\n' +
          '      window.__mygoArkWeb.asyncReply(' + rid + ', true, __v === undefined ? "" : String(__v));\n' +
          '    }\n' +
          '  }, function(__e){\n' +
          '    var __m = (__e && __e.stack) ? String(__e.stack) : String(__e);\n' +
          '    if (window.__mygoArkWeb && typeof window.__mygoArkWeb.asyncReply === "function") {\n' +
          '      window.__mygoArkWeb.asyncReply(' + rid + ', false, __m);\n' +
          '    }\n' +
          '  });\n' +
          '  return "__MYGO_ASYNC_OK__";\n' +
          '})()';
        try {
          this.ctrl.runJavaScriptExt(wrapper).then((ext: webview.JsMessageExt): void => {
            let okStarted: boolean = false;
            try {
              if (ext.getType() === webview.JsMessageType.STRING) {
                okStarted = ext.getString() === '__MYGO_ASYNC_OK__';
              }
            } catch (_) {}
            if (!okStarted) {
              this.settleEval(req.reqId, '', 'SyntaxError: failed to compile async function');
            }
          }).catch((err: BusinessError): void => {
            const rawMsg: string = err?.message ?? 'failed to evaluate script';
            const finalErr: string = rawMsg.indexOf('SyntaxError') >= 0
              ? rawMsg
              : ('SyntaxError: ' + rawMsg);
            this.settleEval(req.reqId, '', finalErr);
          });
        } catch (e) {
          this.settleEval(req.reqId, '', 'SyntaxError: ' + JSON.stringify(e));
        }
        break;
      }

      case WEB_ASYNC_CAPTURE: {
        try {
          this.ctrl.webPageSnapshot(
            {
              id: 'mygo_cap_' + req.reqId.toString(),
              size: { width: '100%', height: '100%' }
            },
            (err: BusinessError, res: webview.SnapshotResult): void => {
              if (err || res === undefined || res.imagePixelMap === undefined) {
                const msg: string = err?.message ?? 'webPageSnapshot failed';
                host.resolveWebAsync(req.reqId, '', msg, new ArrayBuffer(0));
                return;
              }
              hostBridge.packPixelMapToPng(res.imagePixelMap).then((png: ArrayBuffer): void => {
                host.resolveWebAsync(req.reqId, '', '', png);
              }).catch((packErr: BusinessError): void => {
                host.resolveWebAsync(req.reqId, '', packErr?.message ?? 'png encode failed', new ArrayBuffer(0));
              });
            }
          );
        } catch (e) {
          host.resolveWebAsync(req.reqId, '', 'capture failed: ' + JSON.stringify(e), new ArrayBuffer(0));
        }
        break;
      }

      case WEB_ASYNC_PRINT_PDF: {
        let wIn: number = req.pageWidth > 0 ? req.pageWidth : 8.27;
        let hIn: number = req.pageHeight > 0 ? req.pageHeight : 11.69;
        if (req.landscape && wIn < hIn) {
          const tmp: number = wIn;
          wIn = hIn;
          hIn = tmp;
        }
        const maxVertMargin: number = Math.max(0, hIn * 0.45);
        const maxHorizMargin: number = Math.max(0, wIn * 0.45);
        const pdfCfg: webview.PdfConfiguration = {
          width: wIn,
          height: hIn,
          marginTop: Math.max(0, Math.min(maxVertMargin, req.marginTop)),
          marginBottom: Math.max(0, Math.min(maxVertMargin, req.marginBottom)),
          marginRight: Math.max(0, Math.min(maxHorizMargin, req.marginRight)),
          marginLeft: Math.max(0, Math.min(maxHorizMargin, req.marginLeft)),
          shouldPrintBackground: req.background
        };
        try {
          this.ctrl.createPdf(pdfCfg).then((pdfData: webview.PdfData): void => {
            const bytes: Uint8Array = pdfData.pdfArrayBuffer();
            const out: ArrayBuffer = new ArrayBuffer(bytes.byteLength);
            new Uint8Array(out).set(bytes);
            host.resolveWebAsync(req.reqId, '', '', out);
          }).catch((err: BusinessError): void => {
            host.resolveWebAsync(req.reqId, '', err?.message ?? 'createPdf failed', new ArrayBuffer(0));
          });
        } catch (e) {
          host.resolveWebAsync(req.reqId, '', 'createPdf failed: ' + JSON.stringify(e), new ArrayBuffer(0));
        }
        break;
      }

      default:
        host.resolveWebAsync(req.reqId, '', 'unsupported web async op', new ArrayBuffer(0));
        break;
    }
  }

  onPageBegin(url: string): void {
    if (url === 'about:blank' && !this.hasRealNavigation) {
      return;
    }
    host.notifyWebEvent(this.webId, WEB_EV_PAGE_BEGIN, this.safeCanBack(), this.safeCanForward(), 0, url, '');
  }

  onPageEnd(url: string): void {
    if (url === 'about:blank' && !this.hasRealNavigation) {
      return;
    }
    host.notifyWebEvent(this.webId, WEB_EV_PAGE_END, this.safeCanBack(), this.safeCanForward(), 0, url, '');
  }

  onTitle(rawTitle: string): void {
    const t: string = rawTitle.trim();
    if (t.length === 0 || t === 'about:blank') {
      return;
    }
    host.notifyWebEvent(this.webId, WEB_EV_TITLE, this.safeCanBack(), this.safeCanForward(), 0, t, '');
  }

  onMainError(reqUrl: string, code: number, info: string): void {
    if (reqUrl === 'about:blank' && !this.hasRealNavigation) {
      return;
    }
    host.notifyWebEvent(this.webId, WEB_EV_ERROR, this.safeCanBack(), this.safeCanForward(), code, reqUrl, info);
  }

  onJsMessage(msg: string): void {
    host.notifyWebEvent(this.webId, WEB_EV_MESSAGE, this.safeCanBack(), this.safeCanForward(), 0, msg, '');
  }

  onJsTabOut(shiftHeld: boolean): void {
    host.notifyWebEvent(
      this.webId,
      WEB_EV_TAB_OUT,
      this.safeCanBack(),
      this.safeCanForward(),
      shiftHeld ? 1 : 0,
      '',
      ''
    );
  }

  onJsUnhandledKey(modsBits: number, domKey: string, domCode: string): void {
    host.notifyWebEvent(
      this.webId,
      WEB_EV_UNHANDLED_KEY,
      this.safeCanBack(),
      this.safeCanForward(),
      modsBits,
      domKey,
      domCode
    );
  }

  onJsWheelScroll(modsBits: number, clientX: number, clientY: number, deltaX: number, deltaY: number): void {
    const surfX: number = this.frameX + clientX * this.scaleX;
    const surfY: number = this.frameY + clientY * this.scaleY;
    host.notifyWebEvent(
      this.webId,
      WEB_EV_EMBEDDED_SCROLL,
      this.safeCanBack(),
      this.safeCanForward(),
      modsBits,
      surfX.toFixed(2) + ',' + surfY.toFixed(2),
      deltaX.toFixed(2) + ',' + deltaY.toFixed(2)
    );
  }

  onAudioStateChanged(playing: boolean): void {
    host.notifyWebEvent(
      this.webId,
      WEB_EV_AUDIO_STATE,
      this.safeCanBack(),
      this.safeCanForward(),
      playing ? 1 : 0,
      '',
      ''
    );
  }

  onWindowNew(ev: OnWindowNewEvent): void {
    host.clearWebControllerHandler(ev.handler);
    const disp: string = ev.isAlert ? 'new-window' : 'foreground-tab';
    host.notifyWebEvent(
      this.webId,
      WEB_EV_WINDOW_NEW,
      this.safeCanBack(),
      this.safeCanForward(),
      ev.isUserTrigger ? 1 : 0,
      ev.targetUrl ?? '',
      disp
    );
  }

  onWindowExit(): void {
    host.notifyWebEvent(this.webId, WEB_EV_WINDOW_CLOSE, this.safeCanBack(), this.safeCanForward(), 0, '', '');
  }

  onRenderExited(reason: RenderExitReason): void {
    let label: string = 'crashed';
    if (reason === RenderExitReason.ProcessAbnormalTermination) {
      label = 'abnormal-termination';
    } else if (reason === RenderExitReason.ProcessWasKilled) {
      label = 'killed';
    } else if (reason === RenderExitReason.ProcessOom) {
      label = 'oom';
    }
    host.notifyWebEvent(this.webId, WEB_EV_RENDER_EXITED, false, false, 0, label, '');
  }

  onPermissionRequest(request: PermissionRequest): void {
    let origin: string = '';
    let rawRes: string[] = [];
    try {
      origin = request.getOrigin();
      rawRes = request.getAccessibleResource();
    } catch (_) {}
    const kinds: string[] = [];
    for (let i: number = 0; i < rawRes.length; i++) {
      const r: string = rawRes[i];
      if (r === ProtectedResourceType.VIDEO_CAPTURE) {
        if (kinds.indexOf('camera') < 0) {
          kinds.push('camera');
        }
      } else if (r === ProtectedResourceType.AUDIO_CAPTURE) {
        if (kinds.indexOf('microphone') < 0) {
          kinds.push('microphone');
        }
      } else if (r === ProtectedResourceType.MidiSysex) {
        if (kinds.indexOf('midi') < 0) {
          kinds.push('midi');
        }
      }
    }
    if (kinds.length === 0) {
      try {
        request.deny();
      } catch (_) {}
      return;
    }
    this.nextPermId = (this.nextPermId + 1) & 0x7fffffff;
    if (this.nextPermId === 0) {
      this.nextPermId = 1;
    }
    const permId: number = this.nextPermId;
    const p: PendingWebPerm = new PendingWebPerm();
    p.isGeo = false;
    p.origin = origin;
    p.rawResources = rawRes;
    p.permReq = request;
    this.pendingPerms.set(permId, p);
    host.notifyWebEvent(
      this.webId,
      WEB_EV_PERM_REQ,
      this.safeCanBack(),
      this.safeCanForward(),
      permId,
      origin,
      kinds.join(',')
    );
  }

  onGeolocationShow(origin: string, geo: JsGeolocation): void {
    this.nextPermId = (this.nextPermId + 1) & 0x7fffffff;
    if (this.nextPermId === 0) {
      this.nextPermId = 1;
    }
    const permId: number = this.nextPermId;
    const p: PendingWebPerm = new PendingWebPerm();
    p.isGeo = true;
    p.origin = origin;
    p.geoReq = geo;
    this.pendingPerms.set(permId, p);
    host.notifyWebEvent(
      this.webId,
      WEB_EV_PERM_REQ,
      this.safeCanBack(),
      this.safeCanForward(),
      permId,
      origin,
      'geolocation'
    );
  }

  onDownloadStart(url: string, contentDisposition: string): void {
    if (url.length === 0) {
      return;
    }
    const suggested: string = extractDispositionFilename(contentDisposition);
    host.notifyWebEvent(
      this.webId,
      WEB_EV_DOWNLOAD_START,
      this.safeCanBack(),
      this.safeCanForward(),
      0,
      url,
      suggested
    );
  }
}

@Observed
class EmbeddedWebSlotModel {
  slotKey: string = '';
  webId: number = 0;
  incognito: boolean = false;
  autoplay: boolean = false;
  pinchZoom: boolean = false;
  scrollToContainer: boolean = false;
  slotBgHex: string = '#FFFFFFFF';
  placedVisible: boolean = false;
  paintZ: number = 1;
  frameX: number = 0;
  frameY: number = 0;
  frameW: number = 1;
  frameH: number = 1;
  clipX: number = 0;
  clipY: number = 0;
  clipW: number = 0;
  clipH: number = 0;
  pageW: number = 1;
  pageH: number = 1;
  r0: number = 0;
  r1: number = 0;
  r2: number = 0;
  r3: number = 0;
  docStartScripts: Array<ScriptItem> = [];
  docEndScripts: Array<ScriptItem> = [];
}

@Component
struct EmbeddedWebSlot {
  @ObjectLink slotModel: EmbeddedWebSlotModel;
  private driver: ArkWebDriver = new ArkWebDriver();

  aboutToAppear(): void {
    this.syncSetupIntoModel();
    this.driver.bind(this.getUIContext(), this.slotModel.webId, true, (): void => {
      this.syncSetupIntoModel();
    });
  }

  aboutToDisappear(): void {
    this.driver.unbind();
  }

  private syncSetupIntoModel(): void {
    const st: ManagedWebState = hostBridge.getOrCreateWebState(this.slotModel.webId);
    this.slotModel.incognito = (st.flags & WEB_SETUP_INCOGNITO) !== 0;
    this.slotModel.autoplay = (st.flags & WEB_SETUP_AUTOPLAY) !== 0;
    this.slotModel.pinchZoom = (st.flags & WEB_SETUP_PINCH_ZOOM) !== 0;
    this.slotModel.scrollToContainer = (st.flags & WEB_SETUP_SCROLL_TO_CONTAINER) !== 0;
    this.slotModel.slotBgHex = st.bgHex;
    const pair: [Array<ScriptItem>, Array<ScriptItem>] = buildHybridScriptLists(
      st.scripts,
      true,
      this.slotModel.scrollToContainer
    );
    this.slotModel.docStartScripts = pair[0];
    this.slotModel.docEndScripts = pair[1];
  }

  private updateDriverGeometry(): number {
    this.driver.frameX = this.slotModel.frameX;
    this.driver.frameY = this.slotModel.frameY;
    this.driver.scaleX = (this.slotModel.pageW > 0 && this.slotModel.frameW > 0)
      ? (this.slotModel.frameW / this.slotModel.pageW)
      : 1;
    this.driver.scaleY = (this.slotModel.pageH > 0 && this.slotModel.frameH > 0)
      ? (this.slotModel.frameH / this.slotModel.pageH)
      : 1;
    return this.slotModel.paintZ;
  }

  private emitEmbeddedPress(surfX: number, surfY: number, btn: number): void {
    host.notifyWebEvent(
      this.slotModel.webId,
      WEB_EV_EMBEDDED_PRESS,
      this.driver.safeCanBack(),
      this.driver.safeCanForward(),
      btn,
      surfX.toFixed(2) + ',' + surfY.toFixed(2),
      ''
    );
  }

  build() {
    Stack({ alignContent: Alignment.TopStart }) {
      Web({
        src: '',
        controller: this.driver.ctrl,
        incognitoMode: this.slotModel.incognito
      })
        .id(this.driver.compId())
        .position({
          x: this.slotModel.frameX - this.slotModel.clipX,
          y: this.slotModel.frameY - this.slotModel.clipY
        })
        .width(Math.max(1, this.slotModel.pageW))
        .height(Math.max(1, this.slotModel.pageH))
        .scale({
          x: (this.slotModel.pageW > 0 && this.slotModel.frameW > 0) ? (this.slotModel.frameW / this.slotModel.pageW) : 1,
          y: (this.slotModel.pageH > 0 && this.slotModel.frameH > 0) ? (this.slotModel.frameH / this.slotModel.pageH) : 1,
          centerX: 0,
          centerY: 0
        })
        .borderRadius({
          topLeft: this.slotModel.r0,
          topRight: this.slotModel.r1,
          bottomRight: this.slotModel.r2,
          bottomLeft: this.slotModel.r3
        })
        .clip(true)
        .backgroundColor(this.slotModel.slotBgHex)
        .javaScriptAccess(true)
        .domStorageAccess(true)
        .databaseAccess(true)
        .imageAccess(true)
        .onlineImageAccess(true)
        .mixedMode(MixedMode.All)
        .darkMode(WebDarkMode.Auto)
        .multiWindowAccess(true)
        .allowWindowOpenMethod(true)
        .mediaPlayGestureAccess(!this.slotModel.autoplay)
        .zoomAccess(this.slotModel.pinchZoom)
        .runJavaScriptOnDocumentStart(this.slotModel.docStartScripts)
        .runJavaScriptOnDocumentEnd(this.slotModel.docEndScripts)
        .onControllerAttached((): void => {
          this.driver.onAttached();
        })
        .onPageBegin((ev: OnPageBeginEvent): void => {
          this.driver.onPageBegin(ev.url ?? '');
        })
        .onPageEnd((ev: OnPageEndEvent): void => {
          this.driver.onPageEnd(ev.url ?? '');
        })
        .onTitleReceive((ev: OnTitleReceiveEvent): void => {
          this.driver.onTitle(ev.title ?? '');
        })
        .onErrorReceive((ev: OnErrorReceiveEvent): void => {
          let mainFrame: boolean = false;
          let reqUrl: string = '';
          let code: number = -1;
          let info: string = '';
          try {
            mainFrame = ev.request.isMainFrame();
            reqUrl = ev.request.getRequestUrl();
          } catch (_) {}
          if (!mainFrame) {
            return;
          }
          try {
            code = ev.error.getErrorCode();
            info = ev.error.getErrorInfo();
          } catch (_) {}
          this.driver.onMainError(reqUrl, code, info);
        })
        .onAudioStateChanged((ev: OnAudioStateChangedEvent): void => {
          this.driver.onAudioStateChanged(ev.playing);
        })
        .onWindowNew((ev: OnWindowNewEvent): void => {
          this.driver.onWindowNew(ev);
        })
        .onWindowExit((): void => {
          this.driver.onWindowExit();
        })
        .onRenderExited((ev: OnRenderExitedEvent): void => {
          this.driver.onRenderExited(ev.renderExitReason);
        })
        .onPermissionRequest((ev: OnPermissionRequestEvent): void => {
          this.driver.onPermissionRequest(ev.request);
        })
        .onGeolocationShow((ev: OnGeolocationShowEvent): void => {
          this.driver.onGeolocationShow(ev.origin, ev.geolocation);
        })
        .onDownloadStart((ev: OnDownloadStartEvent): void => {
          this.driver.onDownloadStart(ev.url, ev.contentDisposition);
        })
    }
    .position({
      x: this.slotModel.clipX,
      y: this.slotModel.clipY
    })
    .width(Math.max(1, this.slotModel.clipW))
    .height(Math.max(1, this.slotModel.clipH))
    .clip(true)
    .zIndex(this.updateDriverGeometry())
    .visibility(
      (this.slotModel.placedVisible && this.slotModel.clipW > 0 && this.slotModel.clipH > 0)
        ? Visibility.Visible
        : Visibility.Hidden
    )
    .onMouse((ev: MouseEvent): void => {
      if (ev.action === MouseAction.Press) {
        let btn: number = 0;
        if (ev.button === MouseButton.Right) {
          btn = 1;
        } else if (ev.button === MouseButton.Middle) {
          btn = 2;
        }
        this.emitEmbeddedPress(this.slotModel.clipX + ev.x, this.slotModel.clipY + ev.y, btn);
      }
    })
    .onTouch((ev: TouchEvent): void => {
      if (ev.type === TouchType.Down && ev.touches.length > 0) {
        this.emitEmbeddedPress(this.slotModel.clipX + ev.touches[0].x, this.slotModel.clipY + ev.touches[0].y, 0);
      }
    })
  }
}

class MenuEntryModel {
  key: string = '';
  itemId: number = 0;
  label: string = '';
  labelInfo: string = '';
  enabled: boolean = true;
  checkable: boolean = false;
  isRadio: boolean = false;
  checked: boolean = false;
  hasSubmenu: boolean = false;
  subLevel: MenuLevelModel | undefined = undefined;
}

class MenuSectionModel {
  key: string = '';
  entries: MenuEntryModel[] = [];
}

class MenuLevelModel {
  sections: MenuSectionModel[] = [];
  hasCheckable: boolean = false;
  widthVp: number = 148;
}

class PopupMenuContext {
  root: MenuLevelModel = new MenuLevelModel();
  darkMode: boolean = false;
  direction: Direction = Direction.Auto;
  onSelect: (chosenId: number) => void = (_chosenId: number): void => {};
}

class MessageBoxButtonModel {
  key: string = '';
  index: number = 0;
  label: string = '';
  isDefault: boolean = false;
  isDestructive: boolean = false;
}

class MessageBoxContext {
  title: string = '';
  message: string = '';
  detail: string = '';
  checkboxLabel: string = '';
  checkboxChecked: boolean = false;
  darkMode: boolean = false;
  buttons: MessageBoxButtonModel[] = [];
  onToggleCheckbox: (checked: boolean) => void = (_checked: boolean): void => {};
  onChoose: (index: number) => void = (_index: number): void => {};
}

function measureMenuLevelWidthVp(uiCtx: UIContext, sections: MenuSectionModel[], hasCheckable: boolean): number {
  let maxRowVp: number = 128;
  for (let s: number = 0; s < sections.length; s++) {
    const entries: MenuEntryModel[] = sections[s].entries;
    for (let i: number = 0; i < entries.length; i++) {
      const ent: MenuEntryModel = entries[i];
      let labelVp: number = ent.label.length * 13;
      let infoVp: number = ent.labelInfo.length * 8;
      try {
        const labelPx: number = uiCtx.getMeasureUtils().measureText({
          textContent: ent.label,
          fontSize: 13,
          fontWeight: FontWeight.Normal
        });
        labelVp = uiCtx.px2vp(labelPx);
        if (ent.labelInfo.length > 0) {
          const infoPx: number = uiCtx.getMeasureUtils().measureText({
            textContent: ent.labelInfo,
            fontSize: 12,
            fontWeight: FontWeight.Normal
          });
          infoVp = uiCtx.px2vp(infoPx);
        }
      } catch (_) {}
      let rowVp: number = 28 + labelVp;
      if (hasCheckable) {
        rowVp += 18;
      }
      if (ent.labelInfo.length > 0) {
        rowVp += 18 + infoVp;
      }
      if (ent.hasSubmenu) {
        rowVp += 20;
      }
      if (rowVp > maxRowVp) {
        maxRowVp = rowVp;
      }
    }
  }
  return Math.min(300, Math.max(128, Math.ceil(maxRowVp + 8)));
}

function buildMenuLevel(uiCtx: UIContext, items: HostMenuItem[], prefix: string): MenuLevelModel {
  const sections: MenuSectionModel[] = [];
  let current: MenuEntryModel[] = [];
  let secIdx: number = 0;
  let hasCheckable: boolean = false;
  for (let i: number = 0; i < items.length; i++) {
    const raw: HostMenuItem = items[i];
    if (raw.kind === MENU_KIND_SEPARATOR) {
      if (current.length > 0) {
        const sec: MenuSectionModel = new MenuSectionModel();
        sec.key = prefix + '_sec_' + secIdx.toString();
        sec.entries = current;
        sections.push(sec);
        secIdx++;
        current = [];
      }
      continue;
    }
    const ent: MenuEntryModel = new MenuEntryModel();
    ent.key = prefix + '_it_' + i.toString() + '_' + raw.id.toString();
    ent.itemId = raw.id;
    ent.label = raw.label;
    ent.labelInfo = raw.accel;
    ent.enabled = raw.enabled;
    ent.checkable = raw.kind === MENU_KIND_CHECKBOX || raw.kind === MENU_KIND_RADIO;
    ent.isRadio = raw.kind === MENU_KIND_RADIO;
    ent.checked = raw.checked && ent.checkable;
    if (ent.checkable) {
      hasCheckable = true;
    }
    if (raw.kind === MENU_KIND_SUBMENU && raw.items.length > 0) {
      const sub: MenuLevelModel = buildMenuLevel(uiCtx, raw.items, ent.key);
      if (sub.sections.length > 0) {
        ent.subLevel = sub;
        ent.hasSubmenu = true;
      }
    }
    current.push(ent);
  }
  if (current.length > 0) {
    const lastSec: MenuSectionModel = new MenuSectionModel();
    lastSec.key = prefix + '_sec_' + secIdx.toString();
    lastSec.entries = current;
    sections.push(lastSec);
  }
  const level: MenuLevelModel = new MenuLevelModel();
  level.sections = sections;
  level.hasCheckable = hasCheckable;
  level.widthVp = measureMenuLevelWidthVp(uiCtx, sections, hasCheckable);
  return level;
}

function disposeDeferred(fn: () => void): void {
  setTimeout((): void => {
    try {
      fn();
    } catch (_) {}
  }, 0);
}

@Component
struct MygoPopupMenuLevel3 {
  menuLevel: MenuLevelModel = new MenuLevelModel();
  isDark: boolean = false;
  menuDir: Direction = Direction.Auto;
  onMenuSelect: (chosenId: number) => void = (_chosenId: number): void => {};

  @Builder
  buildItemRow(ent: MenuEntryModel): void {
    Row() {
      if (this.menuLevel.hasCheckable) {
        Row() {
          Text(ent.checked ? (ent.isRadio ? '•' : '✓') : '')
            .fontSize(ent.isRadio ? 15 : 13)
            .fontWeight(FontWeight.Medium)
            .fontColor(
              ent.enabled
                ? (this.isDark ? '#F1F3F5' : '#182431')
                : (this.isDark ? '#66FFFFFF' : '#66182431')
            )
        }
        .width(18)
        .height('100%')
        .alignItems(VerticalAlign.Center)
        .justifyContent(FlexAlign.Start)
      }
      Text(ent.label)
        .fontSize(13)
        .fontWeight(FontWeight.Normal)
        .fontColor(
          ent.enabled
            ? (this.isDark ? '#F1F3F5' : '#182431')
            : (this.isDark ? '#66FFFFFF' : '#66182431')
        )
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .layoutWeight(1)
      if (ent.labelInfo.length > 0) {
        Text(ent.labelInfo)
          .fontSize(12)
          .fontWeight(FontWeight.Normal)
          .fontColor(this.isDark ? '#8CFFFFFF' : '#8C182431')
          .margin({ left: 16 })
      }
    }
    .width('100%')
    .height(30)
    .padding({ left: 10, right: 10 })
    .alignItems(VerticalAlign.Center)
  }

  build() {
    Menu() {
      ForEach(this.menuLevel.sections, (sec3: MenuSectionModel) => {
        MenuItemGroup() {
          ForEach(sec3.entries, (ent3: MenuEntryModel) => {
            MenuItem((): void => {
              this.buildItemRow(ent3);
            })
            .enabled(ent3.enabled)
            .onClick((): void => {
              if (ent3.enabled && ent3.itemId !== 0) {
                this.onMenuSelect(ent3.itemId);
              }
            })
          }, (ent3: MenuEntryModel): string => ent3.key)
        }
      }, (sec3: MenuSectionModel): string => sec3.key)
    }
    .width(this.menuLevel.widthVp)
    .radius(8)
    .padding(4)
    .menuItemGroupDivider({
      strokeWidth: LengthMetrics.px(1),
      color: this.isDark ? '#24FFFFFF' : '#1A000000',
      startMargin: LengthMetrics.vp(8),
      endMargin: LengthMetrics.vp(8),
      mode: DividerMode.EMBEDDED_IN_MENU
    })
    .direction(this.menuDir)
  }
}

@Component
struct MygoPopupMenuLevel2 {
  menuLevel: MenuLevelModel = new MenuLevelModel();
  isDark: boolean = false;
  menuDir: Direction = Direction.Auto;
  onMenuSelect: (chosenId: number) => void = (_chosenId: number): void => {};

  @Builder
  buildItemRow(ent: MenuEntryModel): void {
    Row() {
      if (this.menuLevel.hasCheckable) {
        Row() {
          Text(ent.checked ? (ent.isRadio ? '•' : '✓') : '')
            .fontSize(ent.isRadio ? 15 : 13)
            .fontWeight(FontWeight.Medium)
            .fontColor(
              ent.enabled
                ? (this.isDark ? '#F1F3F5' : '#182431')
                : (this.isDark ? '#66FFFFFF' : '#66182431')
            )
        }
        .width(18)
        .height('100%')
        .alignItems(VerticalAlign.Center)
        .justifyContent(FlexAlign.Start)
      }
      Text(ent.label)
        .fontSize(13)
        .fontWeight(FontWeight.Normal)
        .fontColor(
          ent.enabled
            ? (this.isDark ? '#F1F3F5' : '#182431')
            : (this.isDark ? '#66FFFFFF' : '#66182431')
        )
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .layoutWeight(1)
      if (ent.labelInfo.length > 0) {
        Text(ent.labelInfo)
          .fontSize(12)
          .fontWeight(FontWeight.Normal)
          .fontColor(this.isDark ? '#8CFFFFFF' : '#8C182431')
          .margin({ left: 16 })
      }
      if (ent.hasSubmenu) {
        Text('›')
          .fontSize(16)
          .fontWeight(FontWeight.Normal)
          .fontColor(
            ent.enabled
              ? (this.isDark ? '#A6FFFFFF' : '#99182431')
              : (this.isDark ? '#4DFFFFFF' : '#4D182431')
          )
          .margin({ left: 12 })
      }
    }
    .width('100%')
    .height(30)
    .padding({ left: 10, right: 10 })
    .alignItems(VerticalAlign.Center)
  }

  @Builder
  buildSub3(sub: MenuLevelModel): void {
    MygoPopupMenuLevel3({
      menuLevel: sub,
      isDark: this.isDark,
      menuDir: this.menuDir,
      onMenuSelect: this.onMenuSelect
    })
  }

  build() {
    Menu() {
      ForEach(this.menuLevel.sections, (sec2: MenuSectionModel) => {
        MenuItemGroup() {
          ForEach(sec2.entries, (ent2: MenuEntryModel) => {
            if (ent2.hasSubmenu && ent2.subLevel !== undefined) {
              MenuItem((): void => {
                this.buildItemRow(ent2);
              })
              .enabled(ent2.enabled)
              .subMenuBuilder((): void => {
                if (ent2.subLevel !== undefined) {
                  this.buildSub3(ent2.subLevel);
                }
              })
            } else {
              MenuItem((): void => {
                this.buildItemRow(ent2);
              })
              .enabled(ent2.enabled)
              .onClick((): void => {
                if (ent2.enabled && ent2.itemId !== 0) {
                  this.onMenuSelect(ent2.itemId);
                }
              })
            }
          }, (ent2: MenuEntryModel): string => ent2.key)
        }
      }, (sec2: MenuSectionModel): string => sec2.key)
    }
    .width(this.menuLevel.widthVp)
    .radius(8)
    .padding(4)
    .menuItemGroupDivider({
      strokeWidth: LengthMetrics.px(1),
      color: this.isDark ? '#24FFFFFF' : '#1A000000',
      startMargin: LengthMetrics.vp(8),
      endMargin: LengthMetrics.vp(8),
      mode: DividerMode.EMBEDDED_IN_MENU
    })
    .direction(this.menuDir)
  }
}

@Component
struct MygoPopupMenuLevel1 {
  menuLevel: MenuLevelModel = new MenuLevelModel();
  isDark: boolean = false;
  menuDir: Direction = Direction.Auto;
  onMenuSelect: (chosenId: number) => void = (_chosenId: number): void => {};

  @Builder
  buildItemRow(ent: MenuEntryModel): void {
    Row() {
      if (this.menuLevel.hasCheckable) {
        Row() {
          Text(ent.checked ? (ent.isRadio ? '•' : '✓') : '')
            .fontSize(ent.isRadio ? 15 : 13)
            .fontWeight(FontWeight.Medium)
            .fontColor(
              ent.enabled
                ? (this.isDark ? '#F1F3F5' : '#182431')
                : (this.isDark ? '#66FFFFFF' : '#66182431')
            )
        }
        .width(18)
        .height('100%')
        .alignItems(VerticalAlign.Center)
        .justifyContent(FlexAlign.Start)
      }
      Text(ent.label)
        .fontSize(13)
        .fontWeight(FontWeight.Normal)
        .fontColor(
          ent.enabled
            ? (this.isDark ? '#F1F3F5' : '#182431')
            : (this.isDark ? '#66FFFFFF' : '#66182431')
        )
        .maxLines(1)
        .textOverflow({ overflow: TextOverflow.Ellipsis })
        .layoutWeight(1)
      if (ent.labelInfo.length > 0) {
        Text(ent.labelInfo)
          .fontSize(12)
          .fontWeight(FontWeight.Normal)
          .fontColor(this.isDark ? '#8CFFFFFF' : '#8C182431')
          .margin({ left: 16 })
      }
      if (ent.hasSubmenu) {
        Text('›')
          .fontSize(16)
          .fontWeight(FontWeight.Normal)
          .fontColor(
            ent.enabled
              ? (this.isDark ? '#A6FFFFFF' : '#99182431')
              : (this.isDark ? '#4DFFFFFF' : '#4D182431')
          )
          .margin({ left: 12 })
      }
    }
    .width('100%')
    .height(30)
    .padding({ left: 10, right: 10 })
    .alignItems(VerticalAlign.Center)
  }

  @Builder
  buildSub2(sub: MenuLevelModel): void {
    MygoPopupMenuLevel2({
      menuLevel: sub,
      isDark: this.isDark,
      menuDir: this.menuDir,
      onMenuSelect: this.onMenuSelect
    })
  }

  build() {
    Menu() {
      ForEach(this.menuLevel.sections, (sec1: MenuSectionModel) => {
        MenuItemGroup() {
          ForEach(sec1.entries, (ent1: MenuEntryModel) => {
            if (ent1.hasSubmenu && ent1.subLevel !== undefined) {
              MenuItem((): void => {
                this.buildItemRow(ent1);
              })
              .enabled(ent1.enabled)
              .subMenuBuilder((): void => {
                if (ent1.subLevel !== undefined) {
                  this.buildSub2(ent1.subLevel);
                }
              })
            } else {
              MenuItem((): void => {
                this.buildItemRow(ent1);
              })
              .enabled(ent1.enabled)
              .onClick((): void => {
                if (ent1.enabled && ent1.itemId !== 0) {
                  this.onMenuSelect(ent1.itemId);
                }
              })
            }
          }, (ent1: MenuEntryModel): string => ent1.key)
        }
      }, (sec1: MenuSectionModel): string => sec1.key)
    }
    .width(this.menuLevel.widthVp)
    .radius(8)
    .padding(4)
    .menuItemGroupDivider({
      strokeWidth: LengthMetrics.px(1),
      color: this.isDark ? '#24FFFFFF' : '#1A000000',
      startMargin: LengthMetrics.vp(8),
      endMargin: LengthMetrics.vp(8),
      mode: DividerMode.EMBEDDED_IN_MENU
    })
    .direction(this.menuDir)
  }
}

@Builder
function buildPopupMenuRoot(ctx: PopupMenuContext): void {
  WithTheme({ colorMode: ctx.darkMode ? ThemeColorMode.DARK : ThemeColorMode.LIGHT }) {
    MygoPopupMenuLevel1({
      menuLevel: ctx.root,
      isDark: ctx.darkMode,
      menuDir: ctx.direction,
      onMenuSelect: ctx.onSelect
    })
  }
}

@Builder
function buildMessageBoxDialog(ctx: MessageBoxContext): void {
  Column() {
    if (ctx.title.length > 0) {
      Text(ctx.title)
        .fontSize(18)
        .fontWeight(FontWeight.Bold)
        .fontColor(ctx.darkMode ? '#FFFFFF' : '#182431')
        .width('100%')
        .margin({ bottom: 8 })
    }
    if (ctx.message.length > 0) {
      Text(ctx.message)
        .fontSize(15)
        .fontWeight(ctx.title.length > 0 ? FontWeight.Medium : FontWeight.Bold)
        .fontColor(ctx.darkMode ? '#F1F3F5' : '#182431')
        .width('100%')
        .margin({ bottom: ctx.detail.length > 0 ? 8 : 16 })
    }
    if (ctx.detail.length > 0) {
      Text(ctx.detail)
        .fontSize(13)
        .fontColor(ctx.darkMode ? '#A6ADB4' : '#666E77')
        .width('100%')
        .margin({ bottom: 16 })
    }
    if (ctx.checkboxLabel.length > 0) {
      Row() {
        Checkbox({ name: 'msgbox_chk' })
          .select(ctx.checkboxChecked)
          .onChange((val: boolean): void => {
            ctx.onToggleCheckbox(val);
          })
          .margin({ right: 8 })
        Text(ctx.checkboxLabel)
          .fontSize(14)
          .fontColor(ctx.darkMode ? '#E5E7EB' : '#182431')
          .layoutWeight(1)
      }
      .width('100%')
      .alignItems(VerticalAlign.Center)
      .margin({ bottom: 18 })
    }
    Flex({ wrap: FlexWrap.Wrap, justifyContent: FlexAlign.End }) {
      ForEach(ctx.buttons, (btn: MessageBoxButtonModel) => {
        Button(btn.label)
          .type(ButtonType.Capsule)
          .fontSize(14)
          .height(36)
          .padding({ left: 18, right: 18 })
          .margin({ left: 8, top: 4 })
          .backgroundColor(
            btn.isDestructive
              ? '#E84026'
              : (btn.isDefault ? '#0A59F7' : (ctx.darkMode ? '#2E3238' : '#E6E9ED'))
          )
          .fontColor(
            (btn.isDestructive || btn.isDefault)
              ? '#FFFFFF'
              : (ctx.darkMode ? '#F1F3F5' : '#182431')
          )
          .defaultFocus(btn.isDefault)
          .onClick((): void => {
            ctx.onChoose(btn.index);
          })
      }, (btn: MessageBoxButtonModel): string => btn.key)
    }
    .width('100%')
  }
  .width(400)
  .padding(24)
  .borderRadius(20)
  .backgroundColor(ctx.darkMode ? '#1F2227' : '#FFFFFF')
}

@Entry
@Component
struct Index {
  private content: NodeContent = new NodeContent();
  private winId: number = 1;
  private surfaceBound: boolean = false;
  private webDriverBound: boolean = false;
  private webIncognito: boolean = false;
  private bgARGB: number = 0xFFF4F4F6;
  private mainWebDriver: ArkWebDriver = new ArkWebDriver();
  private slotMap: Map<number, EmbeddedWebSlotModel> = new Map<number, EmbeddedWebSlotModel>();
  private activeMenuNode: ComponentContent<PopupMenuContext> | undefined = undefined;
  private activeMenuReqId: number = 0;
  private activeMsgNode: ComponentContent<MessageBoxContext> | undefined = undefined;
  private activeMsgReqId: number = 0;
  private activeMsgCancelId: number = 0;
  private activeMsgChecked: boolean = false;

  @State isWebWindow: boolean = false;
  @State winBgHex: string = '#FFF4F4F6';
  @State mainAutoplay: boolean = false;
  @State mainPinchZoom: boolean = true;
  @State mainDocStartScripts: Array<ScriptItem> = [];
  @State mainDocEndScripts: Array<ScriptItem> = [];
  @State embeddedSlots: Array<EmbeddedWebSlotModel> = [];
  @State surfaceHitRects: Array<Rectangle> = [{ x: 0, y: 0, width: '100%', height: '100%' }];

  aboutToAppear(): void {
    const uiCtx: UIContext = this.getUIContext();
    const sharedStorage: LocalStorage | undefined = uiCtx.getSharedLocalStorage();
    this.winId = sharedStorage?.get<number>('winId') ?? 1;
    const arkWinId: number = sharedStorage?.get<number>('arkWindowId') ?? hostBridge.getArkWindowId(this.winId);
    const darkMode: boolean = AppStorage.get<boolean>('darkMode') ?? false;

    const initialIsWeb: boolean = (sharedStorage?.get<boolean>('isWebWindow') ?? false) ||
      hostBridge.isManagedWebWindow(this.winId);
    this.isWebWindow = initialIsWeb;
    this.webIncognito = (sharedStorage?.get<boolean>('webIncognito') ?? false) ||
      hostBridge.isManagedWebIncognito(this.winId);

    const defaultHex: string = darkMode ? '#FF1C1C1F' : (initialIsWeb ? '#FFFFFFFF' : '#FFF4F4F6');
    const defaultARGB: number = darkMode ? 0xFF1C1C1F : (initialIsWeb ? 0xFFFFFFFF : 0xFFF4F4F6);
    this.winBgHex = sharedStorage?.get<string>('bgHex') ?? defaultHex;
    this.bgARGB = sharedStorage?.get<number>('bgARGB') ?? defaultARGB;

    hostBridge.registerPopupMenuOpener(
      this.winId,
      (reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void => {
        this.openNativePopupMenu(reqId, x, y, rtl, items);
      }
    );
    hostBridge.registerMessageBoxOpener(
      this.winId,
      (box: HostMessageBox): void => {
        this.openNativeMessageBox(box);
      }
    );
    hostBridge.registerEmbeddedSyncer(this.winId, (): void => {
      this.onEmbeddedOrModeSync();
    });

    if (this.isWebWindow) {
      this.bindStandaloneWebWindow();
      return;
    }

    let densityPixels: number = 1.5;
    let refreshRate: number = 120;
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      if (d.densityPixels > 0) {
        densityPixels = d.densityPixels;
      }
      if (d.refreshRate > 0) {
        refreshRate = d.refreshRate;
      }
    } catch (_) {}

    host.bindWindow(this.winId, uiCtx, this.content, arkWinId, densityPixels, refreshRate, darkMode, this.bgARGB);
    this.surfaceBound = true;
    this.syncEmbeddedWebViews();
  }

  aboutToDisappear(): void {
    this.dismissActiveMenu(0);
    this.dismissActiveMessageBox();
    hostBridge.unregisterPopupMenuOpener(this.winId);
    hostBridge.unregisterMessageBoxOpener(this.winId);
    hostBridge.unregisterEmbeddedSyncer(this.winId);
    if (this.webDriverBound) {
      this.mainWebDriver.unbind();
      this.webDriverBound = false;
    }
    this.slotMap.clear();
    this.embeddedSlots = [];
    if (this.surfaceBound) {
      host.unbindWindow(this.winId);
      this.surfaceBound = false;
    }
  }

  private bindStandaloneWebWindow(): void {
    if (this.webDriverBound) {
      return;
    }
    this.webDriverBound = true;
    this.webIncognito = hostBridge.isManagedWebIncognito(this.winId) || this.webIncognito;
    this.syncStandaloneWebSetup();
    this.mainWebDriver.bind(this.getUIContext(), this.winId, false, (): void => {
      this.syncStandaloneWebSetup();
    });
  }

  private syncStandaloneWebSetup(): void {
    const st: ManagedWebState = hostBridge.getOrCreateWebState(this.winId);
    this.mainAutoplay = (st.flags & WEB_SETUP_AUTOPLAY) !== 0;
    this.mainPinchZoom = (st.flags & WEB_SETUP_PINCH_ZOOM) !== 0 || st.flags === 0;
    this.winBgHex = st.bgHex;
    const pair: [Array<ScriptItem>, Array<ScriptItem>] = buildHybridScriptLists(st.scripts, false, false);
    this.mainDocStartScripts = pair[0];
    this.mainDocEndScripts = pair[1];
  }

  private onEmbeddedOrModeSync(): void {
    if (!this.isWebWindow && hostBridge.isManagedWebWindow(this.winId)) {
      if (this.surfaceBound) {
        host.unbindWindow(this.winId);
        this.surfaceBound = false;
      }
      this.isWebWindow = true;
      this.bindStandaloneWebWindow();
      return;
    }
    if (!this.isWebWindow) {
      this.syncEmbeddedWebViews();
    }
  }

  private syncEmbeddedWebViews(): void {
    const webIds: number[] = hostBridge.getEmbeddedWebIds(this.winId);
    const placements: HostPlacedWebView[] = hostBridge.getEmbeddedPlacements(this.winId);
    const regions: HostSurfaceRegion[] = hostBridge.getEmbeddedSurfaceRegions(this.winId);

    if (webIds.length > 0 && this.winBgHex !== '#00000000') {
      this.winBgHex = '#00000000';
    }

    const placedByWebId: Map<number, HostPlacedWebView> = new Map<number, HostPlacedWebView>();
    const zByWebId: Map<number, number> = new Map<number, number>();
    for (let i: number = 0; i < placements.length; i++) {
      const p: HostPlacedWebView = placements[i];
      placedByWebId.set(p.webId, p);
      zByWebId.set(p.webId, i + 1);
    }

    const activeSet: Map<number, boolean> = new Map<number, boolean>();
    const nextSlots: Array<EmbeddedWebSlotModel> = [];
    for (let i: number = 0; i < webIds.length; i++) {
      const wid: number = webIds[i];
      activeSet.set(wid, true);
      let model: EmbeddedWebSlotModel | undefined = this.slotMap.get(wid);
      if (model === undefined) {
        model = new EmbeddedWebSlotModel();
        model.slotKey = 'emb_web_' + wid.toString();
        model.webId = wid;
        const st: ManagedWebState = hostBridge.getOrCreateWebState(wid);
        model.incognito = (st.flags & WEB_SETUP_INCOGNITO) !== 0;
        model.autoplay = (st.flags & WEB_SETUP_AUTOPLAY) !== 0;
        model.pinchZoom = (st.flags & WEB_SETUP_PINCH_ZOOM) !== 0;
        model.scrollToContainer = (st.flags & WEB_SETUP_SCROLL_TO_CONTAINER) !== 0;
        model.slotBgHex = st.bgHex;
        const pair: [Array<ScriptItem>, Array<ScriptItem>] = buildHybridScriptLists(
          st.scripts,
          true,
          model.scrollToContainer
        );
        model.docStartScripts = pair[0];
        model.docEndScripts = pair[1];
        this.slotMap.set(wid, model);
      }
      const pl: HostPlacedWebView | undefined = placedByWebId.get(wid);
      if (pl !== undefined && pl.clipW > 0 && pl.clipH > 0) {
        model.placedVisible = true;
        model.paintZ = zByWebId.get(wid) ?? 1;
        model.frameX = pl.frameX;
        model.frameY = pl.frameY;
        model.frameW = pl.frameW;
        model.frameH = pl.frameH;
        model.clipX = pl.clipX;
        model.clipY = pl.clipY;
        model.clipW = pl.clipW;
        model.clipH = pl.clipH;
        model.pageW = pl.pageW > 0 ? pl.pageW : pl.frameW;
        model.pageH = pl.pageH > 0 ? pl.pageH : pl.frameH;
        model.r0 = pl.r0;
        model.r1 = pl.r1;
        model.r2 = pl.r2;
        model.r3 = pl.r3;
      } else {
        model.placedVisible = false;
      }
      nextSlots.push(model);
    }

    const deadIds: number[] = [];
    this.slotMap.forEach((_m: EmbeddedWebSlotModel, wid: number): void => {
      if (!activeSet.has(wid)) {
        deadIds.push(wid);
      }
    });
    for (let i: number = 0; i < deadIds.length; i++) {
      this.slotMap.delete(deadIds[i]);
    }
    this.embeddedSlots = nextSlots;

    if (placements.length === 0) {
      this.surfaceHitRects = [{ x: 0, y: 0, width: '100%', height: '100%' }];
    } else if (regions.length === 0) {
      this.surfaceHitRects = [{ x: 0, y: 0, width: 0, height: 0 }];
    } else {
      const rects: Array<Rectangle> = [];
      for (let i: number = 0; i < regions.length; i++) {
        const r: HostSurfaceRegion = regions[i];
        rects.push({
          x: r.x,
          y: r.y,
          width: r.w,
          height: r.h
        });
      }
      this.surfaceHitRects = rects;
    }
  }

  private dismissActiveMenu(chosenId: number): void {
    const reqId: number = this.activeMenuReqId;
    const node: ComponentContent<PopupMenuContext> | undefined = this.activeMenuNode;
    this.activeMenuReqId = 0;
    this.activeMenuNode = undefined;
    if (reqId !== 0) {
      host.resolvePopupMenu(reqId, chosenId);
    }
    if (node !== undefined) {
      this.getUIContext().getPromptAction().closeMenu(node).then((): void => {
        disposeDeferred((): void => {
          node.dispose();
        });
      }).catch((): void => {
        disposeDeferred((): void => {
          node.dispose();
        });
      });
    }
  }

  private dismissActiveMessageBox(): void {
    const reqId: number = this.activeMsgReqId;
    const node: ComponentContent<MessageBoxContext> | undefined = this.activeMsgNode;
    const cancelId: number = this.activeMsgCancelId;
    const checked: boolean = this.activeMsgChecked;
    this.activeMsgReqId = 0;
    this.activeMsgNode = undefined;
    if (reqId !== 0) {
      host.resolveDialog(reqId, cancelId, checked, '', []);
    }
    if (node !== undefined) {
      this.getUIContext().getPromptAction().closeCustomDialog(node).then((): void => {
        disposeDeferred((): void => {
          node.dispose();
        });
      }).catch((): void => {
        disposeDeferred((): void => {
          node.dispose();
        });
      });
    }
  }

  private openNativeMessageBox(box: HostMessageBox): void {
    this.dismissActiveMessageBox();
    const uiCtx: UIContext = this.getUIContext();
    let settled: boolean = false;
    let currentChecked: boolean = box.checkboxChecked;

    const finish = (response: number, closeExplicitly: boolean): void => {
      if (settled) {
        return;
      }
      settled = true;
      if (this.activeMsgReqId === box.reqId) {
        this.activeMsgReqId = 0;
        const curNode: ComponentContent<MessageBoxContext> | undefined = this.activeMsgNode;
        this.activeMsgNode = undefined;
        if (curNode !== undefined) {
          if (closeExplicitly) {
            uiCtx.getPromptAction().closeCustomDialog(curNode).then((): void => {
              disposeDeferred((): void => {
                curNode.dispose();
              });
            }).catch((): void => {
              disposeDeferred((): void => {
                curNode.dispose();
              });
            });
          } else {
            disposeDeferred((): void => {
              curNode.dispose();
            });
          }
        }
      }
      host.resolveDialog(box.reqId, response, currentChecked, '', []);
    };

    const ctx: MessageBoxContext = new MessageBoxContext();
    ctx.title = box.title;
    ctx.message = box.message;
    ctx.detail = box.detail;
    ctx.checkboxLabel = box.checkboxLabel;
    ctx.checkboxChecked = box.checkboxChecked;
    ctx.darkMode = AppStorage.get<boolean>('darkMode') ?? false;
    for (let i: number = 0; i < box.buttons.length; i++) {
      const bm: MessageBoxButtonModel = new MessageBoxButtonModel();
      bm.key = 'mb_btn_' + box.reqId.toString() + '_' + i.toString();
      bm.index = i;
      bm.label = box.buttons[i];
      bm.isDefault = i === box.defaultId;
      bm.isDestructive = box.destructiveIds.indexOf(i) >= 0;
      ctx.buttons.push(bm);
    }
    ctx.onToggleCheckbox = (checked: boolean): void => {
      currentChecked = checked;
      this.activeMsgChecked = checked;
    };
    ctx.onChoose = (idx: number): void => {
      finish(idx, true);
    };

    const contentNode: ComponentContent<MessageBoxContext> = new ComponentContent<MessageBoxContext>(
      uiCtx,
      wrapBuilder<[MessageBoxContext]>(buildMessageBoxDialog),
      ctx,
      { nestingBuilderSupported: true }
    );
    this.activeMsgReqId = box.reqId;
    this.activeMsgCancelId = box.cancelId;
    this.activeMsgChecked = box.checkboxChecked;
    this.activeMsgNode = contentNode;

    uiCtx.getPromptAction().openCustomDialog(contentNode, {
      alignment: DialogAlignment.Center,
      isModal: true,
      autoCancel: false,
      onWillDismiss: (dismissAction: DismissDialogAction): void => {
        dismissAction.dismiss();
        finish(box.cancelId, false);
      }
    }).catch((): void => {
      finish(box.cancelId, false);
    });
  }

  private openNativePopupMenu(reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void {
    this.dismissActiveMenu(0);
    const uiCtx: UIContext = this.getUIContext();
    const rootLevel: MenuLevelModel = buildMenuLevel(uiCtx, items, 'req_' + reqId.toString());
    if (rootLevel.sections.length === 0) {
      host.resolvePopupMenu(reqId, 0);
      return;
    }

    let settled: boolean = false;
    const finish = (chosenId: number, closeExplicitly: boolean): void => {
      if (settled) {
        return;
      }
      settled = true;
      if (this.activeMenuReqId === reqId) {
        this.activeMenuReqId = 0;
        const curNode: ComponentContent<PopupMenuContext> | undefined = this.activeMenuNode;
        this.activeMenuNode = undefined;
        if (curNode !== undefined) {
          if (closeExplicitly) {
            uiCtx.getPromptAction().closeMenu(curNode).then((): void => {
              disposeDeferred((): void => {
                curNode.dispose();
              });
            }).catch((): void => {
              disposeDeferred((): void => {
                curNode.dispose();
              });
            });
          } else {
            disposeDeferred((): void => {
              curNode.dispose();
            });
          }
        }
      }
      host.resolvePopupMenu(reqId, chosenId);
    };

    const ctx: PopupMenuContext = new PopupMenuContext();
    ctx.root = rootLevel;
    ctx.darkMode = AppStorage.get<boolean>('darkMode') ?? false;
    ctx.direction = rtl ? Direction.Rtl : Direction.Ltr;
    ctx.onSelect = (chosenId: number): void => {
      finish(chosenId, true);
    };

    const contentNode: ComponentContent<PopupMenuContext> = new ComponentContent<PopupMenuContext>(
      uiCtx,
      wrapBuilder<[PopupMenuContext]>(buildPopupMenuRoot),
      ctx,
      { nestingBuilderSupported: true }
    );
    this.activeMenuReqId = reqId;
    this.activeMenuNode = contentNode;

    uiCtx.getPromptAction().openMenu(
      contentNode,
      { id: this.rootStackId() },
      {
        anchorPosition: { x: Math.max(0, x), y: Math.max(0, y) },
        enableArrow: false,
        showInSubWindow: this.winId === 1,
        backgroundBlurStyle: BlurStyle.COMPONENT_ULTRA_THICK,
        backgroundBlurStyleOptions: {
          colorMode: ctx.darkMode ? ThemeColorMode.DARK : ThemeColorMode.LIGHT
        },
        onDisappear: (): void => {
          finish(0, false);
        }
      }
    ).catch((): void => {
      finish(0, false);
    });
  }

  private rootStackId(): string {
    return 'mygo_root_' + this.winId.toString();
  }

  build() {
    Stack({ alignContent: Alignment.TopStart }) {
      if (this.isWebWindow) {
        Web({
          src: '',
          controller: this.mainWebDriver.ctrl,
          incognitoMode: this.webIncognito
        })
          .id(this.mainWebDriver.compId())
          .width('100%')
          .height('100%')
          .backgroundColor(this.winBgHex)
          .javaScriptAccess(true)
          .domStorageAccess(true)
          .databaseAccess(true)
          .imageAccess(true)
          .onlineImageAccess(true)
          .mixedMode(MixedMode.All)
          .darkMode(WebDarkMode.Auto)
          .multiWindowAccess(true)
          .allowWindowOpenMethod(true)
          .mediaPlayGestureAccess(!this.mainAutoplay)
          .zoomAccess(this.mainPinchZoom)
          .runJavaScriptOnDocumentStart(this.mainDocStartScripts)
          .runJavaScriptOnDocumentEnd(this.mainDocEndScripts)
          .onControllerAttached((): void => {
            this.mainWebDriver.onAttached();
          })
          .onPageBegin((ev: OnPageBeginEvent): void => {
            this.mainWebDriver.onPageBegin(ev.url ?? '');
          })
          .onPageEnd((ev: OnPageEndEvent): void => {
            this.mainWebDriver.onPageEnd(ev.url ?? '');
          })
          .onTitleReceive((ev: OnTitleReceiveEvent): void => {
            this.mainWebDriver.onTitle(ev.title ?? '');
          })
          .onErrorReceive((ev: OnErrorReceiveEvent): void => {
            let mainFrame: boolean = false;
            let reqUrl: string = '';
            let code: number = -1;
            let info: string = '';
            try {
              mainFrame = ev.request.isMainFrame();
              reqUrl = ev.request.getRequestUrl();
            } catch (_) {}
            if (!mainFrame) {
              return;
            }
            try {
              code = ev.error.getErrorCode();
              info = ev.error.getErrorInfo();
            } catch (_) {}
            this.mainWebDriver.onMainError(reqUrl, code, info);
          })
          .onAudioStateChanged((ev: OnAudioStateChangedEvent): void => {
            this.mainWebDriver.onAudioStateChanged(ev.playing);
          })
          .onWindowNew((ev: OnWindowNewEvent): void => {
            this.mainWebDriver.onWindowNew(ev);
          })
          .onWindowExit((): void => {
            this.mainWebDriver.onWindowExit();
          })
          .onRenderExited((ev: OnRenderExitedEvent): void => {
            this.mainWebDriver.onRenderExited(ev.renderExitReason);
          })
          .onPermissionRequest((ev: OnPermissionRequestEvent): void => {
            this.mainWebDriver.onPermissionRequest(ev.request);
          })
          .onGeolocationShow((ev: OnGeolocationShowEvent): void => {
            this.mainWebDriver.onGeolocationShow(ev.origin, ev.geolocation);
          })
          .onDownloadStart((ev: OnDownloadStartEvent): void => {
            this.mainWebDriver.onDownloadStart(ev.url, ev.contentDisposition);
          })
      } else {
        ForEach(this.embeddedSlots, (slotItem: EmbeddedWebSlotModel) => {
          EmbeddedWebSlot({ slotModel: slotItem })
        }, (slotItem: EmbeddedWebSlotModel): string => slotItem.slotKey)

        Stack({ alignContent: Alignment.TopStart }) {
          ContentSlot(this.content)
        }
        .width('100%')
        .height('100%')
        .zIndex(10000)
        .responseRegion(this.surfaceHitRects)
        .mouseResponseRegion(this.surfaceHitRects)
      }
    }
    .id(this.rootStackId())
    .width('100%')
    .height('100%')
    .backgroundColor(this.winBgHex)
  }
}
