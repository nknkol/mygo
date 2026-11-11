import host, {
  HostBridgeCallbacks,
  HostClipRep,
  HostClipWrite,
  HostDisplayInfo,
  HostFileDialog,
  HostFileFilter,
  HostMenuItem,
  HostMessageBox,
  HostNotifShow,
  HostNowPlaying,
  HostPowerReq,
  HostWinConfig
} from 'libmygo_host.so';
import { Permissions, Want, WantAgent, abilityAccessCtrl, bundleManager, common, wantAgent } from '@kit.AbilityKit';
import { avSession } from '@kit.AVSessionKit';
import { preferences } from '@kit.ArkData';
import { util } from '@kit.ArkTS';
import { display, window } from '@kit.ArkUI';
import { backgroundTaskManager } from '@kit.BackgroundTasksKit';
import { BusinessError, batteryInfo, commonEventManager, pasteboard } from '@kit.BasicServicesKit';
import { fileIo, fileShare, fileUri, picker } from '@kit.CoreFileKit';
import { image } from '@kit.ImageKit';
import { i18n } from '@kit.LocalizationKit';
import { connection } from '@kit.NetworkKit';
import { notificationManager } from '@kit.NotificationKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x0000;
const TAG: string = 'mygo-bridge';

// Wire flags & constants matching mygo/internal/ohos/proto.go.
const WIN_FLAG_RESIZABLE: number = 1 << 0;
const WIN_FLAG_MINIMIZABLE: number = 1 << 1;
const WIN_FLAG_MAXIMIZABLE: number = 1 << 2;
const WIN_FLAG_CLOSABLE: number = 1 << 3;
const WIN_FLAG_MODAL: number = 1 << 4;
const WIN_FLAG_HIDDEN_TITLEBAR: number = 1 << 5;
const WIN_FLAG_HIDE_WIN_BUTTONS: number = 1 << 6;
const WIN_FLAG_ALWAYS_ON_TOP: number = 1 << 7;
const WIN_FLAG_MAXIMIZED: number = 1 << 8;
const WIN_FLAG_FULLSCREEN: number = 1 << 9;
const WIN_FLAG_CENTER: number = 1 << 10;
const WIN_FLAG_FRAMELESS: number = 1 << 11;
const WIN_FLAG_HAS_BG: number = 1 << 12;

const CFG_MASK_TITLE: number = 1 << 0;
const CFG_MASK_BOUNDS: number = 1 << 1;
const CFG_MASK_MIN_SIZE: number = 1 << 2;
const CFG_MASK_MAX_SIZE: number = 1 << 3;
const CFG_MASK_FLAGS: number = 1 << 4;
const CFG_MASK_BG: number = 1 << 5;
const CFG_MASK_ALL: number = 0x3F;

const WIN_ACT_SHOW: number = 1;
const WIN_ACT_SHOW_INACTIVE: number = 2;
const WIN_ACT_HIDE: number = 3;
const WIN_ACT_CLOSE: number = 4;
const WIN_ACT_FOCUS: number = 5;
const WIN_ACT_BLUR: number = 6;
const WIN_ACT_MINIMIZE: number = 7;
const WIN_ACT_MAXIMIZE: number = 8;
const WIN_ACT_UNMAXIMIZE: number = 9;
const WIN_ACT_RESTORE: number = 10;
const WIN_ACT_ENTER_FULLSCREEN: number = 11;
const WIN_ACT_EXIT_FULLSCREEN: number = 12;
const WIN_ACT_CENTER: number = 13;
const WIN_ACT_START_DRAG: number = 14;
const WIN_ACT_CLOSE_ALLOW: number = 15;
const WIN_ACT_CLOSE_DENY: number = 16;

const WIN_OP_FOCUS: number = 1;
const WIN_OP_BLUR: number = 2;
const WIN_OP_SHOW: number = 3;
const WIN_OP_HIDE: number = 4;
const WIN_OP_MINIMIZE: number = 7;
const WIN_OP_RESTORE: number = 8;
const WIN_OP_MAXIMIZE: number = 9;
const WIN_OP_UNMAXIMIZE: number = 10;
const WIN_OP_ENTER_FULLSCREEN: number = 11;
const WIN_OP_LEAVE_FULLSCREEN: number = 12;
const WIN_OP_CLOSE_REQUEST: number = 13;
const WIN_OP_CLOSED: number = 14;

const DLG_FLAG_OPEN_FILES: number = 1 << 0;
const DLG_FLAG_OPEN_DIRS: number = 1 << 1;
const DLG_FLAG_MULTIPLE: number = 1 << 2;

const SHELL_OPEN_EXTERNAL: number = 1;
const SHELL_OPEN_PATH: number = 2;
const SHELL_SHOW_IN_FOLDER: number = 3;
const SHELL_TRASH: number = 4;

const APP_OPEN_URLS: number = 1;
const APP_OPEN_FILES: number = 2;
const APP_OPEN_ACTIVATE: number = 3;

const MEDIA_CMD_PLAY: number = 1 << 0;
const MEDIA_CMD_PAUSE: number = 1 << 1;
const MEDIA_CMD_TOGGLE: number = 1 << 2;
const MEDIA_CMD_STOP: number = 1 << 3;
const MEDIA_CMD_NEXT: number = 1 << 4;
const MEDIA_CMD_PREV: number = 1 << 5;
const MEDIA_CMD_SEEK: number = 1 << 6;

const NOTIF_PARAM_KEY: string = 'mygoNotificationId';

function hashNotifId(id: string): number {
  let h: number = 0x811c9dc5;
  for (let i: number = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  const pos: number = h & 0x7fffffff;
  return pos > 0 ? pos : 1;
}

const PREF_STORE_NAME: string = 'mygo_file_perms';
const PREF_KEY_URIS: string = 'persisted_uris';

export type PopupMenuOpener = (reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]) => void;
export type MessageBoxOpener = (box: HostMessageBox) => void;

class ManagedWindow {
  winId: number = 1;
  arkWindowId: number = 0;
  win: window.Window | undefined = undefined;
  ready: boolean = false;
  active: boolean = false;
  closingFromEngine: boolean = false;
  lastStatus: window.WindowStatusType = window.WindowStatusType.FLOATING;
  cfg: HostWinConfig | undefined = undefined;
  pendingActions: number[] = [];
  willCloseResolve: ((prevent: boolean) => void) | undefined = undefined;
}

function rgbaToHexColor(rgba: number): string {
  const r: number = (rgba >>> 24) & 0xFF;
  const g: number = (rgba >>> 16) & 0xFF;
  const b: number = (rgba >>> 8) & 0xFF;
  const a: number = rgba & 0xFF;
  const to2 = (v: number): string => v.toString(16).padStart(2, '0');
  return '#' + to2(a) + to2(r) + to2(g) + to2(b);
}

function encodeUtf8Buffer(text: string): ArrayBuffer {
  const enc: util.TextEncoder = util.TextEncoder.create('utf-8');
  const u8: Uint8Array = enc.encodeInto(text);
  const out: ArrayBuffer = new ArrayBuffer(u8.byteLength);
  new Uint8Array(out).set(u8);
  return out;
}

function splitPathDirAndBase(fullPath: string): [string, string] {
  const trimmed: string = fullPath.length > 1 && fullPath.endsWith('/')
    ? fullPath.substring(0, fullPath.length - 1)
    : fullPath;
  const slash: number = trimmed.lastIndexOf('/');
  if (slash <= 0) {
    return ['/', trimmed.substring(slash + 1)];
  }
  return [trimmed.substring(0, slash), trimmed.substring(slash + 1)];
}

function formatPickerFilters(filters: HostFileFilter[]): string[] {
  const out: string[] = [];
  for (let i: number = 0; i < filters.length; i++) {
    const f: HostFileFilter = filters[i];
    if (f.extensions.length === 0) {
      continue;
    }
    if (f.extensions.indexOf('*') >= 0) {
      return [];
    }
    const suffixes: string[] = [];
    for (let j: number = 0; j < f.extensions.length; j++) {
      const ext: string = f.extensions[j].trim();
      if (ext.length > 0) {
        suffixes.push(ext.startsWith('.') ? ext : ('.' + ext));
      }
    }
    if (suffixes.length > 0) {
      const desc: string = f.name.trim().length > 0 ? f.name.trim().replace(/\|/g, ' ') : 'Files';
      out.push(desc + '|' + suffixes.join(','));
    }
  }
  return out;
}

class HostBridgeManager {
  private context: common.UIAbilityContext | undefined = undefined;
  private windowStage: window.WindowStage | undefined = undefined;
  private windows: Map<number, ManagedWindow> = new Map<number, ManagedWindow>();
  private popupOpeners: Map<number, PopupMenuOpener> = new Map<number, PopupMenuOpener>();
  private msgBoxOpeners: Map<number, MessageBoxOpener> = new Map<number, MessageBoxOpener>();
  private displayListenersRegistered: boolean = false;
  private pasteboardListenerRegistered: boolean = false;
  private callbacksRegistered: boolean = false;
  private lastSelfChangeCount: number = -1;
  private tokenId: number = 0;
  private bundleName: string = 'dev.egoist.music';
  private bundleLabel: string = 'MyGo';
  private mediaSession: avSession.AVSession | undefined = undefined;
  private mediaCoverPm: image.PixelMap | undefined = undefined;
  private mediaMetaCache: avSession.AVMetadata | undefined = undefined;
  private registeredMediaCmds: number = 0;
  private avChain: Promise<void> = Promise.resolve();
  private npPlaying: boolean = false;
  private powerKeepDisplay: boolean = false;
  private powerKeepRunning: boolean = false;
  private bgRunningActive: boolean = false;
  private bgChain: Promise<void> = Promise.resolve();
  private notifPermAsked: boolean = false;
  private powerSubscriber: commonEventManager.CommonEventSubscriber | undefined = undefined;
  private netConn: connection.NetConnection | undefined = undefined;
  private netWatching: boolean = false;
  private pasteboardUpdateHandler: () => void = (): void => {
    this.onSystemPasteboardUpdated();
  };

  init(ctx: common.UIAbilityContext): void {
    this.context = ctx;
    try {
      const bInfo: bundleManager.BundleInfo = bundleManager.getBundleInfoForSelfSync(
        bundleManager.BundleFlag.GET_BUNDLE_INFO_WITH_APPLICATION
      );
      if (bInfo.name.length > 0) {
        this.bundleName = bInfo.name;
      }
      if (bInfo.appInfo !== undefined) {
        this.tokenId = bInfo.appInfo.accessTokenId;
        try {
          const lbl: string = ctx.resourceManager.getStringSync(bInfo.appInfo.labelResource);
          if (lbl.length > 0) {
            this.bundleLabel = lbl;
          }
        } catch (_) {}
      }
    } catch (_) {}

    if (!this.callbacksRegistered) {
      this.callbacksRegistered = true;
      const cb: HostBridgeCallbacks = {
        onWinCreate: (cfg: HostWinConfig): void => {
          this.handleWinCreate(cfg);
        },
        onWinConfig: (cfg: HostWinConfig): void => {
          this.handleWinConfig(cfg);
        },
        onWinAction: (winId: number, action: number): void => {
          this.handleWinAction(winId, action);
        },
        onPopupMenu: (reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void => {
          this.handlePopupMenu(reqId, winId, x, y, rtl, items);
        },
        onClipWrite: (data: HostClipWrite): void => {
          this.handleClipWrite(data);
        },
        onClipRead: (reqId: number, formats: string[]): void => {
          this.handleClipRead(reqId, formats);
        },
        onDialogOpen: (dlg: HostFileDialog): void => {
          this.handleDialogOpen(dlg);
        },
        onDialogSave: (dlg: HostFileDialog): void => {
          this.handleDialogSave(dlg);
        },
        onDialogMsg: (box: HostMessageBox): void => {
          this.handleDialogMsg(box);
        },
        onShellReq: (reqId: number, op: number, target: string): void => {
          this.handleShellReq(reqId, op, target);
        },
        onNowPlaying: (np: HostNowPlaying): void => {
          this.handleNowPlaying(np);
        },
        onNotifShow: (notif: HostNotifShow): void => {
          this.handleNotifShow(notif);
        },
        onNotifRemove: (all: boolean, id: string): void => {
          this.handleNotifRemove(all, id);
        },
        onPowerReq: (req: HostPowerReq): void => {
          this.handlePowerReq(req);
        },
        onNetWatch: (): void => {
          this.handleNetWatch();
        },
        onEngineQuit: (): void => {
          this.handleEngineQuit();
        },
        onEngineRestart: (): void => {
          this.handleEngineRestart();
        }
      };
      host.setBridgeCallbacks(cb);
    }
    this.refreshEnvAndDisplays();
    this.registerDisplayListeners();
    this.registerPasteboardListener();
    this.registerPowerEvents();
    this.queryAndNotifyNetState();
    this.reactivatePersistedUris();
  }

  onAppForeground(): void {
    host.notifyWindowEvent(1, WIN_OP_SHOW);
  }

  onAppBackground(): void {
    host.notifyWindowEvent(1, WIN_OP_HIDE);
  }

  handleWant(want: Want, isNewWant: boolean): void {
    const notifParam: Object | undefined = want.parameters?.[NOTIF_PARAM_KEY];
    if (typeof notifParam === 'string' && notifParam.length > 0) {
      host.notifyNotifClicked(notifParam);
    }
    const rawUri: string = (want.uri ?? '').trim();
    if (rawUri.length > 0) {
      if (rawUri.toLowerCase().startsWith('file://')) {
        this.resolveIncomingFileUri(rawUri).then((resolved: string): void => {
          if (resolved.length > 0) {
            host.notifyAppOpen(APP_OPEN_FILES, [resolved]);
          }
        }).catch((): void => {});
      } else {
        host.notifyAppOpen(APP_OPEN_URLS, [rawUri]);
      }
    }
    if (isNewWant) {
      host.notifyAppOpen(APP_OPEN_ACTIVATE, []);
      const mainMw: ManagedWindow | undefined = this.windows.get(1);
      if (mainMw !== undefined && mainMw.win !== undefined) {
        try {
          mainMw.win.showWindow().then((): void => {
            this.focusManagedWindow(mainMw);
          }).catch((): void => {});
        } catch (_) {}
      }
    }
  }

  attachMainStage(stage: window.WindowStage, mainWin: window.Window): void {
    this.windowStage = stage;
    let mw: ManagedWindow | undefined = this.windows.get(1);
    if (mw === undefined) {
      mw = new ManagedWindow();
      mw.winId = 1;
      this.windows.set(1, mw);
    }
    mw.win = mainWin;
    try {
      const props: window.WindowProperties = mainWin.getWindowProperties();
      mw.arkWindowId = props.id;
    } catch (_) {}
    mw.active = true;
    this.attachWindowListeners(mw);
  }

  onMainContentLoaded(): void {
    const mw: ManagedWindow | undefined = this.windows.get(1);
    if (mw === undefined || mw.win === undefined) {
      return;
    }
    mw.ready = true;
    this.reportWindowRect(mw);
    if (mw.cfg !== undefined) {
      this.applyWindowConfig(mw, mw.cfg, CFG_MASK_ALL, true);
    }
    if (this.powerKeepDisplay) {
      try { mw.win.setWindowKeepScreenOn(true).catch((): void => {}); } catch (_) {}
    }
    this.flushPendingActions(mw);
  }

  destroyStage(): void {
    this.unregisterDisplayListeners();
    this.unregisterPasteboardListener();
    this.unregisterPowerEvents();
    this.unregisterNetWatch();
    this.teardownAVSessionAndBackground();
    this.windows.forEach((mw: ManagedWindow): void => {
      this.detachWindowListeners(mw);
    });
    this.windows.clear();
    this.popupOpeners.clear();
    this.msgBoxOpeners.clear();
    this.windowStage = undefined;
  }

  getArkWindowId(winId: number): number {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw !== undefined && mw.arkWindowId > 0) {
      return mw.arkWindowId;
    }
    return AppStorage.get<number>('windowId') ?? 0;
  }

  registerPopupMenuOpener(winId: number, opener: PopupMenuOpener): void {
    this.popupOpeners.set(winId, opener);
  }

  unregisterPopupMenuOpener(winId: number): void {
    this.popupOpeners.delete(winId);
  }

  registerMessageBoxOpener(winId: number, opener: MessageBoxOpener): void {
    this.msgBoxOpeners.set(winId, opener);
  }

  unregisterMessageBoxOpener(winId: number): void {
    this.msgBoxOpeners.delete(winId);
  }

  // ---------- Pasteboard Bridge ----------

  private registerPasteboardListener(): void {
    if (this.pasteboardListenerRegistered) {
      return;
    }
    this.pasteboardListenerRegistered = true;
    try {
      const sysBoard: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
      sysBoard.on('update', this.pasteboardUpdateHandler);
      this.onSystemPasteboardUpdated();
    } catch (_) {}
  }

  private unregisterPasteboardListener(): void {
    if (!this.pasteboardListenerRegistered) {
      return;
    }
    this.pasteboardListenerRegistered = false;
    try {
      const sysBoard: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
      sysBoard.off('update', this.pasteboardUpdateHandler);
    } catch (_) {}
  }

  private onSystemPasteboardUpdated(): void {
    try {
      const sysBoard: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
      const count: number = sysBoard.getChangeCount();
      if (count === this.lastSelfChangeCount) {
        return;
      }
      sysBoard.getMimeTypes().then((mimes: string[]): void => {
        if (sysBoard.getChangeCount() === this.lastSelfChangeCount) {
          return;
        }
        host.notifyClipChanged(count >>> 0, true, mimes);
      }).catch((): void => {
        host.notifyClipChanged(count >>> 0, true, []);
      });
    } catch (_) {}
  }

  private handleClipWrite(data: HostClipWrite): void {
    this.writePasteboardAsync(data).catch((err: BusinessError): void => {
      hilog.error(DOMAIN, TAG, 'handleClipWrite failed: %{public}s', JSON.stringify(err));
    });
  }

  private async writePasteboardAsync(data: HostClipWrite): Promise<void> {
    const sysBoard: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    if (data.clear) {
      await sysBoard.clearData();
      this.lastSelfChangeCount = sysBoard.getChangeCount();
      return;
    }

    let pd: pasteboard.PasteData | undefined = undefined;
    let firstRec: pasteboard.PasteDataRecord | undefined = undefined;

    const addPrimaryOrExtra = (mime: string, val: pasteboard.ValueType): void => {
      if (pd === undefined) {
        pd = pasteboard.createData(mime, val);
        firstRec = pd.getRecord(0);
      } else if (firstRec !== undefined) {
        firstRec.addEntry(mime, val);
      }
    };

    if (data.html.length > 0) {
      addPrimaryOrExtra(pasteboard.MIMETYPE_TEXT_HTML, data.html);
    }
    if (data.text.length > 0 || (data.html.length === 0 && data.uriList.length === 0 && data.png.byteLength === 0)) {
      addPrimaryOrExtra(pasteboard.MIMETYPE_TEXT_PLAIN, data.text);
    }
    if (data.png.byteLength > 0) {
      let imgSrc: image.ImageSource | undefined = undefined;
      try {
        imgSrc = image.createImageSource(data.png);
        const pm: image.PixelMap = await imgSrc.createPixelMap();
        addPrimaryOrExtra(pasteboard.MIMETYPE_PIXELMAP, pm);
      } catch (e) {
        hilog.error(DOMAIN, TAG, 'decode clipboard PNG failed: %{public}s', JSON.stringify(e));
      } finally {
        if (imgSrc !== undefined) {
          try { await imgSrc.release(); } catch (_) {}
        }
      }
    }
    if (data.uriList.length > 0) {
      const lines: string[] = data.uriList.replace(/\r\n/g, '\n').split('\n');
      let addedFirstUri: boolean = false;
      for (let i: number = 0; i < lines.length; i++) {
        const u: string = lines[i].trim();
        if (u.length === 0 || u.startsWith('#')) {
          continue;
        }
        if (pd === undefined) {
          pd = pasteboard.createData(pasteboard.MIMETYPE_TEXT_URI, u);
          firstRec = pd.getRecord(0);
          addedFirstUri = true;
        } else if (!addedFirstUri && firstRec !== undefined) {
          firstRec.addEntry(pasteboard.MIMETYPE_TEXT_URI, u);
          addedFirstUri = true;
        } else {
          pd.addRecord(pasteboard.MIMETYPE_TEXT_URI, u);
        }
      }
    }

    if (pd === undefined) {
      await sysBoard.clearData();
      this.lastSelfChangeCount = sysBoard.getChangeCount();
      return;
    }

    const prop: pasteboard.PasteDataProperty = pd.getProperty();
    prop.shareOption = data.concealed ? pasteboard.ShareOption.INAPP : pasteboard.ShareOption.LOCALDEVICE;
    pd.setProperty(prop);

    await sysBoard.setData(pd);
    this.lastSelfChangeCount = sysBoard.getChangeCount();
  }

  private handleClipRead(reqId: number, formats: string[]): void {
    this.readPasteboardAsync(formats).then((reps: HostClipRep[]): void => {
      host.resolveClipRead(reqId, '', reps);
    }).catch((err: BusinessError): void => {
      const msg: string = err?.message ?? 'pasteboard read failed';
      host.resolveClipRead(reqId, msg, []);
    });
  }

  private async ensurePasteboardPermission(): Promise<boolean> {
    if (this.context === undefined) {
      return false;
    }
    const perm: Permissions = 'ohos.permission.READ_PASTEBOARD';
    const atMgr: abilityAccessCtrl.AtManager = abilityAccessCtrl.createAtManager();
    try {
      if (this.tokenId !== 0) {
        const status: abilityAccessCtrl.GrantStatus = atMgr.checkAccessTokenSync(this.tokenId, perm);
        if (status === abilityAccessCtrl.GrantStatus.PERMISSION_GRANTED) {
          return true;
        }
      }
      const res = await atMgr.requestPermissionsFromUser(this.context, [perm]);
      if (res.authResults.length > 0 && res.authResults[0] === 0) {
        return true;
      }
    } catch (_) {}
    return false;
  }

  private async readPasteboardAsync(formats: string[]): Promise<HostClipRep[]> {
    const sysBoard: pasteboard.SystemPasteboard = pasteboard.getSystemPasteboard();
    if (!sysBoard.hasDataSync()) {
      return [];
    }
    await this.ensurePasteboardPermission();

    let pd: pasteboard.PasteData;
    try {
      pd = await sysBoard.getData();
    } catch (e) {
      const be: BusinessError = e as BusinessError;
      if (be.code === 201 || be.code === 12900005) {
        return [];
      }
      throw e as Error;
    }

    const wantAll: boolean = formats.length === 0;
    const wants = (fmt: string): boolean => {
      if (wantAll) {
        return true;
      }
      for (let i: number = 0; i < formats.length; i++) {
        if (formats[i] === fmt) {
          return true;
        }
      }
      return false;
    };

    const reps: HostClipRep[] = [];
    const recCount: number = pd.getRecordCount();

    if (wants('text/html')) {
      let htmlVal: string = '';
      try {
        htmlVal = pd.getPrimaryHtml() ?? '';
      } catch (_) {}
      if (htmlVal.length === 0 && recCount > 0) {
        try {
          const v: pasteboard.ValueType = await pd.getRecord(0).getData(pasteboard.MIMETYPE_TEXT_HTML);
          if (typeof v === 'string') {
            htmlVal = v;
          }
        } catch (_) {}
      }
      if (htmlVal.length > 0) {
        reps.push({ format: 'text/html', data: encodeUtf8Buffer(htmlVal) });
      }
    }

    if (wants('text/plain')) {
      const textParts: string[] = [];
      for (let i: number = 0; i < recCount; i++) {
        const rec: pasteboard.PasteDataRecord = pd.getRecord(i);
        let t: string = '';
        try {
          const v: pasteboard.ValueType = await rec.getData(pasteboard.MIMETYPE_TEXT_PLAIN);
          if (typeof v === 'string') {
            t = v;
          }
        } catch (_) {}
        if (t.length === 0) {
          try {
            t = rec.toPlainText() ?? '';
          } catch (_) {}
        }
        if (t.length > 0) {
          textParts.push(t);
        }
      }
      if (textParts.length > 0) {
        reps.push({ format: 'text/plain', data: encodeUtf8Buffer(textParts.join('\n')) });
      }
    }

    if (wants('text/uri-list') || wants('application/x-mygo-file-list')) {
      const uris: string[] = [];
      for (let i: number = 0; i < recCount; i++) {
        const rec: pasteboard.PasteDataRecord = pd.getRecord(i);
        try {
          const v: pasteboard.ValueType = await rec.getData(pasteboard.MIMETYPE_TEXT_URI);
          if (typeof v === 'string' && v.length > 0) {
            uris.push(v);
          }
        } catch (_) {}
      }
      if (uris.length === 0) {
        try {
          const pu: string = pd.getPrimaryUri() ?? '';
          if (pu.length > 0) {
            uris.push(pu);
          }
        } catch (_) {}
      }
      if (uris.length > 0) {
        reps.push({ format: 'text/uri-list', data: encodeUtf8Buffer(uris.join('\r\n')) });
      }
    }

    if (wants('image/png')) {
      let pm: image.PixelMap | undefined = undefined;
      try {
        pm = pd.getPrimaryPixelMap();
      } catch (_) {}
      if (pm === undefined && recCount > 0) {
        try {
          const v: pasteboard.ValueType = await pd.getRecord(0).getData(pasteboard.MIMETYPE_PIXELMAP);
          if (v !== undefined && typeof v === 'object' && !(v instanceof ArrayBuffer)) {
            pm = v as image.PixelMap;
          }
        } catch (_) {}
      }
      if (pm !== undefined) {
        const packer: image.ImagePacker = image.createImagePacker();
        try {
          const pngBuf: ArrayBuffer = await packer.packToData(pm, { format: 'image/png', quality: 100 });
          if (pngBuf.byteLength > 0) {
            reps.push({ format: 'image/png', data: pngBuf });
          }
        } catch (_) {
        } finally {
          try { await packer.release(); } catch (_) {}
        }
      }
    }

    for (let fIdx: number = 0; fIdx < formats.length; fIdx++) {
      const customFmt: string = formats[fIdx];
      if (customFmt === 'text/plain' || customFmt === 'text/html' ||
          customFmt === 'text/uri-list' || customFmt === 'application/x-mygo-file-list' ||
          customFmt === 'image/png') {
        continue;
      }
      if (recCount > 0) {
        try {
          const v: pasteboard.ValueType = await pd.getRecord(0).getData(customFmt);
          if (v instanceof ArrayBuffer) {
            reps.push({ format: customFmt, data: v });
          } else if (typeof v === 'string') {
            reps.push({ format: customFmt, data: encodeUtf8Buffer(v) });
          }
        } catch (_) {}
      }
    }

    return reps;
  }

  // ---------- Dialogs, FileURI Persistence & Shell ----------

  private reactivatePersistedUris(): void {
    if (this.context === undefined) {
      return;
    }
    try {
      const pref: preferences.Preferences = preferences.getPreferencesSync(this.context, { name: PREF_STORE_NAME });
      const raw: preferences.ValueType = pref.getSync(PREF_KEY_URIS, []);
      if (Array.isArray(raw) && raw.length > 0) {
        const policies: fileShare.PolicyInfo[] = [];
        for (let i: number = 0; i < raw.length; i++) {
          const u: string = String(raw[i]);
          if (u.length > 0) {
            policies.push({
              uri: u,
              operationMode: fileShare.OperationMode.READ_MODE | fileShare.OperationMode.WRITE_MODE
            });
          }
        }
        if (policies.length > 0) {
          fileShare.activatePermission(policies).catch((): void => {});
        }
      }
    } catch (_) {}
  }

  private async persistAndRecordUris(uris: string[], writable: boolean): Promise<void> {
    if (uris.length === 0 || this.context === undefined) {
      return;
    }
    const mode: number = writable
      ? (fileShare.OperationMode.READ_MODE | fileShare.OperationMode.WRITE_MODE)
      : fileShare.OperationMode.READ_MODE;
    const policies: fileShare.PolicyInfo[] = [];
    for (let i: number = 0; i < uris.length; i++) {
      if (uris[i].length > 0) {
        policies.push({ uri: uris[i], operationMode: mode });
      }
    }
    if (policies.length === 0) {
      return;
    }
    try {
      await fileShare.persistPermission(policies);
      const pref: preferences.Preferences = preferences.getPreferencesSync(this.context, { name: PREF_STORE_NAME });
      const prevRaw: preferences.ValueType = pref.getSync(PREF_KEY_URIS, []);
      const merged: string[] = [];
      if (Array.isArray(prevRaw)) {
        for (let i: number = 0; i < prevRaw.length; i++) {
          const s: string = String(prevRaw[i]);
          if (s.length > 0 && merged.indexOf(s) < 0) {
            merged.push(s);
          }
        }
      }
      for (let i: number = 0; i < uris.length; i++) {
        if (uris[i].length > 0 && merged.indexOf(uris[i]) < 0) {
          merged.push(uris[i]);
        }
      }
      while (merged.length > 500) {
        merged.shift();
      }
      pref.putSync(PREF_KEY_URIS, merged);
      await pref.flush();
    } catch (_) {}
  }

  private async resolveIncomingFileUri(uri: string, writable: boolean = false): Promise<string> {
    await this.persistAndRecordUris([uri], writable);
    let directPath: string = '';
    try {
      const fu: fileUri.FileUri = new fileUri.FileUri(uri);
      directPath = fu.path;
    } catch (_) {}

    if (directPath.length > 0) {
      try {
        if (fileIo.accessSync(directPath)) {
          return directPath;
        }
      } catch (_) {}
      if (writable) {
        return directPath;
      }
    }

    if (this.context === undefined) {
      return directPath;
    }

    // Fallback for virtual URIs whose underlying path is not directly accessible:
    // copy into sandbox cache directory so Go's os.Open reads the file seamlessly.
    let srcFile: fileIo.File | undefined = undefined;
    try {
      srcFile = fileIo.openSync(uri, fileIo.OpenMode.READ_ONLY);
      const cacheDir: string = this.context.cacheDir + '/picked';
      try { fileIo.mkdirSync(cacheDir, true); } catch (_) {}
      const baseName: string = srcFile.name && srcFile.name.length > 0
        ? srcFile.name
        : ('file_' + Date.now().toString());
      const dstPath: string = cacheDir + '/' + baseName;
      fileIo.copyFileSync(srcFile.fd, dstPath);
      return dstPath;
    } catch (_) {
      return directPath;
    } finally {
      if (srcFile !== undefined) {
        try { fileIo.closeSync(srcFile); } catch (_) {}
      }
    }
  }

  private createDocumentPicker(winId: number): picker.DocumentViewPicker | undefined {
    if (this.context === undefined) {
      return undefined;
    }
    const mw: ManagedWindow | undefined = this.windows.get(winId) ?? this.windows.get(1);
    if (mw !== undefined && mw.win !== undefined) {
      try {
        return new picker.DocumentViewPicker(this.context, mw.win);
      } catch (_) {}
    }
    return new picker.DocumentViewPicker(this.context);
  }

  private handleDialogOpen(dlg: HostFileDialog): void {
    this.runOpenDialogAsync(dlg).then((paths: string[]): void => {
      host.resolveDialog(dlg.reqId, 0, false, '', paths);
    }).catch((err: BusinessError): void => {
      const msg: string = err?.message ?? 'open dialog failed';
      host.resolveDialog(dlg.reqId, 0, false, msg, []);
    });
  }

  private async runOpenDialogAsync(dlg: HostFileDialog): Promise<string[]> {
    const docPicker: picker.DocumentViewPicker | undefined = this.createDocumentPicker(dlg.winId);
    if (docPicker === undefined) {
      return [];
    }
    const opts: picker.DocumentSelectOptions = new picker.DocumentSelectOptions();
    const wantFiles: boolean = (dlg.flags & DLG_FLAG_OPEN_FILES) !== 0;
    const wantDirs: boolean = (dlg.flags & DLG_FLAG_OPEN_DIRS) !== 0;
    const multiple: boolean = (dlg.flags & DLG_FLAG_MULTIPLE) !== 0;

    if (wantFiles && wantDirs) {
      opts.selectMode = picker.DocumentSelectMode.MIXED;
    } else if (wantDirs) {
      opts.selectMode = picker.DocumentSelectMode.FOLDER;
    } else {
      opts.selectMode = picker.DocumentSelectMode.FILE;
    }
    opts.maxSelectNumber = multiple ? 500 : 1;

    if (dlg.defaultPath.length > 0) {
      try {
        opts.defaultFilePathUri = fileUri.getUriFromPath(dlg.defaultPath);
      } catch (_) {}
    }
    const suffixes: string[] = formatPickerFilters(dlg.filters);
    if (suffixes.length > 0 && opts.selectMode !== picker.DocumentSelectMode.FOLDER) {
      opts.fileSuffixFilters = suffixes;
    }

    const uris: string[] = await docPicker.select(opts);
    if (uris === undefined || uris.length === 0) {
      return [];
    }
    await this.persistAndRecordUris(uris, wantDirs);
    const out: string[] = [];
    for (let i: number = 0; i < uris.length; i++) {
      const p: string = await this.resolveIncomingFileUri(uris[i], wantDirs);
      if (p.length > 0) {
        out.push(p);
      }
    }
    return out;
  }

  private handleDialogSave(dlg: HostFileDialog): void {
    this.runSaveDialogAsync(dlg).then((savedPath: string): void => {
      const paths: string[] = savedPath.length > 0 ? [savedPath] : [];
      host.resolveDialog(dlg.reqId, 0, false, '', paths);
    }).catch((err: BusinessError): void => {
      const msg: string = err?.message ?? 'save dialog failed';
      host.resolveDialog(dlg.reqId, 0, false, msg, []);
    });
  }

  private async runSaveDialogAsync(dlg: HostFileDialog): Promise<string> {
    const docPicker: picker.DocumentViewPicker | undefined = this.createDocumentPicker(dlg.winId);
    if (docPicker === undefined) {
      return '';
    }
    const opts: picker.DocumentSaveOptions = new picker.DocumentSaveOptions();
    if (dlg.defaultPath.length > 0) {
      if (dlg.defaultPath.startsWith('/')) {
        const parts: [string, string] = splitPathDirAndBase(dlg.defaultPath);
        if (parts[0].length > 0) {
          try {
            opts.defaultFilePathUri = fileUri.getUriFromPath(parts[0]);
          } catch (_) {}
        }
        if (parts[1].length > 0) {
          opts.newFileNames = [parts[1]];
        }
      } else {
        opts.newFileNames = [dlg.defaultPath];
      }
    }
    const suffixes: string[] = formatPickerFilters(dlg.filters);
    if (suffixes.length > 0) {
      opts.fileSuffixChoices = suffixes;
    }

    const uris: string[] = await docPicker.save(opts);
    if (uris === undefined || uris.length === 0) {
      return '';
    }
    return await this.resolveIncomingFileUri(uris[0], true);
  }

  private handleDialogMsg(box: HostMessageBox): void {
    const opener: MessageBoxOpener | undefined = this.msgBoxOpeners.get(box.winId) ?? this.msgBoxOpeners.get(1);
    if (opener === undefined) {
      host.resolveDialog(box.reqId, box.cancelId, box.checkboxChecked, '', []);
      return;
    }
    opener(box);
  }

  private handleShellReq(reqId: number, op: number, target: string): void {
    this.runShellReqAsync(op, target).then((): void => {
      host.resolveShell(reqId, '');
    }).catch((err: BusinessError): void => {
      const msg: string = err?.message ?? 'shell operation failed';
      host.resolveShell(reqId, msg);
    });
  }

  private async runShellReqAsync(op: number, target: string): Promise<void> {
    if (this.context === undefined) {
      throw new Error('UIAbilityContext unavailable');
    }
    const ctx: common.UIAbilityContext = this.context;

    switch (op) {
      case SHELL_OPEN_EXTERNAL: {
        try {
          await ctx.openLink(target, { appLinkingOnly: false });
        } catch (_) {
          const want: Want = {
            action: 'ohos.want.action.viewData',
            uri: target
          };
          await ctx.startAbility(want);
        }
        return;
      }
      case SHELL_OPEN_PATH: {
        const uri: string = fileUri.getUriFromPath(target);
        const want: Want = {
          action: 'ohos.want.action.viewData',
          uri: uri
        };
        await ctx.startAbility(want);
        return;
      }
      case SHELL_SHOW_IN_FOLDER: {
        let dirPath: string = target;
        try {
          if (!fileIo.statSync(target).isDirectory()) {
            const parts: [string, string] = splitPathDirAndBase(target);
            dirPath = parts[0];
          }
        } catch (_) {
          const parts: [string, string] = splitPathDirAndBase(target);
          dirPath = parts[0];
        }
        const uri: string = fileUri.getUriFromPath(dirPath);
        const want: Want = {
          action: 'ohos.want.action.viewData',
          uri: uri
        };
        await ctx.startAbility(want);
        return;
      }
      case SHELL_TRASH: {
        const trashDir: string = ctx.filesDir + '/.Trash';
        try { fileIo.mkdirSync(trashDir, true); } catch (_) {}
        const parts: [string, string] = splitPathDirAndBase(target);
        const base: string = parts[1].length > 0 ? parts[1] : 'trashed';
        let dst: string = trashDir + '/' + base;
        try {
          if (fileIo.accessSync(dst)) {
            dst = trashDir + '/' + Date.now().toString() + '_' + base;
          }
        } catch (_) {}
        try {
          fileIo.renameSync(target, dst);
        } catch (_) {
          const st: fileIo.Stat = fileIo.statSync(target);
          if (st.isDirectory()) {
            fileIo.rmdirSync(target);
          } else {
            fileIo.unlinkSync(target);
          }
        }
        return;
      }
      default:
        throw new Error('unsupported shell op');
    }
  }

  // ---------- Environment & Multi-Window Management ----------

  refreshEnvAndDisplays(): void {
    let pkgId: string = 'dev.egoist.music';
    let pkgVer: string = '0.1.0';
    let pkgName: string = 'MyGo Music';
    try {
      const bInfo: bundleManager.BundleInfo = bundleManager.getBundleInfoForSelfSync(
        bundleManager.BundleFlag.GET_BUNDLE_INFO_WITH_APPLICATION
      );
      if (bInfo.name.length > 0) {
        pkgId = bInfo.name;
      }
      if (bInfo.versionName.length > 0) {
        pkgVer = bInfo.versionName;
      }
      if (this.context !== undefined && bInfo.appInfo !== undefined && bInfo.appInfo.labelId > 0) {
        const label: string = this.context.resourceManager.getStringSync(bInfo.appInfo.labelId);
        if (label.length > 0) {
          pkgName = label;
        }
      }
    } catch (_) {}

    let langs: string[] = [];
    let locale: string = '';
    let hourCycle: number = 24;
    let firstWeekday: number = -1;
    try {
      langs = i18n.System.getPreferredLanguageList();
    } catch (_) {}
    try {
      locale = i18n.System.getSystemLocaleInstance().toString();
    } catch (_) {}
    if (langs.length === 0 && locale.length > 0) {
      langs = [locale];
    }
    try {
      hourCycle = i18n.System.is24HourClock() ? 24 : 12;
    } catch (_) {}
    try {
      const wd: number = i18n.System.getFirstDayOfWeek();
      if (wd >= 1 && wd <= 7) {
        firstWeekday = wd % 7;
      }
    } catch (_) {}

    let defaultId: number = 0;
    let rawDisplays: display.Display[] = [];
    try {
      const defDisp: display.Display = display.getDefaultDisplaySync();
      defaultId = defDisp.id;
      rawDisplays = [defDisp];
    } catch (_) {}

    const sendSnapshot = (disps: display.Display[], workAreas: Map<number, display.Rect>): void => {
      const list: HostDisplayInfo[] = [];
      for (let i: number = 0; i < disps.length; i++) {
        const d: display.Display = disps[i];
        const sc: number = d.densityPixels > 0 ? d.densityPixels : 1.0;
        const xDip: number = Math.round((d.x ?? 0) / sc);
        const yDip: number = Math.round((d.y ?? 0) / sc);
        const wDip: number = Math.max(1, Math.round(d.width / sc));
        const hDip: number = Math.max(1, Math.round(d.height / sc));
        let wx: number = xDip;
        let wy: number = yDip;
        let ww: number = wDip;
        let wh: number = hDip;
        const area: display.Rect | undefined = workAreas.get(d.id);
        if (area !== undefined && area.width > 0 && area.height > 0) {
          wx = xDip + Math.round(area.left / sc);
          wy = yDip + Math.round(area.top / sc);
          ww = Math.max(1, Math.round(area.width / sc));
          wh = Math.max(1, Math.round(area.height / sc));
        }
        const label: string = (d.name !== undefined && d.name.length > 0) ? d.name : ('Display ' + d.id.toString());
        const item: HostDisplayInfo = {
          id: d.id,
          label: label,
          x: xDip,
          y: yDip,
          w: wDip,
          h: hDip,
          wx: wx,
          wy: wy,
          ww: ww,
          wh: wh,
          scale: sc,
          rotation: (d.rotation ?? 0) * 90,
          primary: d.id === defaultId,
          internal: d.id === 0
        };
        list.push(item);
      }
      host.updateEnvInfo(pkgName, pkgVer, pkgId, locale, firstWeekday, hourCycle, langs, list);
    };

    const emptyAreas: Map<number, display.Rect> = new Map<number, display.Rect>();
    sendSnapshot(rawDisplays, emptyAreas);

    display.getAllDisplays().then((allDisps: display.Display[]): void => {
      const targetDisps: display.Display[] = allDisps.length > 0 ? allDisps : rawDisplays;
      const areas: Map<number, display.Rect> = new Map<number, display.Rect>();
      let remaining: number = targetDisps.length;
      if (remaining === 0) {
        return;
      }
      for (let i: number = 0; i < targetDisps.length; i++) {
        const cur: display.Display = targetDisps[i];
        cur.getAvailableArea().then((rect: display.Rect): void => {
          areas.set(cur.id, rect);
          remaining--;
          if (remaining === 0) {
            sendSnapshot(targetDisps, areas);
          }
        }).catch((): void => {
          remaining--;
          if (remaining === 0) {
            sendSnapshot(targetDisps, areas);
          }
        });
      }
    }).catch((): void => {});
  }

  private registerDisplayListeners(): void {
    if (this.displayListenersRegistered) {
      return;
    }
    this.displayListenersRegistered = true;
    const onDispEvent = (_id: number): void => {
      this.refreshEnvAndDisplays();
    };
    try {
      display.on('add', onDispEvent);
      display.on('remove', onDispEvent);
      display.on('change', onDispEvent);
    } catch (_) {}
  }

  private unregisterDisplayListeners(): void {
    if (!this.displayListenersRegistered) {
      return;
    }
    this.displayListenersRegistered = false;
    try {
      display.off('add');
      display.off('remove');
      display.off('change');
    } catch (_) {}
  }

  private currentScale(): number {
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      return d.densityPixels > 0 ? d.densityPixels : 1.0;
    } catch (_) {
      return 1.0;
    }
  }

  private handleWinCreate(cfg: HostWinConfig): void {
    if (cfg.winId === 1) {
      let mw: ManagedWindow | undefined = this.windows.get(1);
      if (mw === undefined) {
        mw = new ManagedWindow();
        mw.winId = 1;
        this.windows.set(1, mw);
      }
      mw.cfg = cfg;
      if (mw.ready && mw.win !== undefined) {
        this.applyWindowConfig(mw, cfg, CFG_MASK_ALL, true);
      }
      return;
    }

    if (this.windowStage === undefined) {
      return;
    }
    const mw: ManagedWindow = new ManagedWindow();
    mw.winId = cfg.winId;
    mw.cfg = cfg;
    this.windows.set(cfg.winId, mw);

    const sc: number = this.currentScale();
    const frameless: boolean = (cfg.flags & WIN_FLAG_FRAMELESS) !== 0;
    const modal: boolean = (cfg.flags & WIN_FLAG_MODAL) !== 0;
    const opts: window.SubWindowOptions = {
      title: cfg.title.length > 0 ? cfg.title : 'Window',
      decorEnabled: !frameless,
      isModal: modal
    };
    if (cfg.w > 0 && cfg.h > 0) {
      opts.windowRect = {
        left: Math.round(cfg.x * sc),
        top: Math.round(cfg.y * sc),
        width: Math.max(100, Math.round(cfg.w * sc)),
        height: Math.max(80, Math.round(cfg.h * sc))
      };
    }

    const stage: window.WindowStage = this.windowStage;
    stage.createSubWindowWithOptions('mygo_win_' + cfg.winId.toString(), opts).then((subWin: window.Window): void => {
      if (!this.windows.has(cfg.winId)) {
        subWin.destroyWindow().catch((): void => {});
        return;
      }
      mw.win = subWin;
      try {
        mw.arkWindowId = subWin.getWindowProperties().id;
      } catch (_) {}
      this.attachWindowListeners(mw);

      const storage: LocalStorage = new LocalStorage();
      storage.setOrCreate<number>('winId', cfg.winId);
      storage.setOrCreate<number>('arkWindowId', mw.arkWindowId);

      subWin.loadContent('pages/Index', storage).then((): void => {
        if (!this.windows.has(cfg.winId)) {
          return;
        }
        mw.ready = true;
        if (mw.cfg !== undefined) {
          this.applyWindowConfig(mw, mw.cfg, CFG_MASK_ALL, true);
        }
        this.reportWindowRect(mw);
        this.flushPendingActions(mw);
      }).catch((err: Error): void => {
        hilog.error(DOMAIN, TAG, 'subWindow loadContent failed: %{public}s', err.message);
      });
    }).catch((err: Error): void => {
      hilog.error(DOMAIN, TAG, 'createSubWindowWithOptions failed: %{public}s', err.message);
      this.windows.delete(cfg.winId);
    });
  }

  private mergeConfig(prev: HostWinConfig | undefined, next: HostWinConfig, mask: number): HostWinConfig {
    if (prev === undefined) {
      return next;
    }
    const merged: HostWinConfig = {
      winId: next.winId,
      parentOrMask: prev.parentOrMask,
      x: (mask & CFG_MASK_BOUNDS) !== 0 ? next.x : prev.x,
      y: (mask & CFG_MASK_BOUNDS) !== 0 ? next.y : prev.y,
      w: (mask & CFG_MASK_BOUNDS) !== 0 ? next.w : prev.w,
      h: (mask & CFG_MASK_BOUNDS) !== 0 ? next.h : prev.h,
      minW: (mask & CFG_MASK_MIN_SIZE) !== 0 ? next.minW : prev.minW,
      minH: (mask & CFG_MASK_MIN_SIZE) !== 0 ? next.minH : prev.minH,
      maxW: (mask & CFG_MASK_MAX_SIZE) !== 0 ? next.maxW : prev.maxW,
      maxH: (mask & CFG_MASK_MAX_SIZE) !== 0 ? next.maxH : prev.maxH,
      flags: (mask & CFG_MASK_FLAGS) !== 0 ? next.flags : prev.flags,
      titleBarHeight: next.titleBarHeight > 0 ? next.titleBarHeight : prev.titleBarHeight,
      bgRGBA: (mask & CFG_MASK_BG) !== 0 ? next.bgRGBA : prev.bgRGBA,
      title: (mask & CFG_MASK_TITLE) !== 0 ? next.title : prev.title
    };
    return merged;
  }

  private handleWinConfig(cfg: HostWinConfig): void {
    const mw: ManagedWindow | undefined = this.windows.get(cfg.winId);
    if (mw === undefined) {
      return;
    }
    const mask: number = cfg.parentOrMask;
    mw.cfg = this.mergeConfig(mw.cfg, cfg, mask);
    if (mw.ready && mw.win !== undefined) {
      this.applyWindowConfig(mw, mw.cfg, mask, false);
    }
  }

  private applyWindowConfig(mw: ManagedWindow, cfg: HostWinConfig, mask: number, isCreate: boolean): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    const sc: number = this.currentScale();

    if ((mask & CFG_MASK_TITLE) !== 0 && cfg.title.length > 0) {
      try {
        w.setWindowTitle(cfg.title).catch((): void => {});
      } catch (_) {}
    }

    if ((mask & CFG_MASK_BG) !== 0 && (cfg.flags & WIN_FLAG_HAS_BG) !== 0) {
      try {
        w.setWindowBackgroundColor(rgbaToHexColor(cfg.bgRGBA));
      } catch (_) {}
    }

    if ((mask & (CFG_MASK_MIN_SIZE | CFG_MASK_MAX_SIZE | CFG_MASK_FLAGS)) !== 0) {
      const resizable: boolean = (cfg.flags & WIN_FLAG_RESIZABLE) !== 0;
      const limits: window.WindowLimits = {
        pixelUnit: window.PixelUnit.VP
      };
      if (!resizable && cfg.w > 0 && cfg.h > 0) {
        limits.minWidth = cfg.w;
        limits.maxWidth = cfg.w;
        limits.minHeight = cfg.h;
        limits.maxHeight = cfg.h;
      } else {
        if (cfg.minW > 0) {
          limits.minWidth = cfg.minW;
        }
        if (cfg.minH > 0) {
          limits.minHeight = cfg.minH;
        }
        if (cfg.maxW > 0) {
          limits.maxWidth = cfg.maxW;
        }
        if (cfg.maxH > 0) {
          limits.maxHeight = cfg.maxH;
        }
      }
      if (limits.minWidth !== undefined || limits.minHeight !== undefined ||
          limits.maxWidth !== undefined || limits.maxHeight !== undefined) {
        try {
          w.setWindowLimits(limits).catch((): void => {});
        } catch (_) {}
      }
    }

    if ((mask & CFG_MASK_BOUNDS) !== 0 && (!isCreate || mw.winId !== 1)) {
      if (cfg.w > 0 && cfg.h > 0 && !isCreate) {
        try {
          w.resize(Math.max(100, Math.round(cfg.w * sc)), Math.max(80, Math.round(cfg.h * sc))).catch((): void => {});
        } catch (_) {}
      }
      if ((cfg.flags & WIN_FLAG_CENTER) !== 0) {
        this.centerWindow(mw);
      } else if (!isCreate || cfg.x !== 0 || cfg.y !== 0) {
        try {
          w.moveWindowTo(Math.round(cfg.x * sc), Math.round(cfg.y * sc)).catch((): void => {});
        } catch (_) {}
      }
    } else if (isCreate && mw.winId === 1 && (cfg.flags & WIN_FLAG_CENTER) !== 0) {
      this.centerWindow(mw);
    }

    if ((mask & CFG_MASK_FLAGS) !== 0) {
      const hiddenTB: boolean = (cfg.flags & WIN_FLAG_HIDDEN_TITLEBAR) !== 0;
      const frameless: boolean = (cfg.flags & WIN_FLAG_FRAMELESS) !== 0;
      const hideBtns: boolean = (cfg.flags & WIN_FLAG_HIDE_WIN_BUTTONS) !== 0;
      try {
        w.setWindowDecorVisible(!hiddenTB && !frameless);
        if (hiddenTB && cfg.titleBarHeight > 0) {
          w.setWindowDecorHeight(cfg.titleBarHeight);
        }
      } catch (_) {}

      try {
        if (hideBtns || frameless) {
          w.setWindowTitleButtonVisible(false, false, false);
        } else {
          w.setWindowTitleButtonVisible(
            (cfg.flags & WIN_FLAG_MAXIMIZABLE) !== 0,
            (cfg.flags & WIN_FLAG_MINIMIZABLE) !== 0,
            (cfg.flags & WIN_FLAG_CLOSABLE) !== 0
          );
        }
      } catch (_) {}

      if (hiddenTB && !hideBtns && !frameless) {
        this.reportTitleButtonRect(mw);
      }

      if (mw.winId >= 2 && (cfg.flags & WIN_FLAG_MODAL) === 0) {
        const zLevel: number = (cfg.flags & WIN_FLAG_ALWAYS_ON_TOP) !== 0 ? 100 : 0;
        try {
          w.setSubWindowZLevel(zLevel).catch((): void => {});
        } catch (_) {}
      }

      if (isCreate) {
        if ((cfg.flags & WIN_FLAG_FULLSCREEN) !== 0) {
          try {
            w.maximize(window.MaximizePresentation.ENTER_IMMERSIVE).catch((): void => {});
          } catch (_) {}
        } else if ((cfg.flags & WIN_FLAG_MAXIMIZED) !== 0) {
          try {
            w.maximize(window.MaximizePresentation.EXIT_IMMERSIVE).catch((): void => {});
          } catch (_) {}
        }
      }
    }
  }

  private centerWindow(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      const props: window.WindowProperties = w.getWindowProperties();
      const winW: number = props.windowRect.width > 0 ? props.windowRect.width : Math.round((mw.cfg?.w ?? 960) * this.currentScale());
      const winH: number = props.windowRect.height > 0 ? props.windowRect.height : Math.round((mw.cfg?.h ?? 640) * this.currentScale());
      const targetX: number = Math.max(0, Math.round((d.width - winW) / 2));
      const targetY: number = Math.max(0, Math.round((d.height - winH) / 2));
      w.moveWindowTo(targetX, targetY).catch((): void => {});
    } catch (_) {}
  }

  private handleWinAction(winId: number, action: number): void {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw === undefined) {
      return;
    }
    if (action === WIN_ACT_CLOSE_ALLOW || action === WIN_ACT_CLOSE_DENY) {
      const resolve: ((prevent: boolean) => void) | undefined = mw.willCloseResolve;
      mw.willCloseResolve = undefined;
      if (action === WIN_ACT_CLOSE_ALLOW) {
        mw.closingFromEngine = true;
        if (resolve !== undefined) {
          resolve(false);
        }
        if (winId === 1) {
          this.handleEngineQuit();
        } else {
          this.cleanupClosedSubWindow(winId);
        }
      } else {
        if (resolve !== undefined) {
          resolve(true);
        }
      }
      return;
    }

    if (!mw.ready || mw.win === undefined) {
      mw.pendingActions.push(action);
      return;
    }
    this.executeWindowAction(mw, action);
  }

  private flushPendingActions(mw: ManagedWindow): void {
    if (mw.pendingActions.length === 0) {
      return;
    }
    const actions: number[] = mw.pendingActions.slice();
    mw.pendingActions = [];
    for (let i: number = 0; i < actions.length; i++) {
      this.executeWindowAction(mw, actions[i]);
    }
  }

  private executeWindowAction(mw: ManagedWindow, action: number): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    switch (action) {
      case WIN_ACT_SHOW:
      case WIN_ACT_SHOW_INACTIVE:
        try {
          w.showWindow().then((): void => {
            this.reportWindowRect(mw);
            if (action === WIN_ACT_SHOW) {
              this.focusManagedWindow(mw);
            }
          }).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_HIDE:
        if (mw.winId >= 2) {
          try {
            w.minimize().catch((): void => {});
          } catch (_) {}
        }
        break;
      case WIN_ACT_CLOSE:
        mw.closingFromEngine = true;
        if (mw.winId === 1) {
          this.handleEngineQuit();
        } else {
          this.detachWindowListeners(mw);
          this.windows.delete(mw.winId);
          this.popupOpeners.delete(mw.winId);
          this.msgBoxOpeners.delete(mw.winId);
          try {
            w.destroyWindow().catch((): void => {});
          } catch (_) {}
        }
        break;
      case WIN_ACT_FOCUS:
        this.focusManagedWindow(mw);
        break;
      case WIN_ACT_BLUR:
        if (mw.winId >= 2) {
          const mainMw: ManagedWindow | undefined = this.windows.get(1);
          if (mainMw !== undefined) {
            this.focusManagedWindow(mainMw);
          }
        }
        break;
      case WIN_ACT_MINIMIZE:
        try {
          w.minimize().catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_MAXIMIZE:
        try {
          w.maximize(window.MaximizePresentation.EXIT_IMMERSIVE).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_UNMAXIMIZE:
      case WIN_ACT_RESTORE:
      case WIN_ACT_EXIT_FULLSCREEN:
        try {
          w.recover().catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_ENTER_FULLSCREEN:
        try {
          w.maximize(window.MaximizePresentation.ENTER_IMMERSIVE).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_CENTER:
        this.centerWindow(mw);
        break;
      case WIN_ACT_START_DRAG:
        try {
          w.startMoving().catch((): void => {});
        } catch (_) {}
        break;
      default:
        break;
    }
  }

  private focusManagedWindow(target: ManagedWindow): void {
    if (target.arkWindowId <= 0) {
      return;
    }
    let activeId: number = 0;
    this.windows.forEach((cand: ManagedWindow): void => {
      if (cand.active && cand.arkWindowId > 0) {
        activeId = cand.arkWindowId;
      }
    });
    if (activeId > 0 && activeId !== target.arkWindowId) {
      try {
        window.shiftAppWindowFocus(activeId, target.arkWindowId).catch((): void => {});
      } catch (_) {}
    }
  }

  private attachWindowListeners(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    const winId: number = mw.winId;

    try {
      w.on('windowEvent', (evType: window.WindowEventType): void => {
        switch (evType) {
          case window.WindowEventType.WINDOW_SHOWN:
            host.notifyWindowEvent(winId, WIN_OP_SHOW);
            break;
          case window.WindowEventType.WINDOW_ACTIVE:
            mw.active = true;
            host.notifyWindowEvent(winId, WIN_OP_FOCUS);
            break;
          case window.WindowEventType.WINDOW_INACTIVE:
            mw.active = false;
            host.notifyWindowEvent(winId, WIN_OP_BLUR);
            break;
          case window.WindowEventType.WINDOW_HIDDEN:
            mw.active = false;
            host.notifyWindowEvent(winId, WIN_OP_HIDE);
            break;
          default:
            break;
        }
      });
    } catch (_) {}

    try {
      w.on('windowStatusChange', (status: window.WindowStatusType): void => {
        const prev: window.WindowStatusType = mw.lastStatus;
        mw.lastStatus = status;
        if (status === window.WindowStatusType.MINIMIZE) {
          host.notifyWindowEvent(winId, WIN_OP_MINIMIZE);
        } else if (status === window.WindowStatusType.MAXIMIZE) {
          host.notifyWindowEvent(winId, WIN_OP_MAXIMIZE);
        } else if (status === window.WindowStatusType.FULL_SCREEN) {
          host.notifyWindowEvent(winId, WIN_OP_ENTER_FULLSCREEN);
        } else if (status === window.WindowStatusType.FLOATING) {
          if (prev === window.WindowStatusType.MINIMIZE) {
            host.notifyWindowEvent(winId, WIN_OP_RESTORE);
          } else if (prev === window.WindowStatusType.FULL_SCREEN) {
            host.notifyWindowEvent(winId, WIN_OP_LEAVE_FULLSCREEN);
          } else if (prev === window.WindowStatusType.MAXIMIZE) {
            host.notifyWindowEvent(winId, WIN_OP_UNMAXIMIZE);
          }
        }
      });
    } catch (_) {}

    try {
      w.on('windowRectChange', (opts: window.RectChangeOptions): void => {
        const sc: number = this.currentScale();
        const xDip: number = Math.round(opts.rect.left / sc);
        const yDip: number = Math.round(opts.rect.top / sc);
        const wDip: number = Math.round(opts.rect.width / sc);
        const hDip: number = Math.round(opts.rect.height / sc);
        host.notifyWindowRect(winId, xDip, yDip, wDip, hDip);
      });
    } catch (_) {}

    try {
      w.on('windowTitleButtonRectChange', (rect: window.TitleButtonRect): void => {
        const hDip: number = Math.max(0, Math.round(rect.top + rect.height));
        const rDip: number = Math.max(0, Math.round(rect.right + rect.width));
        host.notifyTitleBar(winId, hDip, 0, rDip);
      });
    } catch (_) {}

    try {
      w.on('windowWillClose', (): Promise<boolean> => {
        if (mw.closingFromEngine) {
          return Promise.resolve(false);
        }
        return new Promise<boolean>((resolve: (prevent: boolean) => void): void => {
          mw.willCloseResolve = resolve;
          host.notifyWindowEvent(winId, WIN_OP_CLOSE_REQUEST);
        });
      });
    } catch (_) {}
  }

  private detachWindowListeners(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    try { w.off('windowEvent'); } catch (_) {}
    try { w.off('windowStatusChange'); } catch (_) {}
    try { w.off('windowRectChange'); } catch (_) {}
    try { w.off('windowTitleButtonRectChange'); } catch (_) {}
    try { w.off('windowWillClose'); } catch (_) {}
  }

  private cleanupClosedSubWindow(winId: number): void {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw === undefined) {
      return;
    }
    this.detachWindowListeners(mw);
    this.windows.delete(winId);
    this.popupOpeners.delete(winId);
    this.msgBoxOpeners.delete(winId);
    if (mw.win !== undefined) {
      try {
        mw.win.destroyWindow().catch((): void => {});
      } catch (_) {}
    }
  }

  private reportWindowRect(mw: ManagedWindow): void {
    if (mw.win === undefined) {
      return;
    }
    try {
      const r: window.Rect = mw.win.getWindowProperties().windowRect;
      const sc: number = this.currentScale();
      host.notifyWindowRect(
        mw.winId,
        Math.round(r.left / sc),
        Math.round(r.top / sc),
        Math.round(r.width / sc),
        Math.round(r.height / sc)
      );
    } catch (_) {}
  }

  private reportTitleButtonRect(mw: ManagedWindow): void {
    if (mw.win === undefined) {
      return;
    }
    try {
      const rect: window.TitleButtonRect = mw.win.getTitleButtonRect();
      if (rect.width > 0 && rect.height > 0) {
        const hDip: number = Math.max(0, Math.round(rect.top + rect.height));
        const rDip: number = Math.max(0, Math.round(rect.right + rect.width));
        host.notifyTitleBar(mw.winId, hDip, 0, rDip);
      }
    } catch (_) {}
  }

  private handlePopupMenu(reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void {
    const opener: PopupMenuOpener | undefined = this.popupOpeners.get(winId) ?? this.popupOpeners.get(1);
    if (opener === undefined) {
      host.resolvePopupMenu(reqId, 0);
      return;
    }
    opener(reqId, x, y, rtl, items);
  }

  private handleEngineQuit(): void {
    if (this.context !== undefined) {
      try {
        this.context.terminateSelf().catch((): void => {});
      } catch (_) {}
    }
  }

  private handleEngineRestart(): void {
    this.npPlaying = false;
    this.powerKeepDisplay = false;
    this.powerKeepRunning = false;
    this.applyKeepScreenOn(false);
    this.syncBackgroundRunning();
    const staleIds: number[] = [];
    this.windows.forEach((_mw: ManagedWindow, id: number): void => {
      if (id >= 2) {
        staleIds.push(id);
      }
    });
    for (let i: number = 0; i < staleIds.length; i++) {
      this.cleanupClosedSubWindow(staleIds[i]);
    }
  }

  // ---------- AVSession, Notifications, Power & Network ----------

  private async buildLaunchWantAgent(extraParams?: Record<string, Object>): Promise<WantAgent | undefined> {
    if (this.context === undefined) {
      return undefined;
    }
    const wantInfo: wantAgent.WantAgentInfo = {
      wants: [
        {
          bundleName: this.bundleName,
          abilityName: 'EntryAbility',
          parameters: extraParams
        }
      ],
      actionType: wantAgent.OperationType.START_ABILITY,
      requestCode: 0,
      actionFlags: [wantAgent.WantAgentFlags.UPDATE_PRESENT_FLAG]
    };
    try {
      return await wantAgent.getWantAgent(wantInfo);
    } catch (_) {
      return undefined;
    }
  }

  private handleNowPlaying(np: HostNowPlaying): void {
    this.avChain = this.avChain.then((): Promise<void> => {
      return this.applyNowPlayingAsync(np);
    }).catch((err: BusinessError): void => {
      hilog.error(DOMAIN, TAG, 'applyNowPlayingAsync failed: %{public}s', JSON.stringify(err));
    });
  }

  private clearAVSessionListeners(session: avSession.AVSession): void {
    if ((this.registeredMediaCmds & (MEDIA_CMD_PLAY | MEDIA_CMD_TOGGLE)) !== 0) {
      try { session.off('play'); } catch (_) {}
    }
    if ((this.registeredMediaCmds & (MEDIA_CMD_PAUSE | MEDIA_CMD_TOGGLE)) !== 0) {
      try { session.off('pause'); } catch (_) {}
    }
    if ((this.registeredMediaCmds & MEDIA_CMD_STOP) !== 0) {
      try { session.off('stop'); } catch (_) {}
    }
    if ((this.registeredMediaCmds & MEDIA_CMD_NEXT) !== 0) {
      try { session.off('playNext'); } catch (_) {}
    }
    if ((this.registeredMediaCmds & MEDIA_CMD_PREV) !== 0) {
      try { session.off('playPrevious'); } catch (_) {}
    }
    if ((this.registeredMediaCmds & MEDIA_CMD_SEEK) !== 0) {
      try { session.off('seek'); } catch (_) {}
    }
    this.registeredMediaCmds = 0;
  }

  private syncAVSessionListeners(session: avSession.AVSession, cmds: number): void {
    if (cmds === this.registeredMediaCmds) {
      return;
    }
    this.clearAVSessionListeners(session);

    if ((cmds & (MEDIA_CMD_PLAY | MEDIA_CMD_TOGGLE)) !== 0) {
      const sendCmd: number = (cmds & MEDIA_CMD_PLAY) !== 0 ? MEDIA_CMD_PLAY : MEDIA_CMD_TOGGLE;
      try {
        session.on('play', (): void => {
          host.notifyMediaCmd(sendCmd, 0);
        });
      } catch (_) {}
    }
    if ((cmds & (MEDIA_CMD_PAUSE | MEDIA_CMD_TOGGLE)) !== 0) {
      const sendCmd: number = (cmds & MEDIA_CMD_PAUSE) !== 0 ? MEDIA_CMD_PAUSE : MEDIA_CMD_TOGGLE;
      try {
        session.on('pause', (): void => {
          host.notifyMediaCmd(sendCmd, 0);
        });
      } catch (_) {}
    }
    if ((cmds & MEDIA_CMD_STOP) !== 0) {
      try {
        session.on('stop', (): void => {
          host.notifyMediaCmd(MEDIA_CMD_STOP, 0);
        });
      } catch (_) {}
    }
    if ((cmds & MEDIA_CMD_NEXT) !== 0) {
      try {
        session.on('playNext', (): void => {
          host.notifyMediaCmd(MEDIA_CMD_NEXT, 0);
        });
      } catch (_) {}
    }
    if ((cmds & MEDIA_CMD_PREV) !== 0) {
      try {
        session.on('playPrevious', (): void => {
          host.notifyMediaCmd(MEDIA_CMD_PREV, 0);
        });
      } catch (_) {}
    }
    if ((cmds & MEDIA_CMD_SEEK) !== 0) {
      try {
        session.on('seek', (timeMs: number): void => {
          host.notifyMediaCmd(MEDIA_CMD_SEEK, timeMs);
        });
      } catch (_) {}
    }
    this.registeredMediaCmds = cmds;
  }

  private async applyNowPlayingAsync(np: HostNowPlaying): Promise<void> {
    if (this.context === undefined) {
      return;
    }
    if (!np.active) {
      this.npPlaying = false;
      this.syncBackgroundRunning();
      if (this.mediaSession !== undefined) {
        this.clearAVSessionListeners(this.mediaSession);
        try {
          await this.mediaSession.setAVPlaybackState({
            state: avSession.PlaybackState.PLAYBACK_STATE_STOP
          });
        } catch (_) {}
        try { await this.mediaSession.deactivate(); } catch (_) {}
      }
      if (this.mediaCoverPm !== undefined) {
        try { await this.mediaCoverPm.release(); } catch (_) {}
        this.mediaCoverPm = undefined;
      }
      this.mediaMetaCache = undefined;
      return;
    }

    let createdSession: boolean = false;
    if (this.mediaSession === undefined) {
      this.mediaSession = await avSession.createAVSession(this.context, 'MyGoAudio', 'audio');
      createdSession = true;
      const agent: WantAgent | undefined = await this.buildLaunchWantAgent();
      if (agent !== undefined) {
        try { await this.mediaSession.setLaunchAbility(agent); } catch (_) {}
      }
    }

    if (np.metaChanged || (createdSession && this.mediaMetaCache !== undefined)) {
      if (np.metaChanged) {
        let newPm: image.PixelMap | undefined = undefined;
        if (np.artwork.byteLength > 0) {
          let imgSrc: image.ImageSource | undefined = undefined;
          try {
            imgSrc = image.createImageSource(np.artwork);
            newPm = await imgSrc.createPixelMap();
          } catch (_) {
          } finally {
            if (imgSrc !== undefined) {
              try { await imgSrc.release(); } catch (_) {}
            }
          }
        }
        if (this.mediaCoverPm !== undefined) {
          try { await this.mediaCoverPm.release(); } catch (_) {}
        }
        this.mediaCoverPm = newPm;

        const meta: avSession.AVMetadata = {
          assetId: 'mygo-current',
          title: np.title.length > 0 ? np.title : this.bundleLabel
        };
        if (np.artist.length > 0) {
          meta.artist = np.artist;
        }
        if (np.album.length > 0) {
          meta.album = np.album;
        }
        if (np.durationMs > 0) {
          meta.duration = Math.round(np.durationMs);
        }
        if (newPm !== undefined) {
          meta.mediaImage = newPm;
        }
        this.mediaMetaCache = meta;
      } else if (this.mediaMetaCache !== undefined && np.durationMs > 0) {
        this.mediaMetaCache.duration = Math.round(np.durationMs);
      }
      if (this.mediaMetaCache !== undefined) {
        try {
          await this.mediaSession.setAVMetadata(this.mediaMetaCache);
        } catch (e) {
          hilog.error(DOMAIN, TAG, 'setAVMetadata failed: %{public}s', JSON.stringify(e));
        }
      }
    }

    this.syncAVSessionListeners(this.mediaSession, np.commands);

    let pbStateEnum: avSession.PlaybackState = avSession.PlaybackState.PLAYBACK_STATE_STOP;
    if (np.state === 1) {
      pbStateEnum = avSession.PlaybackState.PLAYBACK_STATE_PLAY;
    } else if (np.state === 2) {
      pbStateEnum = avSession.PlaybackState.PLAYBACK_STATE_PAUSE;
    }
    const pb: avSession.AVPlaybackState = {
      state: pbStateEnum,
      speed: np.rate > 0 ? np.rate : 1.0,
      position: {
        elapsedTime: Math.max(0, Math.round(np.positionMs)),
        updateTime: Date.now()
      }
    };
    if (np.durationMs > 0) {
      pb.duration = Math.round(np.durationMs);
    }
    try {
      await this.mediaSession.setAVPlaybackState(pb);
    } catch (e) {
      hilog.error(DOMAIN, TAG, 'setAVPlaybackState failed: %{public}s', JSON.stringify(e));
    }
    try {
      await this.mediaSession.activate();
    } catch (_) {}

    this.npPlaying = np.state === 1;
    this.syncBackgroundRunning();
  }

  private teardownAVSessionAndBackground(): void {
    this.npPlaying = false;
    this.powerKeepDisplay = false;
    this.powerKeepRunning = false;
    this.syncBackgroundRunning();
    const sess: avSession.AVSession | undefined = this.mediaSession;
    this.mediaSession = undefined;
    if (sess !== undefined) {
      this.clearAVSessionListeners(sess);
      sess.destroy().catch((): void => {});
    }
    const pm: image.PixelMap | undefined = this.mediaCoverPm;
    this.mediaCoverPm = undefined;
    this.mediaMetaCache = undefined;
    if (pm !== undefined) {
      pm.release().catch((): void => {});
    }
  }

  private handleNotifShow(notif: HostNotifShow): void {
    this.publishNotificationAsync(notif).then((denied: boolean): void => {
      host.resolveNotif(notif.reqId, denied, '');
    }).catch((err: BusinessError): void => {
      const code: number = err?.code ?? 0;
      if (code === 1600004 || code === 1600020 || code === 201) {
        host.resolveNotif(notif.reqId, true, '');
        return;
      }
      const msg: string = err?.message ?? 'notification publish failed';
      host.resolveNotif(notif.reqId, false, msg);
    });
  }

  private async ensureNotificationEnabled(): Promise<boolean> {
    try {
      if (notificationManager.isNotificationEnabledSync()) {
        return true;
      }
    } catch (_) {}
    if (!this.notifPermAsked && this.context !== undefined) {
      this.notifPermAsked = true;
      try {
        await notificationManager.requestEnableNotification(this.context);
      } catch (_) {}
      try {
        return notificationManager.isNotificationEnabledSync();
      } catch (_) {}
    }
    return false;
  }

  private async publishNotificationAsync(notif: HostNotifShow): Promise<boolean> {
    const enabled: boolean = await this.ensureNotificationEnabled();
    if (!enabled) {
      return true;
    }

    const titleText: string = notif.title.trim().length > 0 ? notif.title.trim() : this.bundleLabel;
    const subText: string = notif.subtitle.trim();
    const bodyRaw: string = notif.body.trim();
    let combinedBody: string = bodyRaw;
    if (subText.length > 0 && bodyRaw.length > 0) {
      combinedBody = subText + '\n' + bodyRaw;
    } else if (subText.length > 0) {
      combinedBody = subText;
    }
    if (combinedBody.length === 0) {
      combinedBody = ' ';
    }

    let picPm: image.PixelMap | undefined = undefined;
    if (notif.image.byteLength > 0) {
      let imgSrc: image.ImageSource | undefined = undefined;
      try {
        imgSrc = image.createImageSource(notif.image);
        const pm: image.PixelMap = await imgSrc.createPixelMap();
        if (pm.getPixelBytesNumber() <= 2 * 1024 * 1024) {
          picPm = pm;
        } else {
          await pm.release();
        }
      } catch (_) {
      } finally {
        if (imgSrc !== undefined) {
          try { await imgSrc.release(); } catch (_) {}
        }
      }
    }

    let content: notificationManager.NotificationContent;
    if (picPm !== undefined) {
      content = {
        notificationContentType: notificationManager.ContentType.NOTIFICATION_CONTENT_PICTURE,
        picture: {
          title: titleText,
          text: combinedBody,
          briefText: subText.length > 0 ? subText : titleText,
          expandedTitle: titleText,
          picture: picPm
        }
      };
    } else {
      content = {
        notificationContentType: notificationManager.ContentType.NOTIFICATION_CONTENT_BASIC_TEXT,
        normal: {
          title: titleText,
          text: combinedBody,
          additionalText: subText.length > 0 ? subText : undefined
        }
      };
    }

    const numId: number = notif.id.length > 0 ? hashNotifId(notif.id) : 1;
    const req: notificationManager.NotificationRequest = {
      id: numId,
      content: content,
      notificationSlotType: notif.silent
        ? notificationManager.SlotType.CONTENT_INFORMATION
        : notificationManager.SlotType.SOCIAL_COMMUNICATION,
      tapDismissed: true
    };
    if (notif.id.length > 0) {
      req.label = notif.id.substring(0, 200);
      const params: Record<string, Object> = {};
      params[NOTIF_PARAM_KEY] = notif.id;
      const agent: WantAgent | undefined = await this.buildLaunchWantAgent(params);
      if (agent !== undefined) {
        req.wantAgent = agent;
      }
    }
    if (notif.group.length > 0) {
      req.groupName = notif.group.substring(0, 200);
    }

    try {
      await notificationManager.publish(req);
    } finally {
      if (picPm !== undefined) {
        try { await picPm.release(); } catch (_) {}
      }
    }
    return false;
  }

  private handleNotifRemove(all: boolean, id: string): void {
    if (all) {
      notificationManager.cancelAll().catch((): void => {});
      return;
    }
    if (id.length > 0) {
      notificationManager.cancel(hashNotifId(id), id.substring(0, 200)).catch((): void => {});
    }
  }

  private isCurrentlyOnBattery(): boolean {
    try {
      return batteryInfo.pluggedType === batteryInfo.BatteryPluggedType.NONE;
    } catch (_) {
      return false;
    }
  }

  private registerPowerEvents(): void {
    host.notifyPowerState(this.isCurrentlyOnBattery(), '');
    if (this.powerSubscriber !== undefined) {
      return;
    }
    const subInfo: commonEventManager.CommonEventSubscribeInfo = {
      events: [
        commonEventManager.Support.COMMON_EVENT_SCREEN_LOCKED,
        commonEventManager.Support.COMMON_EVENT_SCREEN_UNLOCKED,
        commonEventManager.Support.COMMON_EVENT_SCREEN_OFF,
        commonEventManager.Support.COMMON_EVENT_SCREEN_ON,
        commonEventManager.Support.COMMON_EVENT_CHARGING,
        commonEventManager.Support.COMMON_EVENT_DISCHARGING,
        commonEventManager.Support.COMMON_EVENT_POWER_CONNECTED,
        commonEventManager.Support.COMMON_EVENT_POWER_DISCONNECTED
      ]
    };
    commonEventManager.createSubscriber(subInfo).then((sub: commonEventManager.CommonEventSubscriber): void => {
      this.powerSubscriber = sub;
      commonEventManager.subscribe(sub, (err: BusinessError, data: commonEventManager.CommonEventData): void => {
        if (err) {
          return;
        }
        const onBat: boolean = this.isCurrentlyOnBattery();
        let evName: string = '';
        if (data.event === commonEventManager.Support.COMMON_EVENT_SCREEN_LOCKED) {
          evName = 'lock-screen';
        } else if (data.event === commonEventManager.Support.COMMON_EVENT_SCREEN_UNLOCKED) {
          evName = 'unlock-screen';
        } else if (data.event === commonEventManager.Support.COMMON_EVENT_SCREEN_OFF) {
          evName = 'suspend';
        } else if (data.event === commonEventManager.Support.COMMON_EVENT_SCREEN_ON) {
          evName = 'resume';
        }
        host.notifyPowerState(onBat, evName);
      });
    }).catch((): void => {});
  }

  private unregisterPowerEvents(): void {
    const sub: commonEventManager.CommonEventSubscriber | undefined = this.powerSubscriber;
    this.powerSubscriber = undefined;
    if (sub !== undefined) {
      try {
        commonEventManager.unsubscribe(sub, (): void => {});
      } catch (_) {}
    }
  }

  private applyKeepScreenOn(keepOn: boolean): void {
    this.windows.forEach((mw: ManagedWindow): void => {
      if (mw.win !== undefined) {
        try {
          mw.win.setWindowKeepScreenOn(keepOn).catch((): void => {});
        } catch (_) {}
      }
    });
  }

  private handlePowerReq(req: HostPowerReq): void {
    this.powerKeepDisplay = req.keepDisplay;
    this.powerKeepRunning = req.keepRunning || req.keepSystem;
    this.applyKeepScreenOn(this.powerKeepDisplay);
    this.syncBackgroundRunning();
  }

  private syncBackgroundRunning(): void {
    const wantBg: boolean = this.npPlaying || this.powerKeepRunning;
    this.bgChain = this.bgChain.then(async (): Promise<void> => {
      if (this.context === undefined || wantBg === this.bgRunningActive) {
        return;
      }
      if (wantBg) {
        const agent: WantAgent | undefined = await this.buildLaunchWantAgent();
        if (agent === undefined) {
          return;
        }
        try {
          await backgroundTaskManager.startBackgroundRunning(
            this.context,
            backgroundTaskManager.BackgroundMode.AUDIO_PLAYBACK,
            agent
          );
          this.bgRunningActive = true;
        } catch (_) {}
      } else {
        try {
          await backgroundTaskManager.stopBackgroundRunning(this.context);
        } catch (_) {}
        this.bgRunningActive = false;
      }
    }).catch((): void => {});
  }

  private queryAndNotifyNetState(): void {
    let online: boolean = false;
    let metered: boolean = false;
    try {
      online = connection.hasDefaultNetSync();
      if (online) {
        const netHandle: connection.NetHandle = connection.getDefaultNetSync();
        const caps: connection.NetCapabilities = connection.getNetCapabilitiesSync(netHandle);
        let notMetered: boolean = false;
        if (caps.networkCap !== undefined) {
          for (let i: number = 0; i < caps.networkCap.length; i++) {
            if (caps.networkCap[i] === connection.NetCap.NET_CAPABILITY_NOT_METERED) {
              notMetered = true;
              break;
            }
          }
        }
        metered = !notMetered;
      }
    } catch (_) {
      online = true;
      metered = false;
    }
    host.notifyNetState(online, metered);
  }

  private handleNetWatch(): void {
    this.queryAndNotifyNetState();
    if (this.netWatching) {
      return;
    }
    this.netWatching = true;
    try {
      const conn: connection.NetConnection = connection.createNetConnection();
      this.netConn = conn;
      conn.register((): void => {});
      conn.on('netAvailable', (): void => {
        this.queryAndNotifyNetState();
      });
      conn.on('netCapabilitiesChange', (): void => {
        this.queryAndNotifyNetState();
      });
      conn.on('netLost', (): void => {
        this.queryAndNotifyNetState();
      });
      conn.on('netUnavailable', (): void => {
        this.queryAndNotifyNetState();
      });
    } catch (_) {}
  }

  private unregisterNetWatch(): void {
    this.netWatching = false;
    const conn: connection.NetConnection | undefined = this.netConn;
    this.netConn = undefined;
    if (conn !== undefined) {
      try {
        conn.unregister((): void => {});
      } catch (_) {}
    }
  }
}

export const hostBridge: HostBridgeManager = new HostBridgeManager();
